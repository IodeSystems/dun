package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// startFresh is a first start: no session before, the engine's events in
// the order it sends them.
func startFresh(t *testing.T, sent *bytes.Buffer, resumed int, history []any) tuiModel {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("DUN_HOME", t.TempDir())
	m := newTUIModel(&dunProc{stdin: bufCloser{sent}}, filepath.Join(home, "greet"))
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = nm.(tuiModel)
	m = m.handleEvent(evMsg{"type": "session", "id": "s1", "resumed": float64(resumed)})
	if history != nil {
		m = m.handleEvent(evMsg{"type": "history", "items": history})
	}
	m = m.handleEvent(evMsg{"type": "ready", "tools": []any{"eval", "read"},
		"servers": []any{
			map[string]any{"id": "shell", "running": true, "tools": float64(2)},
			map[string]any{"id": "raglit", "slash": "raglit"},
			map[string]any{"id": "poly-lsp", "slash": "poly-lsp"},
		}})
	return m.handleEvent(evMsg{"type": "settings", "model": "", "url": ""})
}

// A fresh session opens with the splash: what a person acts on — the folder
// (~), the model, the servers, and the optional extras in plain words —
// above the first-run line. No build, no wordmark, no tool count (/status).
func TestSplash_FreshSessionOnly(t *testing.T) {
	var sent bytes.Buffer
	m := startFresh(t, &sent, 0, nil)
	if len(m.convo) < 2 || !m.convo[0].splash {
		t.Fatalf("no splash first:\n%s", convoText(m))
	}
	s := stripANSI(m.convo[0].collapsed)
	for _, want := range []string{"  folder         ~/greet\n", "  model          none yet\n",
		"  servers        shell running · 2 tools · poly-lsp off\n", "  extensions     raglit off · /help raglit\n",
		"  skills         none\n", "  context files  counting…\n", "  context        counting…\n", "  spend          none yet\n"} {
		if !strings.Contains(s, want) {
			t.Errorf("splash lacks %q:\n%s", want, s)
		}
	}
	for _, not := range []string{"dun", version, "tools  ", "raglit on", "auto"} {
		if strings.Contains(s, not) {
			t.Errorf("splash has %q:\n%s", not, s)
		}
	}
	if n := strings.Count(s, "\n"); n != splashRows {
		t.Errorf("splash is %d rows, want %d:\n%s", n, splashRows, s)
	}
	if !strings.Contains(stripANSI(m.convo[len(m.convo)-1].collapsed), "not logged in") {
		t.Errorf("the first-run line is not after the splash:\n%s", convoText(m))
	}
	if strings.Contains(sent.String(), "dun "+version) {
		t.Error("the splash went to the engine")
	}

	// A resumed session: its history is the top of the screen.
	var sent2 bytes.Buffer
	r := startFresh(t, &sent2, 2, []any{map[string]any{"kind": "user", "content": "earlier"}})
	for _, e := range r.convo {
		if e.splash {
			t.Fatalf("a resumed session got a splash:\n%s", convoText(r))
		}
	}
}

