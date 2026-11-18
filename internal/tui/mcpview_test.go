package tui

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/iodesystems/dun/internal/enginecmd"
	"github.com/iodesystems/dun/internal/harness"
	"github.com/iodesystems/dun/internal/mcptable"
)

func tableStates() []harness.ServerState {
	return []harness.ServerState{
		{ID: "playwright", Proto: "stdio", Auto: true, Via: harness.ViaShell, ViaFrom: harness.ScopeGlobal, ViaGlobal: harness.ViaShell,
			Global: "on", From: harness.ScopeGlobal, Instance: harness.InstanceShared, InstanceFrom: harness.ScopeGlobal},
		{ID: "figma", Proto: "http", Auto: true, Running: true, Tools: 3, Via: harness.ViaDirect, ViaFrom: harness.ScopeSession,
			ViaSession: harness.ViaDirect, Global: "off", Project: "on", From: harness.ScopeProject,
			Instance: harness.InstanceAgent, InstanceSession: harness.InstanceAgent, InstanceFrom: harness.ScopeSession},
		{ID: harness.ServerShell, Proto: "stdio", Global: "on", Session: "off", From: harness.ScopeSession, Instance: harness.InstanceSession},
		// Mode set at global and overridden by the project; instance set at global.
		{ID: "acme", Proto: "http", Auto: true, Via: harness.ViaDirect, ViaFrom: harness.ScopeProject, ViaGlobal: harness.ViaShell, ViaProject: harness.ViaDirect,
			Global: "on", From: harness.ScopeGlobal, Instance: harness.InstanceSession, InstanceGlobal: harness.InstanceSession, InstanceFrom: harness.ScopeGlobal},
		// Nothing sets a mode: the scripted global default decides.
		{ID: "dflt", Proto: "http", Auto: true, Via: harness.ViaShell, ViaFrom: harness.ViaFromDefault,
			Global: "on", From: harness.ScopeGlobal, Instance: harness.InstanceShared, InstanceFrom: harness.ScopeGlobal},
		// The default is scripted, but its instance is agent: standalone, and why.
		{ID: "dagent", Proto: "http", Auto: true, Via: harness.ViaDirect, ViaFrom: harness.ViaFromDefaultAgent,
			Global: "on", From: harness.ScopeGlobal, Instance: harness.InstanceAgent, InstanceGlobal: harness.InstanceAgent, InstanceFrom: harness.ScopeGlobal},
	}
}

// The text listing every client gets: the columns, a mark per switch, the
// mode and the state each with the scope that decided it.
func TestServerTable_Plain(t *testing.T) {
	out := mcptable.ServerTable(tableStates()[:3], mcptable.PlainKey)
	lines := strings.Split(out, "\n")
	if want := "  server      proto  global   project  session  mode                   instance            → state"; lines[0] != want {
		t.Errorf("header:\n%q\n%q", lines[0], want)
	}
	for i, want := range []string{
		"playwright  stdio     ●        –        –     scripted (global)      shared (global)     on (global)",
		"figma       http      ○        ●        –     standalone (session)   agent (session)     on (project)",
		"shell       stdio     ●        –        ○     standalone (built-in)  session (built-in)  off (session)",
	} {
		if !strings.HasPrefix(lines[i+1], "  "+want) {
			t.Errorf("row %d:\n%q\nwant prefix\n%q", i, lines[i+1], "  "+want)
		}
	}
}

