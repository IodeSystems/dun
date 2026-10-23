package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/iodesystems/dun"
)

func tableStates() []dun.ServerState {
	return []dun.ServerState{
		{ID: "playwright", Proto: "stdio", Auto: true, Via: dun.ViaShell, ViaFrom: dun.ScopeGlobal, ViaGlobal: dun.ViaShell,
			Global: "on", From: dun.ScopeGlobal},
		{ID: "figma", Proto: "http", Auto: true, Running: true, Tools: 3, Via: dun.ViaDirect, ViaFrom: dun.ScopeSession,
			ViaSession: dun.ViaDirect, Global: "off", Project: "on", From: dun.ScopeProject},
		{ID: dun.ServerShell, Proto: "stdio", Global: "on", Session: "off", From: dun.ScopeSession},
	}
}

// The text listing every client gets: the columns, a mark per switch, the
// mode and the state each with the scope that decided it.
func TestServerTable_Plain(t *testing.T) {
	out := serverTable(tableStates(), plainKey)
	lines := strings.Split(out, "\n")
	if want := "  server      proto  global   project  session  mode                   → state"; lines[0] != want {
		t.Errorf("header:\n%q\n%q", lines[0], want)
	}
	for i, want := range []string{
		"playwright  stdio     ●        –        –     scripted (global)      on (global)",
		"figma       http      ○        ●        –     standalone (session)   on (project)",
		"shell       stdio     ●        –        ○     standalone (built-in)  off (session)",
	} {
		if !strings.HasPrefix(lines[i+1], "  "+want) {
			t.Errorf("row %d:\n%q\nwant prefix\n%q", i, lines[i+1], "  "+want)
		}
	}
}

// A hotkey letter is bold and underlined inside its word; with no color
// (NO_COLOR is lipgloss's Ascii profile) the header is the plain one.
func TestServerHeader_HotKeys(t *testing.T) {
	old := lipgloss.ColorProfile()
	t.Cleanup(func() { lipgloss.SetColorProfile(old) })
	lipgloss.SetColorProfile(termenv.Ascii)
	idW, viaW := tableWidths(tableStates())
	if got, want := serverHeader(idW, viaW, hotKey), serverHeader(idW, viaW, plainKey); got != want {
		t.Errorf("no color:\n%q\n%q", got, want)
	}
	lipgloss.SetColorProfile(termenv.ANSI256)
	got := serverHeader(idW, viaW, hotKey)
	if stripANSI(got) != serverHeader(idW, viaW, plainKey) {
		t.Errorf("styling changed the text: %q", stripANSI(got))
	}
	for _, k := range []string{"g", "p", "s", "m"} {
		if !strings.Contains(got, stHotKey.Render(k)) {
			t.Errorf("%s not marked: %q", k, got)
		}
	}
}

// Bare /mcp opens the table; g/p/s cycle that column of the selected row
// and m the mode at the mode scope, which M then g/p/s picks.
func TestMCPServersView_Keys(t *testing.T) {
	var buf bytes.Buffer
	m := newTUIModel(&dunProc{stdin: bufCloser{&buf}}, "/ws")
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 140, Height: 30})
	m = nm.(tuiModel)
	m.openMCPServers()
	list := []any{}
	for _, st := range tableStates() {
		b, _ := json.Marshal(serversProto([]dun.ServerState{st})[0])
		var o map[string]any
		_ = json.Unmarshal(b, &o)
		list = append(list, o)
	}
	m = m.handleEvent(evMsg{"type": "server", "id": allServers, "message": "mcp servers …", "servers": list})
	if m.serversPaging() == nil {
		t.Fatal("bare /mcp must open the table")
	}
	if strings.Contains(convoText(m), "mcp servers …") {
		t.Error("the table opened; the listing must not also land in the scrollback")
	}
	if h := stripANSI(m.serversPaging().hint()); !strings.Contains(h, "mode at session") {
		t.Errorf("hint names the mode scope: %q", h)
	}
	sent := func(k string) string {
		t.Helper()
		buf.Reset()
		var cur tea.Model = m
		cur, _ = cur.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)})
		m = cur.(tuiModel)
		if buf.Len() == 0 {
			return ""
		}
		var got map[string]string
		_ = json.Unmarshal(buf.Bytes(), &got)
		return got["action"]
	}
	for _, c := range []struct{ key, want string }{
		{"g", "mcp off playwright --global"},
		{"p", "mcp on playwright --project"},
		{"s", "mcp on playwright --session"},
		{"m", "mcp mode playwright scripted --session"},
		{"j", ""},
		{"p", "mcp off figma --project"},
		{"m", "mcp mode figma unset --session"},
		{"M", ""},
		{"g", ""},
		{"m", "mcp mode figma scripted --global"},
	} {
		if got := sent(c.key); got != c.want {
			t.Errorf("%s: sent %q, want %q", c.key, got, c.want)
		}
	}
	if h := stripANSI(m.serversPaging().hint()); !strings.Contains(h, "mode at global") {
		t.Errorf("after M g: %q", h)
	}
}

// The palette completes each new form position by position.
func TestPalette_MCPScopeCompletion(t *testing.T) {
	m, _ := skillModel(t, 0)
	m.engineCmds = engineCmdsFromEvent(evMsg{"items": []any{map[string]any{"name": "mcp",
		"choices": []any{map[string]any{"value": "on"}, map[string]any{"value": "mode"}}}}})
	m.noteServers(evMsg{"servers": []any{map[string]any{"id": "github"}}})
	for in, want := range map[string]string{
		"/mcp on github --p":         "on github --project",
		"/mcp mode github scr":       "mode github scripted",
		"/mcp mode github scripted ": "mode github scripted --global",
		"/mcp unset github ":         "unset github --project",
		"/mcp add play":              "add playwright",
	} {
		mm := typeStr(m, in)
		_, l, ok := mm.paletteArgs()
		if !ok || len(l.choices) == 0 || l.choices[0].value != want {
			t.Errorf("%q: %+v, want %q", in, l, want)
		}
	}
}
