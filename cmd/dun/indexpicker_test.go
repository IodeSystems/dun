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

// Bare /file is the paths table (USER 2026-10-28): each path, its scopes,
// and read or write and indexed in its row.
func TestFilePicker_BareFileOpensTheTable(t *testing.T) {
	v := newVtui(140, 30)
	v.event(map[string]any{"type": "ready", "tools": []any{"eval"}})
	rec := &recWC{}
	v.m.proc = &dunProc{stdin: rec}
	v.m = typeStr(v.m, "/file")
	v.m = key(v.m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if !strings.Contains(rec.String(), `"paths"`) || v.m.switchesWant != "paths" {
		t.Fatalf("not asked: %s", rec.String())
	}
	v.event(map[string]any{"type": "switches", "kind": "paths", "slash": "file", "items": []any{
		map[string]any{"name": "/home/me/notes", "source": "read · indexed (p:file-notes-a1b2c3)", "on": true, "from": "global", "global": "on"},
	}})
	got := v.view()
	for _, want := range []string{"/file · 1 of 1 on", "1 paths", "● /home/me/notes", "read · indexed"} {
		if !strings.Contains(got, want) {
			t.Errorf("lacks %q:\n%s", want, got)
		}
	}
}

// In the /file table, a typed path and enter adds it for the session, and
// ctrl+w flips the row read only ↔ read and write where it is decided; the
// hint names both (USER 2026-10-28: "no obvious hotkey that says add").
func TestFilePicker_AddAndWriteKeys(t *testing.T) {
	v := newVtui(160, 30)
	v.event(map[string]any{"type": "ready", "tools": []any{"eval"}})
	rec := &recWC{}
	v.m.proc = &dunProc{stdin: rec}
	v.m = typeStr(v.m, "/file")
	v.m = key(v.m, tea.KeyPressMsg{Code: tea.KeyEnter})
	v.event(map[string]any{"type": "switches", "kind": "paths", "slash": "file", "items": []any{
		map[string]any{"name": "/home/me/notes", "source": "read · indexed", "on": true, "from": "global", "global": "on"},
	}})
	if got := v.view(); !strings.Contains(got, "enter adds it") || !strings.Contains(got, "ctrl+w read/write") {
		t.Errorf("hint lacks add/write:\n%s", got)
	}
	rec.Reset()
	v.m = key(v.m, tea.KeyPressMsg{Code: 'w', Mod: tea.ModCtrl})
	if s := rec.String(); !strings.Contains(s, "mode /home/me/notes write --global") || !strings.Contains(s, `"paths"`) {
		t.Errorf("ctrl+w sent %s", s)
	}
	rec.Reset()
	v.m = typeStr(v.m, "~/src")
	v.m = key(v.m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if s := rec.String(); !strings.Contains(s, "add ~/src --session") {
		t.Errorf("enter sent %s", s)
	}
}

// Bare /lsp is the languages table (USER 2026-10-28): each language's
// server on or off; ctrl+g switches the row under the filter for every
// project (on → off → unset).
func TestLSPPicker_BareLSPOpensTheTable(t *testing.T) {
	v := newVtui(140, 30)
	v.event(map[string]any{"type": "ready", "tools": []any{"eval"}})
	rec := &recWC{}
	v.m.proc = &dunProc{stdin: rec}
	v.m = typeStr(v.m, "/lsp")
	v.m = key(v.m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if !strings.Contains(rec.String(), `"lsp"`) || v.m.switchesWant != "lsp" {
		t.Fatalf("not asked: %s", rec.String())
	}
	v.event(map[string]any{"type": "switches", "kind": "lsp", "slash": "lsp", "items": []any{
		map[string]any{"name": "go", "description": "gopls · found · .go", "source": "builtin", "on": true},
		map[string]any{"name": "java", "description": "jdtls · found · .java", "source": "user · off by default"},
	}})
	got := v.view()
	for _, want := range []string{"/lsp · 1 of 2 on", "2 languages", "● go", "○ java", "gopls · found"} {
		if !strings.Contains(got, want) {
			t.Errorf("lacks %q:\n%s", want, got)
		}
	}
	v.m = typeStr(v.m, "go")
	rec.Reset()
	v.m = key(v.m, tea.KeyPressMsg{Code: 'g', Mod: tea.ModCtrl})
	if !strings.Contains(rec.String(), "lsp on go --global") {
		t.Errorf("sent %s", rec.String())
	}
}