// A hotkey letter is bold and underlined inside its word, and the styling
// changes no text. (Without a terminal the renderer drops the styling.)
func TestServerHeader_HotKeys(t *testing.T) {
	cw := mcptable.TableWidths(tableStates())
	got := mcptable.ServerHeader(cw, hotKey)
	if stripANSI(got) != mcptable.ServerHeader(cw, mcptable.PlainKey) {
		t.Errorf("styling changed the text: %q", stripANSI(got))
	}
	for _, k := range []string{"g", "p", "s", "m", "i"} {
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
		b, _ := json.Marshal(enginecmd.ServersProto([]harness.ServerState{st})[0])
		var o map[string]any
		_ = json.Unmarshal(b, &o)
		list = append(list, o)
	}
	m = m.feed(evMsg{"type": "server", "id": enginecmd.AllServers, "message": "mcp servers …", "servers": list})
	if m.serversPaging() == nil {
		t.Fatal("bare /mcp must open the table")
	}
	if strings.Contains(convoText(m), "mcp servers …") {
		t.Error("the table opened; the listing must not also land in the scrollback")
	}
	if h := stripANSI(m.serversPaging().hint(m.pg.cur)); !strings.Contains(h, "mode at global · instance at global") {
		t.Errorf("hint names the mode scope: %q", h)
	}
	if v := stripANSI(m.pg.view(140, 10)); !strings.Contains(v, "playwright  stdio     ●        –        –     scripted (global)") {
		t.Errorf("the rows keep their columns:\n%s", v)
	}
	sent := func(k string) string {
		t.Helper()
		buf.Reset()
		var cur tea.Model = m
		cur, _ = cur.Update(runesKey([]rune(k)))
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
		{"m", "mcp mode playwright standalone --global"},
		{"i", "mcp instance playwright session --global"},
		{"j", ""},
		{"i", "mcp instance figma unset --session"},
		{"p", "mcp off figma --project"},
		{"m", "mcp mode figma unset --session"},
		{"M", ""},
		{"g", ""},
		{"m", "mcp mode figma scripted --global"},
		{"i", "mcp instance figma session --global"},
		{"j", ""},
		{"i", ""}, // the shell: built in, fixed
	} {
		if got := sent(c.key); got != c.want {
			t.Errorf("%s: sent %q, want %q", c.key, got, c.want)
		}
	}
	if h := stripANSI(m.serversPaging().hint(m.pg.cur)); !strings.Contains(h, "instance at global") {
		t.Errorf("after M g: %q", h)
	}
}

// openedTable is the /mcp table on tableStates, and a sender of one key that
// returns the command it wrote.
func openedTable(t *testing.T) (*tuiModel, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	m := newTUIModel(&dunProc{stdin: bufCloser{&buf}}, "/ws")
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 140, Height: 30})
	m = nm.(tuiModel)
	m.openMCPServers()
	list := []any{}
	for _, st := range tableStates() {
		b, _ := json.Marshal(enginecmd.ServersProto([]harness.ServerState{st})[0])
		var o map[string]any
		_ = json.Unmarshal(b, &o)
		list = append(list, o)
	}
	m = m.feed(evMsg{"type": "server", "id": enginecmd.AllServers, "message": "mcp servers …", "servers": list})
	if m.serversPaging() == nil {
		t.Fatal("bare /mcp must open the table")
	}
	return &m, &buf
}

// pressKey sends a key to the table and returns the command it wrote.
func pressKey(m *tuiModel, buf *bytes.Buffer, k string) string {
	buf.Reset()
	var cur tea.Model = *m
	cur, _ = cur.Update(runesKey([]rune(k)))
	*m = cur.(tuiModel)
	var got map[string]string
	_ = json.Unmarshal(buf.Bytes(), &got)
	return got["action"]
}

