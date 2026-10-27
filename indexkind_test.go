package dun

import (
	"context"
	"strings"
	"sync"
	"testing"
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
		Servers: []Server{{ID: ServerDocs, URL: ts.URL, Timeout: 10}}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	listErr := error(nil)
	old := listIndexes
	listIndexes = func(context.Context, string, string) ([]raglitIndexRow, error) {
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
	if !ok || len(v.Items) != 2 || v.Items[0].On || v.Items[0].Source != "2304 docs" {
		t.Fatalf("view: %+v", v.Items)
	}
	if _, err := h.SetSwitch(indexesKind, "aw4:aw4-main", ScopeProject, ptr(true)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.SetSwitch(indexesKind, ":code", ScopeSession, ptr(true)); err != nil {
		t.Fatal(err)
	}
	v, _ = h.SwitchView(indexesKind)
	if !v.Items[0].On || v.Items[0].Project != "on" || v.Items[0].From != ScopeProject || !v.Items[1].On || v.Items[1].Session != "on" {
		t.Fatalf("after attach: %+v", v.Items)
	}
	if _, err := h.SetSwitch(indexesKind, "aw4:aw4-main", ScopeProject, ptr(false)); err != nil {
		t.Fatal(err)
	}
	listErr = errString("raglit index --list --json: unknown flag")
	v, _ = h.SwitchView(indexesKind)
	if len(v.Items) != 1 || v.Items[0].Name != ":code" || !strings.Contains(v.Note, "unknown flag") {
		t.Fatalf("fallback: %+v note %q", v.Items, v.Note)
	}
	if _, err := h.SetSwitchMode(indexesKind, "x", ScopeSession); err == nil {
		t.Error("a mode was taken")
	}
}

type errString string

func (e errString) Error() string { return string(e) }
