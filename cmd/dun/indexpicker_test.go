package main

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// Bare /rag attach opens every index, searchable; ctrl+g attaches the
// row under the filter for every project (USER 2026-10-27).
func TestIndexPicker_RaglitAttach(t *testing.T) {
	v := newVtui(140, 30)
	v.event(map[string]any{"type": "session", "id": "s1", "resumed": float64(0)})
	v.event(map[string]any{"type": "ready", "tools": []any{"eval"}})
	rec := &recWC{}
	v.m.proc = &dunProc{stdin: rec}
	v.m = typeStr(v.m, "/rag attach")
	v.m = key(v.m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if !strings.Contains(rec.String(), `"indexes"`) || v.m.switchesWant != "indexes" {
		t.Fatalf("not asked: %s", rec.String())
	}
	v.event(map[string]any{"type": "switches", "kind": "indexes", "slash": "rag", "items": []any{
		map[string]any{"name": "aw4:aw4-main", "description": "/src/aw4", "source": "2304 docs"},
		map[string]any{"name": ":code", "source": "0 docs · global", "on": true, "from": "session", "session": "on"},
	}, "status": []any{"this workspace is not indexed"}})
	got := v.view()
	for _, want := range []string{"/rag · 1 of 2 on", "this workspace is not indexed", "2 indexes", "○ aw4:aw4-main", "● :code"} {
		if !strings.Contains(got, want) {
			t.Errorf("lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "tok") {
		t.Errorf("a token cost on indexes:\n%s", got)
	}
	v.m = typeStr(v.m, "aw4")
	rec.Reset()
	v.m = key(v.m, tea.KeyPressMsg{Code: 'g', Mod: tea.ModCtrl})
	if !strings.Contains(rec.String(), "indexes on aw4:aw4-main --global") {
		t.Errorf("sent %s", rec.String())
	}
}
