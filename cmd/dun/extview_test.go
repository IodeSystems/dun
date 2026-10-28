package main

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// /ext is a screen: its status (how many on, where they load from, what
// did not load), a row per extension with its server's state, and g sends
// /mcp's switch for that server.
func TestExtScreen(t *testing.T) {
	for _, w := range []int{100, 48} {
		rec := &recWC{}
		m := newTUIModel(&dunProc{stdin: rec}, "/ws")
		nm, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: 30})
		m = nm.(tuiModel)
		m.noteServers(evMsg{"servers": []any{map[string]any{"id": "raglit", "auto": false, "global": "off"}}})
		m.openExtensions()
		if !strings.Contains(rec.String(), `"view"`) {
			t.Fatalf("not asked: %s", rec.String())
		}
		m = m.noteExtensions(evMsg{"type": "extensions", "addPath": "~/.dun/extensions/<id>/extension.json",
			"errors": []any{"bad: extension.json: unknown field"},
			"items": []any{map[string]any{"id": "raglit", "source": "bundled", "slash": "rag", "description": "search and ingest",
				"roles":    []any{"search:search", "index:ingest/forget"},
				"commands": []any{map[string]any{"name": "setup", "help": "pick an embedding provider"}}}}})
		if m.activeScreen() == nil {
			t.Fatalf("%d: no screen", w)
		}
		plain := ansi.Strip(m.drawView())
		flat := strings.ReplaceAll(plain, "\n", " ")
		for _, want := range []string{"/ext · 0 of 1 on", "⚠ 1 manifest did not load", "○ raglit · off", "✗ bad: extension.json"} {
			if !strings.Contains(flat, want) {
				t.Errorf("%d: lacks %q:\n%s", w, want, plain)
			}
		}
		if w < narrowW && !strings.Contains(plain, "    bundled · /rag · search:search") {
			t.Errorf("%d: not two lines:\n%s", w, plain)
		}
		rec.Reset()
		nm, _ = m.Update(tea.KeyPressMsg{Code: 'g', Text: "g"})
		m = nm.(tuiModel)
		if !strings.Contains(rec.String(), "mcp on raglit --global") {
			t.Errorf("%d: g sent %s", w, rec.String())
		}
	}
}
