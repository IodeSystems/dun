package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// A first run: the engine says it has no AI service, and the screen's one
// line is what to do about it. A question asked anyway is not sent; the
// line moves under it (said once); after a login and a pick the question
// is back in the composer — not sent — and the pick is confirmed, with no
// "resumed" debris from the restart.
func TestOnboarding_NotLoggedInHoldsTheQuestion(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	var sent bytes.Buffer
	m := newTUIModel(&dunProc{stdin: bufCloser{&sent}}, "/ws")
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = nm.(tuiModel)
	m = m.handleEvent(evMsg{"type": "ready", "tools": []any{"eval"},
		"hint": "off: raglit — /raglit on (auto: every session)"})
	m = m.handleEvent(evMsg{"type": "settings", "model": "", "url": ""})
	txt := convoText(m)
	if !strings.Contains(txt, "not logged in — /login to connect to an AI service") || strings.Contains(txt, "ready") {
		t.Fatalf("first screen:\n%s", txt)
	}

	m = typeStr(m, "Can you help me with this project?")
	m = key(m, kEnter)
	if strings.Contains(sent.String(), `"type":"user"`) {
		t.Fatalf("sent to an engine with nothing to send to: %s", sent.String())
	}
	txt = convoText(m)
	if strings.Contains(txt, "not logged in") || !strings.HasSuffix(strings.TrimSpace(stripANSI(txt)), heldLine) {
		t.Fatalf("the held line, once, under the question, in place of the not-logged-in line:\n%s", txt)
	}

	m.openProviderForm(nil)
	m.providerFields[fieldName].SetValue("work")
	m.providerFields[fieldAddr].SetValue("http://llm/v1")
	m.saveProvider()
	if txt := convoText(m); strings.Contains(txt, "not logged in") || !strings.Contains(txt, "logged in to work") {
		t.Errorf("after the login:\n%s", txt)
	}
	m.switchModel(providerConfig{Name: "work", URL: "http://llm/v1"}, "chat-standard", true)
	m = m.handleEvent(evMsg{"type": "history", "items": []any{map[string]any{"kind": "user", "content": "earlier"}}})
	m = m.handleEvent(evMsg{"type": "ready", "tools": []any{"eval"}, "hint": "off: raglit — /raglit on (auto: every session)"})
	m = m.handleEvent(evMsg{"type": "settings", "model": "chat-standard", "url": "http://llm/v1", "provider": "work"})
	txt = convoText(m)
	for _, gone := range []string{"resumed", "not logged in", "kept your question", "off: raglit"} {
		if strings.Contains(txt, gone) {
			t.Errorf("after the pick the screen still says %q:\n%s", gone, txt)
		}
	}
	if !strings.Contains(txt, "using chat-standard on work · saved as default") {
		t.Errorf("the pick is not confirmed:\n%s", txt)
	}
	if got := m.input.Value(); got != "Can you help me with this project?" {
		t.Errorf("composer = %q, want the held question back", got)
	}
	if strings.Contains(sent.String(), `"type":"user"`) {
		t.Error("the held question was sent without the user")
	}
	if h := stripANSI(m.headView()); !strings.Contains(h, "chat-standard") {
		t.Errorf("header lacks the model: %q", h)
	}
}

// The engine's own not-logged-in error (another client, or a model with no
// URL) is the next step, not "error: agent: chat: …" with retry advice.
func TestOnboarding_SetupErrorIsTheNextStep(t *testing.T) {
	if e := setupMissing(fmt.Errorf("agent: chat: %w", errNoLogin)); e != errNoLogin {
		t.Fatalf("setupMissing = %v", e)
	}
	m := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	m = m.handleEvent(evMsg{"type": "error", "error": errNoLogin.Error()})
	txt := convoText(m)
	if strings.Contains(txt, "error:") || strings.Contains(txt, "session is intact") || !strings.Contains(txt, "/login") {
		t.Errorf("convo:\n%s", txt)
	}
}

// The picker saves the pick as the default; alt+enter is for this session
// only. No checkbox, and tab toggles nothing.
func TestModelPicker_EnterSavesAltEnterDoesNot(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	cfg := loadConfig()
	cfg.putProvider(providerConfig{Name: "work", URL: "http://llm/v1"})
	if err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	m := pickerWith(modelRow{"work", "a"}, modelRow{"work", "b"})
	v := stripANSI(m.modelPanel())
	if strings.Contains(v, "[ ]") || strings.Contains(v, "save as default") || strings.Contains(v, "work  a") {
		t.Errorf("panel:\n%s", v)
	}
	m = pick(m, "tab", "enter")
	if c := loadConfig(); c.Model != "a" || c.Provider != "work" {
		t.Errorf("enter did not save the default: %+v", c)
	}
	if m.switchNote != "using a on work · saved as default" {
		t.Errorf("note %q", m.switchNote)
	}
	m.openModelPicker("")
	m.modelRows = []modelRow{{"work", "a"}, {"work", "b"}}
	m = pick(m, "down")
	nm, _ := m.updateModelPicking(tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModAlt})
	m = nm.(tuiModel)
	if c := loadConfig(); c.Model != "a" {
		t.Errorf("alt+enter changed the default to %q", c.Model)
	}
	if m.switchNote != "using b on work · this session only" {
		t.Errorf("note %q", m.switchNote)
	}
}

