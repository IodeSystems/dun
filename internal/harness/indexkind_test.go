package harness

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/iodesystems/dun/internal/mcpserver"
)

// The indexes kind: every index raglit lists, each one's attachment per
// scope; switching one on attaches it there, off detaches it; a global
// index (":code") attaches too; a raglit with no --json still shows the
// attached ones, with why.
func TestIndexesKind_ListAttachDetach(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	var calls []string
	var mu sync.Mutex
	ts := fakeIndexMCP(t, &calls, &mu)
	ws := t.TempDir()
	h, err := Start(context.Background(), Config{Workspace: ws, SessionFile: t.TempDir() + "/s.jsonl",
		Servers: []mcpserver.Server{{ID: mcpserver.ServerDocs, URL: ts.URL, Timeout: 10}}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	listErr := error(nil)
	old := listIndexes
	listIndexes = func(context.Context, string, string, []string) ([]raglitIndexRow, error) {
		if listErr != nil {
			return nil, listErr
		}
		return []raglitIndexRow{
			{Selector: "aw4:aw4-main", Project: "aw4", Index: "aw4-main", Documents: 2304, Root: "/src/aw4"},
			{Selector: ":code", Index: "code"},
		}, nil
	}
	defer func() { listIndexes = old }()

	v, ok := h.SwitchView(indexesKind)
	// [0] is dund's project index, with no switch (USER 2026-11-17); [1]
	// the provider, raglit itself, its switch /mcp's; the indexes sit under
	// it (USER 2026-10-28).
	if !ok || len(v.Items) != 4 || v.Items[0].Name != projectIndexName || v.Items[0].Source != "project index (dund) · off (DUN_MEMORY=0) · code, docs, memories, strategies, sessions" ||
		v.Items[1].Name != mcpserver.ServerDocs || v.Items[1].Source != "rag add-on (docs_search) · off · 2 indexes" ||
		v.Items[2].On || v.Items[2].Source != "2304 docs" || v.Items[2].Parent != mcpserver.ServerDocs {
		t.Fatalf("view: %+v", v.Items)
	}
	if _, err := h.SetSwitch(indexesKind, projectIndexName, mcpserver.ScopeSession, ptr(false)); err == nil || !strings.Contains(err.Error(), "always on") {
		t.Errorf("the project index switched: %v", err)
	}
	v.Items = v.Items[2:]
	if _, err := h.SetSwitch(indexesKind, "aw4:aw4-main", mcpserver.ScopeProject, ptr(true)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.SetSwitch(indexesKind, ":code", mcpserver.ScopeSession, ptr(true)); err != nil {
		t.Fatal(err)
	}
	v, _ = h.SwitchView(indexesKind)
	v.Items = v.Items[2:]
	if !v.Items[0].On || v.Items[0].Project != "on" || v.Items[0].From != mcpserver.ScopeProject || !v.Items[1].On || v.Items[1].Session != "on" {
		t.Fatalf("after attach: %+v", v.Items)
	}
	if _, err := h.SetSwitch(indexesKind, "aw4:aw4-main", mcpserver.ScopeProject, ptr(false)); err != nil {
		t.Fatal(err)
	}
	listErr = errString("raglit index --list --json: unknown flag")
	v, _ = h.SwitchView(indexesKind)
	byName := map[string]SwitchState{}
	for _, it := range v.Items {
		byName[it.Name] = it
	}
	if a := byName["aw4:aw4-main"]; len(v.Items) != 4 || a.On || a.Project != "off" || !byName[":code"].On || !strings.Contains(v.Note, "unknown flag") {
		t.Fatalf("fallback: %+v note %q", v.Items, v.Note)
	}
	if _, err := h.SetSwitchMode(indexesKind, "x", mcpserver.ScopeSession); err == nil {
		t.Error("a mode was taken")
	}
	// The provider row's switch is /mcp's switch on raglit.
	if _, err := h.SetSwitch(indexesKind, mcpserver.ServerDocs, mcpserver.ScopeSession, ptr(false)); err != nil {
		t.Fatal(err)
	}
	if s, _ := h.knownServer(mcpserver.ServerDocs); s.Scopes.Session == nil || *s.Scopes.Session {
		t.Errorf("raglit's session switch: %+v", s.Scopes.Session)
	}
}

type errString string

func (e errString) Error() string { return string(e) }

// With dund's link, the project index row says whether dund answers, and
// the workspace line says dund reads the workspace; /rag workspace says it
// has no effect.
func TestIndexesKind_ProjectIndexRow(t *testing.T) {
	rag := &fakeRag{}
	h, _, _ := codeHarness(t, rag, Config{})
	old := listIndexes
	listIndexes = func(context.Context, string, string, []string) ([]raglitIndexRow, error) { return nil, nil }
	t.Cleanup(func() { listIndexes = old })
	v, _ := h.SwitchView(indexesKind)
	if len(v.Items) < 2 || v.Items[0].Name != projectIndexName || !v.Items[0].On ||
		!strings.Contains(v.Items[0].Source, "project index (dund) · up · ") || v.Items[1].Name != mcpserver.ServerDocs {
		t.Fatalf("view: %+v", v.Items)
	}
	if len(v.Status) == 0 || !strings.HasPrefix(v.Status[0], "dund reads this workspace (always)") {
		t.Errorf("status %q", v.Status)
	}
	if msg, err := h.SetWorkspaceIndexed(false, mcpserver.ScopeProject); err != nil || !strings.Contains(msg, "no effect now: dund reads this workspace") {
		t.Errorf("/rag workspace off: %q %v", msg, err)
	}
	rag.setDown(true)
	if v, _ := h.SwitchView(indexesKind); !strings.Contains(v.Items[0].Source, "· dund down ·") {
		t.Errorf("dund down: %q", v.Items[0].Source)
	}
}
