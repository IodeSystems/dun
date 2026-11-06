package tui

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

// In the /file table, a search starts only with "/" or "~" (USER
// 2026-11-06: "we should HAVE to type "/" to search, so we can toggle things
// on the file"): before it, g/p/s cycle a scope and r/w set the mode on the
// row, and other letters are the pager's; typed, a path and enter adds it
// for the session. ctrl+w still flips read ↔ write where it is decided.
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
	if got := v.view(); !strings.Contains(got, "/ or ~ searches") || !strings.Contains(got, "w read and write") {
		t.Errorf("hint lacks search/write:\n%s", got)
	}
	sw := v.m.switchesPaging()
	for _, c := range []struct{ key, want string }{
		{"w", "mode /home/me/notes write --global"},
		{"r", "mode /home/me/notes read --global"},
		{"g", "off /home/me/notes --global"},
		{"s", "on /home/me/notes --session"},
		{"p", "on /home/me/notes --project"},
	} {
		rec.Reset()
		v.m = typeStr(v.m, c.key)
		if s := rec.String(); !strings.Contains(s, c.want) {
			t.Errorf("%s sent %s, want %q", c.key, s, c.want)
		}
		if sw.query != "" {
			t.Errorf("%s started a search: %q", c.key, sw.query)
		}
	}
	rec.Reset()
	v.m = typeStr(v.m, "x")
	if sw.query != "" || rec.Len() != 0 {
		t.Errorf("x: query %q, sent %s", sw.query, rec.String())
	}
	rec.Reset()
	v.m = key(v.m, tea.KeyPressMsg{Code: 'w', Mod: tea.ModCtrl})
	if s := rec.String(); !strings.Contains(s, "mode /home/me/notes write --global") || !strings.Contains(s, `"paths"`) {
		t.Errorf("ctrl+w sent %s", s)
	}
	// Searching, g p s r w are the path's letters.
	rec.Reset()
	v.m = typeStr(v.m, "~/src/gprsw")
	if sw.query != "~/src/gprsw" || rec.Len() != 0 {
		t.Fatalf("typed a path: query %q, sent %s", sw.query, rec.String())
	}
	v.m = key(v.m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if s := rec.String(); !strings.Contains(s, "add ~/src/gprsw --session") {
		t.Errorf("enter sent %s", s)
	}
	rec.Reset()
	v.m = typeStr(v.m, "/tmp/a b")
	v.m = key(v.m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if s := rec.String(); !strings.Contains(s, "add /tmp/a b --session") {
		t.Errorf("a path with a space: sent %s", s)
	}
	// Backspace over the "/" ends the search: g is a key again.
	v.m = typeStr(v.m, "/")
	v.m = key(v.m, tea.KeyPressMsg{Code: tea.KeyBackspace})
	rec.Reset()
	v.m = typeStr(v.m, "g")
	if s := rec.String(); !strings.Contains(s, "--global") {
		t.Errorf("g after the search ended: sent %s", s)
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

// /rules is a tree: an import sits under the file that imports it, one
// indent a level; a file with imports shows its cost with them; the pane
// under the list says where the rules are looked for (USER 2026-11-06).
func TestRulesPicker_TreeAndSearchPane(t *testing.T) {
	v := newVtui(160, 40)
	v.event(map[string]any{"type": "ready", "tools": []any{"eval"}})
	rec := &recWC{}
	v.m.proc = &dunProc{stdin: rec}
	v.m = typeStr(v.m, "/rules")
	v.m = key(v.m, tea.KeyPressMsg{Code: tea.KeyEnter})
	v.event(map[string]any{"type": "switches", "kind": "rules", "slash": "rules", "mode": "full", "order": []any{"full", "summary", "index"},
		"items": []any{
			map[string]any{"name": "AGENTS.md", "source": "root", "tokens": 1000.0, "on": true},
			map[string]any{"name": "docs/a.md", "source": "import", "parent": "AGENTS.md", "tokens": 2000.0, "on": true},
			map[string]any{"name": "docs/b.md", "source": "import", "parent": "docs/a.md", "tokens": 500.0, "on": true},
			map[string]any{"name": "docs/off.md", "source": "import", "parent": "AGENTS.md", "tokens": 9000.0, "on": false},
		},
		"search": []any{"looked for in /ws:", "  ✓ AGENTS.md", "  ✗ CLAUDE.md (not there)"},
	})
	got := v.view()
	for _, want := range []string{
		"● AGENTS.md  ~1.0k tok (~3.5k with imports)",
		"▸   ● docs/a.md  ~2.0k tok (~2.5k with imports)",
		"▸     ● docs/b.md  ~500 tok  g",
		"✗ CLAUDE.md (not there)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("lacks %q:\n%s", want, got)
		}
	}
	if strings.Index(got, "✗ CLAUDE.md") < strings.Index(got, "docs/b.md") {
		t.Errorf("the search pane is not under the list:\n%s", got)
	}
}