// No row below the conversation is cut mid-word: hints lose whole hints from
// the end, other text ends at a word with an ellipsis.
func TestFit_NothingCutMidWord(t *testing.T) {
	h := stDim.Render("models  ·  ↑/↓ choose · enter use · esc cancel · ctrl+a log in to a provider")
	got := ansi.Strip(fitHints(h, 50))
	if got != "models  ·  ↑/↓ choose · enter use · esc cancel" {
		t.Errorf("hints: %q", got)
	}
	d := stDim.Render("restart into the latest build (the launcher rebuilds on source change)")
	got = ansi.Strip(fitText(d, 40))
	if !strings.HasSuffix(got, "…") || ansi.StringWidth(got) > 40 || strings.HasSuffix(got, "rebu…") {
		t.Errorf("text: %q", got)
	}
	if s := "short"; fitText(s, 40) != s || fitHints(s, 40) != s {
		t.Error("a row that fits changed")
	}
}

// Every surface below the conversation fits the window at 80 and 100
// columns: the palette, the login form, the picker, /mcp add, and their
// status lines.
func TestFit_PanelsAtTheWalkWidths(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	for _, w := range []int{80, 100} {
		m := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
		nm, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: 30})
		m = nm.(tuiModel)
		m.engineCmds = engineCmdsFromEvent(evMsg{"items": toAny(commandList(nil, false).Items)})
		check := func(what string, mm tuiModel) {
			for _, row := range strings.Split(mm.View().Content, "\n") {
				if ansi.StringWidth(row) > w {
					t.Errorf("%d cols, %s: row %d wide: %q", w, what, ansi.StringWidth(row), ansi.Strip(row))
				}
			}
		}
		check("palette", typeStr(m, "/"))
		pm := m
		pm.openProviderForm(nil)
		check("login form", pm)
		mm := m
		mm.openModelPicker("")
		mm.modelRows = []modelRow{{"work", "chat-standard"}}
		check("picker", mm)
		fm := m
		fm.openMCPForm()
		fm.mcpFields[mcpServer].SetValue("https://x/mcp")
		check("mcp form", fm)
	}
}

// toAny turns the engine's command list into what the event carries.
func toAny[T any](items []T) []any {
	out := make([]any, 0, len(items))
	for _, it := range items {
		b := must(json.Marshal(it))
		var v map[string]any
		_ = json.Unmarshal(b, &v)
		out = append(out, v)
	}
	return out
}

// The palette lists a new user's commands first, and its descriptions start
// in one column.
func TestPalette_SetupFirstOneColumn(t *testing.T) {
	m := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = nm.(tuiModel)
	m.engineCmds = engineCmdsFromEvent(evMsg{"items": toAny(commandList(nil, false).Items)})
	all := m.allSlash()
	var first []string
	for _, c := range all[:6] {
		first = append(first, c.name)
	}
	if got := strings.Join(first, " "); got != "login model help config logout mcp" {
		t.Errorf("first six: %s", got)
	}
	if last := all[len(all)-1].name; tuiGroup[last] != "maintenance" {
		t.Errorf("last is /%s", last)
	}
	m = typeStr(m, "/")
	col := -1
	for _, row := range strings.Split(stripANSI(m.palettePanel()), "\n") {
		if !strings.HasPrefix(strings.TrimLeft(row, "▎ "), "/") || strings.Contains(row, "commands") {
			continue
		}
		f := strings.Fields(strings.TrimLeft(row, "▎ "))
		name := f[0]
		at := strings.Index(row, name) + len(name)
		for at < len(row) && row[at] == ' ' {
			at++
		}
		at = ansi.StringWidth(row[:at]) // in cells: the bar is one cell, three bytes
		if col < 0 {
			col = at
		} else if at != col {
			t.Errorf("description of %s at col %d, want %d:\n%s", name, at, col, stripANSI(m.palettePanel()))
		}
	}
}

// The /mcp add result replaces its "adding …" line, and a running server is
// said in the success colour, not the notice colour.
func TestServerMsg_AddReplacesProgress(t *testing.T) {
	m := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	m.convo = append(m.convo, convoEntry{collapsed: "adding notes…", adding: "notes"})
	m = m.handleEvent(evMsg{"type": "server", "id": "mcp", "action": "add", "message": "notes added · running · 1 tool"})
	txt := convoText(m)
	if strings.Contains(txt, "adding notes") || strings.Count(txt, "notes added · running · 1 tool") != 1 {
		t.Errorf("convo:\n%s", txt)
	}
	if last := m.convo[len(m.convo)-1].collapsed; last != okNotice("notes added", "running", "1 tool") {
		t.Errorf("styled %q", last)
	}
}
