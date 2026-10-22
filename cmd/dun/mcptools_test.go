package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/iodesystems/dun"
)

func toolsEvent(enabled bool) evMsg {
	return evMsg{"type": "mcp_tools", "server": "el", "tools": []any{
		map[string]any{"name": "tts_say", "description": "Say a text.\nLonger detail.", "schema": "{\n  \"type\": \"object\"\n}", "tokens": 120.0, "enabled": enabled},
		map[string]any{"name": "voices", "description": "List voices.", "schema": "{}", "tokens": 80.0, "enabled": true},
	}}
}

// /mcp tools <id> goes to the engine as a slash line; its answer opens the
// pager tree; enter opens a row to its description and schema; space sends
// the toggle, and the fresh list lays out in place.
func TestMCPTools_PagerOpensExpandsToggles(t *testing.T) {
	var buf bytes.Buffer
	m := newTUIModel(&dunProc{stdin: bufCloser{&buf}}, "/ws")
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = nm.(tuiModel)
	m.runSlash("/mcp tools el")
	var sent map[string]string
	if err := json.Unmarshal(buf.Bytes(), &sent); err != nil || sent["id"] != "slash" || sent["action"] != "mcp tools el" {
		t.Fatalf("sent %q", buf.String())
	}
	buf.Reset()
	m = m.handleEvent(toolsEvent(true))
	if !m.paging || m.toolsPaging() == nil {
		t.Fatal("the list did not open the pager")
	}
	v := stripANSI(m.pg.view(100, 20))
	if !strings.Contains(v, "el: 2 of 2 on") || !strings.Contains(v, "● tts_say") || !strings.Contains(v, "Say a text.") {
		t.Fatalf("view:\n%s", v)
	}
	if !strings.Contains(m.pg.hint(), "space on/off") {
		t.Errorf("hint %q", m.pg.hint())
	}
	var cur tea.Model = m
	cur, _ = cur.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = cur.(tuiModel)
	if v := stripANSI(m.pg.view(100, 20)); !strings.Contains(v, "Longer detail.") || !strings.Contains(v, `"type": "object"`) {
		t.Fatalf("enter must open the row:\n%s", v)
	}
	cur, _ = cur.Update(tea.KeyMsg{Type: tea.KeySpace, Runes: []rune(" ")})
	m = cur.(tuiModel)
	sent = nil
	if err := json.Unmarshal(buf.Bytes(), &sent); err != nil || sent["id"] != "mcptools" || sent["action"] != "toggle el tts_say off" {
		t.Fatalf("space sent %q", buf.String())
	}
	ev := toolsEvent(false)
	ev["message"] = "el: tts_say off (saved to x)"
	m = m.handleEvent(ev)
	if v := stripANSI(m.pg.view(100, 20)); !m.paging || !strings.Contains(v, "○ tts_say") || !strings.Contains(v, "1 of 2 on") {
		t.Fatalf("after the toggle:\n%s", v)
	}
	if m.pg.cur != 0 || !m.pg.open[0] {
		t.Error("the selection and the open row must survive a fresh list")
	}
	// Someone else's list does not open a pager here.
	m.paging = false
	m = m.handleEvent(toolsEvent(true))
	if m.paging {
		t.Error("an unrequested list opened the pager")
	}
}

// The listing's per-server cost, and the hint for a heavy one.
func TestToolCost_AndHeavyHint(t *testing.T) {
	st := dun.ServerState{ID: "el", Running: true, Tools: 160, Cost: 38200, Hidden: 12, Via: dun.ViaDirect}
	if got := toolCost(st); got != "160 tools · ~38.2k tokens · 12 hidden" {
		t.Errorf("cost %q", got)
	}
	if h := heavyHint(st); !strings.Contains(h, "/mcp tools el") || !strings.Contains(h, "/mcp move el shell") {
		t.Errorf("hint %q", h)
	}
	st.Via = "" // a built-in cannot move
	if h := heavyHint(st); strings.Contains(h, "move") {
		t.Errorf("built-in hint %q", h)
	}
	if heavyHint(dun.ServerState{ID: "x", Cost: 100}) != "" {
		t.Error("a light server needs no hint")
	}
	if v := serverListing([]dun.ServerState{st}); !strings.Contains(v, "38.2k tokens") || !strings.Contains(v, "heavy:") {
		t.Errorf("listing:\n%s", v)
	}
}
