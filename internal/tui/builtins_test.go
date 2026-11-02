package tui

import (
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/iodesystems/dun"
)

// The /mcp table lists the groups under the servers; g on one sends its
// switch.
func TestMCPTable_ShowsBuiltinGroups(t *testing.T) {
	rec := &recWC{}
	m := newTUIModel(&dunProc{stdin: rec}, "/ws")
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m = nm.(tuiModel)
	m.mcpServersWant = true
	ev := evMsg{"type": "server", "id": "*", "servers": []any{map[string]any{"id": "sh", "running": true, "global": "on"}},
		"builtins": []any{map[string]any{"id": "dun:files", "running": true, "auto": true, "global": "on", "tools": float64(3), "proto": "dun"}}}
	m = m.handleEvent(ev)
	plain := ansi.Strip(m.drawView())
	if !strings.Contains(plain, "dun:files") || !strings.Contains(plain, "sh") {
		t.Fatalf("table:\n%s", plain)
	}
	if len(m.servers) != 0 && slices.ContainsFunc(m.servers, func(s dun.ServerState) bool { return s.ID == "dun:files" }) {
		t.Error("a built-in group reached the server list the splash reads")
	}
	nm, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	m = nm.(tuiModel)
	rec.Reset()
	nm, _ = m.Update(tea.KeyPressMsg{Code: 'g', Text: "g"})
	m = nm.(tuiModel)
	if !strings.Contains(rec.String(), "mcp off dun:files --global") {
		t.Errorf("g sent %s", rec.String())
	}
}
