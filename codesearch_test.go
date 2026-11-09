package dun

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iodesystems/dun/proto"
)

// codeHarness is a root session over a fake dund in a temp workspace, with
// the notices it gave.
func codeHarness(t *testing.T, rag *fakeRag, cfg Config) (*Harness, string, func() []string) {
	t.Helper()
	t.Setenv("DUN_HOME", t.TempDir())
	ws := t.TempDir()
	var mu sync.Mutex
	var notices []string
	cfg.Workspace, cfg.Rag = ws, rag
	cfg.OnNotify = func(s string) { mu.Lock(); notices = append(notices, s); mu.Unlock() }
	h, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return h, ws, func() []string { mu.Lock(); defer mu.Unlock(); return slices.Clone(notices) }
}

func sim(v float64) *float64 { return &v }

// The session sends rag_index for its workspace when it starts and again
// when a turn ends; a child does not.
func TestCodeSearch_IndexAtStartAndTurnEnd(t *testing.T) {
	rag := &fakeRag{}
	h, ws, _ := codeHarness(t, rag, Config{Client: &visionRunner{vision: true}})
	waitFor(t, 5*time.Second, func() bool { return len(rag.indexCalls()) == 1 })
	if q := rag.indexCalls()[0]; q.Root != filepath.Clean(ws) || q.Project != raglitProject(ws) {
		t.Fatalf("rag_index %+v, want the workspace %s", q, ws)
	}
	if _, err := h.Ask(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return len(rag.indexCalls()) == 2 })
}

// DUN_CODE_SEARCH=0 (NoCodeSearch): no index, no search tool.
func TestCodeSearch_OffWithNoCodeSearch(t *testing.T) {
	rag := &fakeRag{}
	h, _, _ := codeHarness(t, rag, Config{NoCodeSearch: true})
	if slices.Contains(modelToolNames(h), "search") {
		t.Error("a search tool with NoCodeSearch")
	}
	time.Sleep(100 * time.Millisecond)
	if n := len(rag.indexCalls()); n != 0 {
		t.Errorf("%d rag_index calls with NoCodeSearch", n)
	}
}