// A model pick restarts the engine under the conversation: the splash stays
// at the top, now with the model.
func TestSplash_KeptThroughAModelPick(t *testing.T) {
	var sent bytes.Buffer
	m := startFresh(t, &sent, 0, nil)
	m.switchModel(providerConfig{Name: "work", URL: "http://llm/v1"}, "chat-standard", true)
	m = m.handleEvent(evMsg{"type": "session", "id": "s1", "resumed": float64(0)})
	m = m.handleEvent(evMsg{"type": "ready", "tools": []any{"eval"}})
	m = m.handleEvent(evMsg{"type": "settings", "model": "chat-standard", "url": "http://llm/v1", "provider": "work"})
	if len(m.convo) == 0 || !m.convo[0].splash {
		t.Fatalf("splash gone after the pick:\n%s", convoText(m))
	}
	if s := stripANSI(m.convo[0].collapsed); !strings.Contains(s, "model          chat-standard on work") {
		t.Errorf("splash model:\n%s", s)
	}
	n := 0
	for _, e := range m.convo {
		if e.splash {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d splashes:\n%s", n, convoText(m))
	}
}

// /status writes the splash at the end of the conversation, client-side:
// nothing goes to the engine (so nothing is in the session log or sent to
// the model).
func TestStatus_WritesTheSplashClientSide(t *testing.T) {
	var sent bytes.Buffer
	m := startFresh(t, &sent, 0, nil)
	before := sent.Len()
	m = typeStr(m, "/status")
	m = key(m, kEnter)
	last := m.convo[len(m.convo)-1]
	if !last.splash || !strings.Contains(stripANSI(last.collapsed), "build          "+version) || !strings.Contains(stripANSI(last.collapsed), "tools          2") {
		t.Fatalf("/status wrote:\n%s", convoText(m))
	}
	if got := sent.String()[before:]; strings.Contains(got, "status") || strings.Contains(got, `"type":"user"`) {
		t.Errorf("/status reached the engine: %s", got)
	}
	if tuiGroup["status"] != "session" {
		t.Error("/status is not in the session group")
	}
}

// The header is the folder and the model: no build, no path, no tool count.
func TestHeader_FolderAndModelOnly(t *testing.T) {
	m := newTUIModel(&dunProc{stdin: discardWC{}}, "/home/sam/src/greet")
	m.model, m.url = "chat-standard", "http://llm/v1"
	m.tools = []string{"a", "b"}
	if h := stripANSI(m.headView()); h != "dun · greet · chat-standard" {
		t.Errorf("header %q", h)
	}
}

// The divider keeps the tmux half-line: the focused side's half is the
// heavy rule in the accent, the other the light rule, dim. The activity
// strip is above the divider, so it lights the upper half too.
func TestDivider_HeavyHalfOnTheFocusedSide(t *testing.T) {
	up := divider(10, true)
	if got := ansi.Strip(up); got != "━━━━━─────" {
		t.Errorf("up: %q", got)
	}
	if !strings.HasPrefix(up, stEdge.Render("━━━━━")) || !strings.HasSuffix(up, stDim.Render("─────")) {
		t.Errorf("up styles: %q", up)
	}
	if got := ansi.Strip(divider(10, false)); got != "─────━━━━━" {
		t.Errorf("down: %q", got)
	}
	m := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 20, Height: 10})
	m = nm.(tuiModel)
	m.focus = focusActivity
	if !strings.Contains(ansi.Strip(m.drawView()), "━━━━━━━━━━──────────") {
		t.Errorf("activity focus does not light the upper half:\n%s", ansi.Strip(m.drawView()))
	}
}

// /model <name> typed saves the default, as enter in the picker does;
// --session is for this session only, as alt+enter is.
func TestModelTyped_SavesTheDefault(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	cfg := loadConfig()
	cfg.putProvider(providerConfig{Name: "work", URL: "http://llm/v1"})
	if err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	m := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	m.provider, m.url = "work", "http://llm/v1"
	modelSlash(&m, []string{"chat-standard"})
	if c := loadConfig(); c.Model != "chat-standard" || c.Provider != "work" {
		t.Errorf("typed /model did not save: %+v", c)
	}
	if m.switchNote != "using chat-standard on work · saved as default" {
		t.Errorf("note %q", m.switchNote)
	}
	modelSlash(&m, []string{"work", "chat-fast", "--session"})
	if c := loadConfig(); c.Model != "chat-standard" {
		t.Errorf("--session changed the default to %q", c.Model)
	}
	if m.switchNote != "using chat-fast on work · this session only" {
		t.Errorf("note %q", m.switchNote)
	}
}