// m and i write where the row's value is decided (global when nothing sets
// it), the hint says so per row, and a narrower value that still decides is
// named when a write at a broader scope cannot change what the row does.
func TestMCPServersView_AutoScope(t *testing.T) {
	m, buf := openedTable(t)
	at := func(cur int) string { return stripANSI(m.serversPaging().hint(cur)) }
	for cur, want := range map[int]string{0: "mode at global · instance at global", 1: "mode at session · instance at session",
		3: "mode at project · instance at global", 4: "mode at global · instance at global"} {
		if h := at(cur); !strings.Contains(h, want) {
			t.Errorf("row %d hint %q lacks %q", cur, h, want)
		}
	}
	for _, c := range []struct {
		row        int
		key, want  string
		flashWords string
	}{
		{3, "m", "mcp mode acme unset --project", ""},      // the project decides: standalone there cycles to unset
		{3, "i", "mcp instance acme agent --global", ""},   // set at global
		{4, "m", "mcp mode dflt standalone --global", ""},  // the scripted default reads as global scripted: next is standalone, stored explicitly
		{4, "i", "mcp instance dflt session --global", ""}, // nothing set: global
	} {
		m.pg.cur = c.row
		if got := pressKey(m, buf, c.key); got != c.want {
			t.Errorf("row %d %s: sent %q, want %q", c.row, c.key, got, c.want)
		}
		if c.flashWords != "" && !strings.Contains(m.flashText, c.flashWords) {
			t.Errorf("row %d %s: flash %q lacks %q", c.row, c.key, m.flashText, c.flashWords)
		}
	}

	// The default-agent row: m is not sent, and says why; i still is.
	m.pg.cur = 5
	if h := at(5); !strings.Contains(h, "mode at global · instance at global") {
		t.Errorf("default-agent hint: %q", h)
	}
	if got := pressKey(m, buf, "m"); got != "" || !strings.Contains(m.flashText, "press i to change the instance first") {
		t.Errorf("default-agent m: sent %q, flash %q", got, m.flashText)
	}
	if got := pressKey(m, buf, "i"); got != "mcp instance dagent unset --global" && got != "mcp instance dagent shared --global" {
		t.Errorf("default-agent i: %q", got)
	}
	// Global unset follows the default: standalone at global cycles to unset.
	m.pg.cur = 0
	pressKey(m, buf, "M")
	pressKey(m, buf, "g")
	if got := pressKey(m, buf, "m"); got != "mcp mode playwright standalone --global" {
		t.Errorf("global scripted: %q", got)
	}
	pressKey(m, buf, "M") // back to the row's own scope for the checks below
	pressKey(m, buf, "s")

	// An explicit pick is sticky across rows, and a session override that
	// still decides is named.
	m.pg.cur = 1 // figma: session decides its mode and its instance
	pressKey(m, buf, "M")
	pressKey(m, buf, "p")
	m.pg.cur = 3
	if got := pressKey(m, buf, "m"); got != "mcp mode acme unset --project" {
		t.Errorf("sticky project: %q", got)
	}
	m.pg.cur = 1
	if got := pressKey(m, buf, "m"); got != "mcp mode figma scripted --project" {
		t.Errorf("sticky project on figma: %q", got)
	}
	if !strings.Contains(m.flashText, "the session's mode (direct) still decides") || !strings.Contains(m.flashText, "M s then m") {
		t.Errorf("override flash: %q", m.flashText)
	}
	pressKey(m, buf, "M")
	pressKey(m, buf, "g")
	if got := pressKey(m, buf, "i"); got != "mcp instance figma session --global" {
		t.Errorf("sticky global: %q", got)
	}
	if !strings.Contains(m.flashText, "the session's instance (agent) still decides") {
		t.Errorf("instance override flash: %q", m.flashText)
	}
	// At the deciding scope there is nothing to say.
	pressKey(m, buf, "M")
	pressKey(m, buf, "s")
	if pressKey(m, buf, "m"); strings.Contains(m.flashText, "still decides") {
		t.Errorf("session write flashed an override: %q", m.flashText)
	}
	// A project value decides over global: acme at global is overridden.
	m.pg.cur = 3
	pressKey(m, buf, "M")
	pressKey(m, buf, "g")
	if pressKey(m, buf, "m"); !strings.Contains(m.flashText, "the project's mode (direct) still decides") {
		t.Errorf("project override flash: %q", m.flashText)
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
		"/mcp instance github ag":    "instance github agent",
		"/mcp add play":              "add playwright",
	} {
		mm := typeStr(m, in)
		_, l, ok := mm.paletteArgs()
		if !ok || len(l.choices) == 0 || l.choices[0].value != want {
			t.Errorf("%q: %+v, want %q", in, l, want)
		}
	}
}

// A server's detail says what it is and what it says about itself, whether
// it is on and which switch decided, in words, and how to list its tools.
func TestServerBody_InWords(t *testing.T) {
	st := harness.ServerState{ID: "poly-lsp", Auto: false, Global: "off", From: harness.ScopeGlobal,
		Description: "semantic code navigation and edits (poly-lsp-mcp)"}
	b := serverBody(st, "stopped")
	for _, want := range []string{"semantic code navigation and edits (poly-lsp-mcp)",
		"off: switched off for every project, nothing set for this project or session — g every project · p this project · s this session", "stopped"} {
		if !strings.Contains(b, want) {
			t.Errorf("lacks %q:\n%s", want, b)
		}
	}
	st = harness.ServerState{ID: "vay", Auto: true, Running: true, Global: "off", Session: "on", From: harness.ScopeSession,
		Via: harness.ViaShell, ViaFrom: harness.ScopeProject, Tools: 112, Standalone: 38000,
		Title: "Vay Platform", Version: "2.3.1", Instructions: "Use vay to query orders.\nPrefer batch calls."}
	b = serverBody(st, "")
	for _, want := range []string{"Vay Platform", "version 2.3.1", "it says: Use vay to query orders.\n  Prefer batch calls.",
		"on: switched on for this session", "mode: scripted: its tools are called inside the shell's eval as vay.<tool>(…)",
		"(set for this project) — m changes it", "112 tools inside the shell's eval, 0 tokens in every request (~38.0k standalone) — t lists them, type to filter"} {
		if !strings.Contains(b, want) {
			t.Errorf("lacks %q:\n%s", want, b)
		}
	}
}
