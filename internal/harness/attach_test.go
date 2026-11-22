package harness

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/iodesystems/dun/internal/mcpserver"
	"github.com/iodesystems/dun/internal/project"
)

func TestQualifyAttach(t *testing.T) {
	for in, want := range map[string]string{
		"docs":         "own:docs",
		" Docs ":       "own:docs",
		"other:handbk": "other:handbk",
		"OTHER:Handbk": "other:handbk",
		"own:my_idx-2": "own:my_idx-2",
		":code":        ":code", // a global index (raglit 359f049)
	} {
		if got, err := qualifyAttach("own", in); err != nil || got != want {
			t.Errorf("%q: %q %v, want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "a:b:c", "other:*", "x/y", "other:", ":"} {
		if got, err := qualifyAttach("own", bad); err == nil {
			t.Errorf("%q: accepted as %q", bad, got)
		}
	}
}

func TestUnionAttachAndSelector(t *testing.T) {
	u := mcpserver.UnionAttach([]string{"a:x", "b:y"}, nil, []string{"b:y", "own:z", "c:w"})
	if !slices.Equal(u, []string{"a:x", "b:y", "own:z", "c:w"}) {
		t.Errorf("union: %v", u)
	}
	if s := attachSelector("own", u, nil, nil); s != "own:*,a:x,b:y,c:w" {
		t.Errorf("selector: %q", s)
	}
	// This project's own indexes are read by "<own>:*" already: no selector,
	// so raglit's default (with the config's shared namespaces) stays.
	if s := attachSelector("own", []string{"own:z"}, nil, nil); s != "" {
		t.Errorf("own only: %q", s)
	}
	if s := attachSelector("own", nil, nil, nil); s != "" {
		t.Errorf("none: %q", s)
	}
	// One of its own switched off: its own named one by one, less that one
	// (USER 2026-10-28: /rag); before the list is known, all of its own.
	ownIdx := []string{"own:main", "own:memory", "own:skills"}
	if s := attachSelector("own", nil, []string{"own:memory"}, ownIdx); s != "own:main,own:skills" {
		t.Errorf("own off: %q", s)
	}
	if s := attachSelector("own", []string{"a:x"}, []string{"own:memory"}, ownIdx); s != "own:main,own:skills,a:x" {
		t.Errorf("own off, other on: %q", s)
	}
	if s := attachSelector("own", nil, []string{"own:memory"}, nil); s != "own:*" {
		t.Errorf("own off, list unknown: %q", s)
	}
	if s := attachSelector("own", nil, ownIdx, ownIdx); s != "own:-*" {
		t.Errorf("all off: %q", s)
	}
}

func TestArgValue(t *testing.T) {
	args := []string{"serve", "--project", "p1", "--index=i", "-home", "/h"}
	for name, want := range map[string]string{"project": "p1", "index": "i", "home": "/h", "daemon": ""} {
		if got := argValue(args, name); got != want {
			t.Errorf("%s: %q, want %q", name, got, want)
		}
	}
}

// Attached: the model's search and get calls that name no index read the
// attached ones with this project's, the proactive search does, and the
// search tool says so. Detached, everything is as before.
func TestAttachIndex_SearchGetAndSurfacing(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	var calls []string
	var mu sync.Mutex
	ts := fakeIndexMCP(t, &calls, &mu)
	ws := t.TempDir()
	h, err := Start(context.Background(), Config{Workspace: ws, Servers: []mcpserver.Server{{ID: mcpserver.ServerDocs, URL: ts.URL, Timeout: 10}}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	ctx := context.Background()
	if err := h.StartServer(ctx, mcpserver.ServerDocs); err != nil {
		t.Fatal(err)
	}
	own := mcpserver.RaglitProject(ws)
	n := 0
	q := func() string { n++; return strings.Repeat("q", n) } // the loop guard refuses repeats
	last := func(tool string) string {
		t.Helper()
		mu.Lock()
		defer mu.Unlock()
		for i := len(calls) - 1; i >= 0; i-- {
			if strings.HasPrefix(calls[i], tool+" ") {
				return calls[i]
			}
		}
		return ""
	}
	searchDesc := func() string {
		for _, d := range h.Session.Tools {
			if d.Function.Name == "docs_search" {
				return d.Function.Description
			}
		}
		return ""
	}

	callTool(t, h, "docs_search", map[string]any{"query": q()})
	if c := last("search"); strings.Contains(c, `"index"`) {
		t.Fatalf("nothing attached, no index: %s", c)
	}

	// A bare name is this project's: no selector, nothing changes.
	name, _, changed, err := h.AttachIndex(ctx, mcpserver.ServerDocs, "notes", mcpserver.ScopeSession, true)
	if err != nil || !changed || name != own+":notes" {
		t.Fatalf("attach notes: %q %v %v", name, changed, err)
	}
	callTool(t, h, "docs_search", map[string]any{"query": q()})
	if c := last("search"); strings.Contains(c, `"index"`) {
		t.Errorf("an own index is read already: %s", c)
	}

	if _, _, _, err := h.AttachIndex(ctx, mcpserver.ServerDocs, "Other:Handbook", mcpserver.ScopeSession, true); err != nil {
		t.Fatal(err)
	}
	sel := own + ":*,other:handbook"
	callTool(t, h, "docs_search", map[string]any{"query": q()})
	if c := last("search"); !strings.Contains(c, `"index":"`+sel+`"`) {
		t.Errorf("search: %s, want index %s", c, sel)
	}
	callTool(t, h, "docs_search", map[string]any{"query": q(), "index": "skills"})
	if c := last("search"); !strings.Contains(c, `"index":"skills"`) {
		t.Errorf("a named index is left alone: %s", c)
	}
	callTool(t, h, "get_document", map[string]any{"path": "/a.md"})
	if c := last("get_document"); !strings.Contains(c, `"index":"`+sel+`"`) {
		t.Errorf("get: %s", c)
	}
	if d := searchDesc(); !strings.Contains(d, "other:handbook") || !strings.HasPrefix(d, "Search.") {
		t.Errorf("description: %q", d)
	}
	surfaceOnce(t, h, "where is the handbook")
	if c := last("search"); !strings.Contains(c, `"index":"`+sel+`"`) {
		t.Errorf("proactive search: %s", c)
	}
	if _, ok := docsBodyFetcher(h.mgr, mcpserver.ServerDocs, "get_document", sel)(ctx, "/b.md", 10); ok {
		t.Error("the fake has no text")
	}
	if c := last("get_document"); !strings.Contains(c, `"path":"/b.md"`) || !strings.Contains(c, `"index":"`+sel+`"`) {
		t.Errorf("fetch: %s", c)
	}

	// Listed with its scope; the same attachment again changes nothing.
	at, _ := h.Attachments(mcpserver.ServerDocs)
	if len(at) != 2 || at[1].Index != "other:handbook" || !slices.Equal(at[1].Scopes, []string{mcpserver.ScopeSession}) {
		t.Errorf("listing: %+v", at)
	}
	if _, _, changed, _ := h.AttachIndex(ctx, mcpserver.ServerDocs, "other:handbook", mcpserver.ScopeSession, true); changed {
		t.Error("attached twice")
	}
	// Project scope adds a second place it is set; unsetting the session's
	// leaves the project's to decide (the nearest scope that says decides).
	_, path, changed, err := h.AttachIndex(ctx, mcpserver.ServerDocs, "other:handbook", mcpserver.ScopeProject, true)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || filepath.Base(path) != project.LocalServersFile {
		t.Errorf("project attach: %v %q", changed, path)
	}
	if _, _, changed, _ := h.SetIndex(ctx, mcpserver.ServerDocs, "other:handbook", mcpserver.ScopeSession, nil); !changed {
		t.Error("unset session")
	}
	callTool(t, h, "docs_search", map[string]any{"query": q()})
	if c := last("search"); !strings.Contains(c, `"index":"`+sel+`"`) {
		t.Errorf("still attached at project: %s", c)
	}
	// Off at the session wins over on at the project.
	if _, _, changed, _ := h.AttachIndex(ctx, mcpserver.ServerDocs, "other:handbook", mcpserver.ScopeSession, false); !changed {
		t.Error("off at session")
	}
	callTool(t, h, "docs_search", map[string]any{"query": q()})
	if c := last("search"); strings.Contains(c, `"index"`) {
		t.Errorf("off at the session: %s", c)
	}
	if _, _, changed, _ := h.SetIndex(ctx, mcpserver.ServerDocs, "other:handbook", mcpserver.ScopeSession, nil); !changed {
		t.Error("unset session again")
	}
	if _, _, changed, _ := h.SetIndex(ctx, mcpserver.ServerDocs, "other:handbook", mcpserver.ScopeProject, nil); !changed {
		t.Error("unset project")
	}
	callTool(t, h, "docs_search", map[string]any{"query": q()})
	if c := last("search"); strings.Contains(c, `"index"`) {
		t.Errorf("detached everywhere: %s", c)
	}
	if strings.Contains(searchDesc(), "Attached") {
		t.Errorf("description after detach: %q", searchDesc())
	}
	if _, _, _, err := h.AttachIndex(ctx, ShellNamespace("x"), "a", mcpserver.ScopeSession, true); err == nil {
		t.Error("no search role: refused")
	}
}

// /rag watch runs the index CLI: `<cli> watch <abs dir> <store flags>
// --index <name>`, the name defaulting to the directory's.
func TestWatchDir_RunsTheIndexCLI(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	bin := t.TempDir()
	argv := filepath.Join(bin, "argv")
	script := "#!/bin/sh\necho \"$@\" > " + argv + "\necho watching\n"
	if err := os.WriteFile(filepath.Join(bin, "raglit"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "My Notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	h, err := Start(context.Background(), Config{Workspace: ws, Servers: []mcpserver.Server{{ID: mcpserver.ServerDocs,
		Command: filepath.Join(bin, "raglit"), Args: []string{"serve", "--project", "p", "--index", "w", "--daemon", "http://x"}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	idx, out, err := h.WatchDir(context.Background(), mcpserver.ServerDocs, "My Notes", "")
	if err != nil || idx != "my-notes" || out != "watching" {
		t.Fatalf("watch: %q %q %v", idx, out, err)
	}
	b, _ := os.ReadFile(argv)
	want := "watch " + filepath.Join(ws, "My Notes") + " --index my-notes --project p --daemon http://x"
	if strings.TrimSpace(string(b)) != want {
		t.Errorf("argv: %q, want %q", strings.TrimSpace(string(b)), want)
	}
	if _, _, err := h.WatchDir(context.Background(), mcpserver.ServerDocs, "nope", "x"); err == nil {
		t.Error("a missing directory is refused")
	}
	// A name is spelled as raglit stores it, so the one reported attaches.
	if idx, _, err := h.WatchDir(context.Background(), mcpserver.ServerDocs, "My Notes", "Hand.Book"); err != nil || idx != "handbook" {
		t.Errorf("name: %q %v", idx, err)
	}
}

// One of this project's own indexes switched off: the model's search names
// the others one by one (USER 2026-10-28: /rag). The list is raglit's,
// cached; until it is known the search reads all of its own.
func TestSetIndex_OwnIndexOffNarrowsTheSearch(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	var calls []string
	var mu sync.Mutex
	ts := fakeIndexMCP(t, &calls, &mu)
	// The project is the workspace's directory name, and ownIdxCache is keyed
	// by it: t.TempDir() names are "001", "002", ... in every test, so a
	// bare TempDir shares its cache entry with every other test's workspace
	// of that number. A listing another test left running (the real raglit,
	// seconds under load) then lands on this entry after it is seeded below.
	// A name only this test uses keeps the entry its own.
	ws := filepath.Join(t.TempDir(), "ownidxoff")
	if err := os.Mkdir(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	h, err := Start(context.Background(), Config{Workspace: ws, Servers: []mcpserver.Server{{ID: mcpserver.ServerDocs, URL: ts.URL, Timeout: 10}}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	ctx := context.Background()
	if err := h.StartServer(ctx, mcpserver.ServerDocs); err != nil {
		t.Fatal(err)
	}
	own := mcpserver.RaglitProject(ws)
	old := listIndexes
	listIndexesMu.Lock()
	listIndexes = func(context.Context, string, string, []string) ([]raglitIndexRow, error) {
		return []raglitIndexRow{{Project: own, Index: "main"}, {Project: own, Index: "memory"}, {Project: "x", Index: "y"}}, nil
	}
	listIndexesMu.Unlock()
	t.Cleanup(func() {
		listIndexesMu.Lock()
		listIndexes = old
		listIndexesMu.Unlock()
		ownIdxCache.Delete(own)
	})
	noteOwnIndexes(own, []raglitIndexRow{{Project: own, Index: "main"}, {Project: own, Index: "memory"}})

	if _, _, changed, err := h.SetIndex(ctx, mcpserver.ServerDocs, "memory", mcpserver.ScopeSession, ptr(false)); err != nil || !changed {
		t.Fatalf("off: %v %v", changed, err)
	}
	callTool(t, h, "docs_search", map[string]any{"query": "zzz"})
	mu.Lock()
	last := calls[len(calls)-1]
	mu.Unlock()
	if !strings.Contains(last, `"index":"`+own+`:main"`) {
		t.Fatalf("own memory off: %s", last)
	}
}