// The model list: chat models first, the rest after and labelled; the
// cursor on the saved default; no count when all rows fit; one indent for
// the search line and the rows.
func TestModelList_ChatFirstCursorOnDefault(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	cfg := loadConfig()
	cfg.putProvider(providerConfig{Name: "work", URL: "http://llm/v1"})
	cfg.use(providerConfig{Name: "work", URL: "http://llm/v1"}, "chat-standard")
	if err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	m := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	m.openModelPicker("")
	m.addModelRows(modelFetchMsg{provider: "work", ids: []string{"embed-small", "chat-fast", "vision-standard", "chat-standard"}})
	var ids []string
	for _, r := range m.modelRows {
		ids = append(ids, r.id)
	}
	if got := strings.Join(ids, " "); got != "chat-fast chat-standard embed-small vision-standard" {
		t.Errorf("order: %s", got)
	}
	if r, _ := m.selectedModel(); r.id != "chat-standard" {
		t.Errorf("cursor on %q, want the saved default", r.id)
	}
	panel := stripANSI(m.modelPanel())
	if strings.Contains(panel, " of ") {
		t.Errorf("a count with everything shown:\n%s", panel)
	}
	rows := strings.Split(panel, "\n")
	if !strings.HasPrefix(rows[1], "  / ") {
		t.Errorf("search line %q", rows[1])
	}
	for _, r := range rows[2:] {
		if r == "" {
			continue
		}
		body, ok := strings.CutPrefix(r, "▎ ")
		if !ok {
			body, ok = strings.CutPrefix(r, "  ")
		}
		if !ok || strings.HasPrefix(body, " ") {
			t.Errorf("row not at the one indent: %q", r)
		}
	}
	if !strings.Contains(panel, "embed-small      embeddings") || !strings.Contains(panel, "vision-standard  images") {
		t.Errorf("kinds not labelled:\n%s", panel)
	}

	// No default: the first chat model.
	t.Setenv("DUN_HOME", t.TempDir())
	n := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	n.openModelPicker("")
	n.addModelRows(modelFetchMsg{provider: "work", ids: []string{"embed-small", "chat-fast"}})
	if r, _ := n.selectedModel(); r.id != "chat-fast" {
		t.Errorf("cursor on %q, want the first chat model", r.id)
	}
}

// Command names in the palette are neutral, not the success green; /mcp
// and /ext fit at 100 columns.
func TestPalette_NeutralNamesNoClipAt100(t *testing.T) {
	m := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	m = nm.(tuiModel)
	m.engineCmds = engineCmdsFromEvent(evMsg{"items": toAny(commandList(nil, false).Items)})
	m = typeStr(m, "/")
	panel := m.palettePanel()
	if strings.Contains(panel, stTool.Render("/model")) {
		t.Error("command names are green")
	}
	for _, row := range strings.Split(stripANSI(panel), "\n") {
		if (strings.Contains(row, "/mcp ") || strings.Contains(row, "/ext ")) && strings.Contains(row, "…") {
			t.Errorf("clipped at 100: %q", row)
		}
	}
}

// The login form and /mcp add: fields, a blank row, then the notes; both
// status lines offer shift+tab back.
func TestForms_SameShape(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	m := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	m.openProviderForm(nil)
	f := m
	f.openMCPForm()
	for name, p := range map[string]string{"login": stripANSI(m.providerFormPanel()), "mcp": stripANSI(f.mcpFormPanel())} {
		rows := strings.Split(p, "\n")
		blank := -1
		for i, r := range rows {
			if r == "" {
				blank = i
				break
			}
		}
		if blank < 0 || !strings.Contains(strings.Join(rows[blank:], "\n"), "saved to") || strings.Contains(strings.Join(rows[:blank], "\n"), "presets") {
			t.Errorf("%s form shape:\n%s", name, p)
		}
	}
	if h := m.providerHint(); !strings.Contains(h, "shift+tab back") {
		t.Errorf("login keys %q", h)
	}
	if h := f.mcpHint(); !strings.Contains(h, "shift+tab back") {
		t.Errorf("mcp keys %q", h)
	}
}