// The search tool asks both indexes under the workspace only, and shows
// each hit as path:start-end and its symbol.
func TestCodeSearch_ToolShowsPathLinesAndSymbol(t *testing.T) {
	rag := &fakeRag{mode: "hybrid"}
	h, ws, _ := codeHarness(t, rag, Config{})
	if !slices.Contains(modelToolNames(h), "search") {
		t.Fatalf("no search tool: %v", modelToolNames(h))
	}
	rag.mu.Lock()
	rag.code = map[string][]proto.RagHit{
		"code": {{Path: "file://" + filepath.Join(ws, "zebra.go"), StartLine: 10, EndLine: 25, Symbol: "func Deploy",
			Text: "func Deploy() {\n\tfrostbite()\n}", Score: 0.03, Similarity: sim(0.7)},
			{Path: "file:///elsewhere/zebra.go", StartLine: 1, EndLine: 2, Text: "other worktree"}},
		"docs": {{Path: "file://" + filepath.Join(ws, "README.md"), StartLine: 3, EndLine: 10, Symbol: "## Zebra",
			Text: "Deploy with care.", Score: 0.02, Similarity: sim(0.6)}},
	}
	rag.mu.Unlock()
	out := callTool(t, h, "search", map[string]any{"query": "zebra deploy"})
	for _, want := range []string{"zebra.go:10-24 func Deploy", "\tfrostbite()", "README.md:3-9 ## Zebra"} {
		if !strings.Contains(out, want) {
			t.Errorf("result lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "other worktree") {
		t.Errorf("a hit outside the workspace:\n%s", out)
	}
	if i, j := strings.Index(out, "zebra.go"), strings.Index(out, "README.md"); i > j {
		t.Errorf("want the higher similarity first:\n%s", out)
	}
	rag.mu.Lock()
	defer rag.mu.Unlock()
	if len(rag.searches) != 2 || rag.searches[0].Filter.PathPrefix != "file://"+filepath.Clean(ws)+"/" {
		t.Errorf("searches %+v", rag.searches)
	}
}

// dund down: the tool says search is degraded and what to use instead, and
// the session says so once.
func TestCodeSearch_DundDownIsSaidOnce(t *testing.T) {
	rag := &fakeRag{down: true}
	h, _, notices := codeHarness(t, rag, Config{})
	out := callTool(t, h, "search", map[string]any{"query": "zebra"})
	if !strings.Contains(out, "degraded") || !strings.Contains(out, "grep") || !strings.Contains(out, "node_query") {
		t.Errorf("result %q", out)
	}
	callTool(t, h, "search", map[string]any{"query": "zebra"})
	waitFor(t, 5*time.Second, func() bool { return len(notices()) >= 1 })
	time.Sleep(50 * time.Millisecond)
	n := 0
	for _, s := range notices() {
		if strings.Contains(s, "code search") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("degraded said %d times: %q", n, notices())
	}
}

// The proactive search surfaces a code fragment as path:start-end and
// symbol, inlined from the file's lines.
func TestCodeSearch_SurfacesAFragment(t *testing.T) {
	rag := &fakeRag{}
	h, ws, _ := codeHarness(t, rag, Config{})
	src := filepath.Join(ws, "zebra.go")
	writeRule(t, src, "package z\n\n// Deploy ships to zebra.\nfunc Deploy() {\n\tfrostbite()\n}\n")
	rag.mu.Lock()
	rag.code = map[string][]proto.RagHit{"code": {{Path: "file://" + src, StartLine: 3, EndLine: 7,
		Symbol: "func Deploy", Text: "// Deploy ships to zebra.", Score: 3, Matched: 2, Terms: 2}}}
	rag.mu.Unlock()
	aside := surfaceOnce(t, h, "zebra deploy")
	for _, want := range []string{"zebra.go:3-6 func Deploy", "\tfrostbite()", "code · zebra.go:3-6"} {
		if !strings.Contains(aside, want) {
			t.Errorf("aside lacks %q:\n%s", want, aside)
		}
	}
	if strings.Contains(aside, "package z") {
		t.Errorf("inlined more than the fragment's lines:\n%s", aside)
	}
}

// With the docs server's search (/rag on), there is one search tool: the
// server's, which adds the code and docs hits when it names no index.
func TestCodeSearch_DocsServerSearchAddsCode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DUN_HOME", home)
	ws := t.TempDir()
	rag := &fakeRag{}
	rag.code = map[string][]proto.RagHit{"code": {{Path: "file://" + filepath.Join(ws, "zebra.go"), StartLine: 1, EndLine: 5,
		Symbol: "func Deploy", Text: "func Deploy() {}"}}}
	h, logPath, _ := startFakeRaglitRag(t, ws, true, rag)
	names := modelToolNames(h)
	if n := len(slices.DeleteFunc(slices.Clone(names), func(s string) bool { return s != "search" })); n != 1 {
		t.Fatalf("%d search tools: %v", n, names)
	}
	out := callTool(t, h, "search", map[string]any{"query": "zebra"})
	logHas(t, logPath, "call search", "zebra")
	if !strings.Contains(out, "This project's code and docs (dund)") || !strings.Contains(out, "zebra.go:1-4 func Deploy") {
		t.Errorf("result lacks the code hits:\n%s", out)
	}
	// The workspace is dund's: raglit does not ingest it.
	b, _ := os.ReadFile(logPath)
	if strings.Contains(string(b), "raglit ingest") {
		t.Errorf("raglit ingested the workspace:\n%s", b)
	}
}

func TestCodeSearch_SplitSpan(t *testing.T) {
	for _, c := range []struct {
		in         string
		path       string
		start, end int
	}{
		{"/a/b.go:3-9", "/a/b.go", 3, 9},
		{"/a/b.go", "/a/b.go", 0, 0},
		{"/a/b:c.go", "/a/b:c.go", 0, 0},
		{"/a/b.go:9-3", "/a/b.go:9-3", 0, 0},
	} {
		p, s, e := splitSpan(c.in)
		if p != c.path || s != c.start || e != c.end {
			t.Errorf("splitSpan(%q) = %q %d %d", c.in, p, s, e)
		}
	}
}

// A workspace of many repositories is not indexed wholesale: no rag_index,
// at start or at a turn's end. A small one is.
func TestCodeSearch_RootGuard(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws := manyRepos(t, 20)
	rag := &fakeRag{}
	h, err := Start(context.Background(), Config{Workspace: ws, Rag: rag, Client: &visionRunner{vision: true}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	if _, err := h.Ask(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if n := len(rag.indexCalls()); n != 0 {
		t.Fatalf("%d rag_index calls for a root of 20 repos: %+v", n, rag.indexCalls())
	}
	if h.codeIndexRoot() != "" {
		t.Fatalf("root %q", h.codeIndexRoot())
	}

	small := manyRepos(t, 2)
	rag2 := &fakeRag{}
	h2, err := Start(context.Background(), Config{Workspace: small, Rag: rag2, Client: &visionRunner{vision: true}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h2.Close)
	waitFor(t, 5*time.Second, func() bool { return len(rag2.indexCalls()) == 1 })
	if q := rag2.indexCalls()[0]; q.Root != filepath.Clean(small) {
		t.Fatalf("rag_index %+v", q)
	}
}
