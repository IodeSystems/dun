package main

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
)

// Round 6 of the onboarding walk (plan item 41).

// onPath makes these commands, and only these, resolve on PATH for the
// /mcp add form.
func onPath(t *testing.T, names ...string) {
	t.Helper()
	old := mcpLookPath
	mcpLookPath = func(name string) (string, error) {
		if slices.Contains(names, name) {
			return "/usr/bin/" + name, nil
		}
		return "", errors.New("not found")
	}
	t.Cleanup(func() { mcpLookPath = old })
}

// finishLogin saves the login form and hands it the service's answer (one
// model, chat-standard), as the check saveProvider starts would.
func finishLogin(m tuiModel) tuiModel {
	m.saveProvider()
	m.loginChecked(loginCheckedMsg{
		models: modelFetchMsg{provider: m.providerChecking, ids: []string{"chat-standard"}}})
	return m
}

// withMascot draws the foreman for the rest of the test.
func withMascot(t *testing.T) {
	t.Helper()
	mascotColour = true
	t.Cleanup(func() { mascotColour = false })
}

// ── 1. the foreman ──

// The grid renders at its fixed size, 13 cells × 6 rows; ▀ with
// the top pixel's colour on the bottom's, ▄ or a space where a half is the
// terminal's own background, and every row ends reset.
func TestMascot_FixedSizeHalfBlocks(t *testing.T) {
	for _, tc := range []struct {
		grid       []string
		w, h, rows int
	}{{mascot, 13, 11, 6}} {
		if mascotWidth(tc.grid) != tc.w || len(tc.grid) != tc.h {
			t.Fatalf("grid %d×%d, want %d×%d", mascotWidth(tc.grid), len(tc.grid), tc.w, tc.h)
		}
		art := mascotArt(tc.grid)
		if len(art) != tc.rows {
			t.Fatalf("%d rows, want %d", len(art), tc.rows)
		}
		for i, r := range art {
			if ansi.StringWidth(r) != tc.w || !strings.HasSuffix(r, "\x1b[0m") {
				t.Errorf("row %d: %d cells, want %d, reset at the end: %q", i, ansi.StringWidth(r), tc.w, r)
			}
		}
	}
	big := mascotArt(mascot)
	// Row 0, cell 4: 'y' over 'Y' — the brim's colour on the hat's.
	if !strings.Contains(big[0], "\x1b[0;38;5;172;48;5;220m▀") {
		t.Errorf("both halves: %q", big[0])
	}
	// Row 0, cell 3: transparent over 'Y' — ▄ in the hat's colour, no background.
	if !strings.Contains(big[0], "\x1b[0;38;5;220m▄") {
		t.Errorf("bottom half only: %q", big[0])
	}
	// The small grid's odd last pixel row: ▀ over the terminal's background.
	if !strings.Contains(mascotArt(mascot)[5], "\x1b[0;38;5;26m▀") {
		t.Errorf("odd row: %q", mascotArt(mascot)[5])
	}
}

// Only a terminal with 256 colours or more draws him.
func TestMascot_ColourProfile(t *testing.T) {
	for p, want := range map[colorprofile.Profile]bool{colorprofile.TrueColor: true, colorprofile.ANSI256: true,
		colorprofile.ANSI: false, colorprofile.Ascii: false, colorprofile.NoTTY: false} {
		if mascotProfileOK(p) != want {
			t.Errorf("profile %v: %v", p, !want)
		}
	}
}

// The fresh session's splash: the foreman at every width, the long tagline
// at 120 columns and more, beside the rows (which keep their words), the tagline
// first; the splash is never shorter than he is, and no row is wider than
// the pane.
func TestSplash6_Mascot(t *testing.T) {
	withMascot(t)
	for _, tc := range []struct {
		w       int
		art     []string
		tagline string
	}{
		{180, mascotArt(mascot), "dun · " + taglineLong},
		{120, mascotArt(mascot), "dun · " + taglineLong},
		{100, mascotArt(mascot), "dun · ain't pretty. gets it dun."},
		{80, mascotArt(mascot), "dun · ain't pretty. gets it dun."},
		{49, mascotArt(mascot), "dun · ain't pretty. gets it dun."},
	} {
		v := splash5(t, tc.w, 30)
		v.event(costEvent(false, 32000))
		raw := strings.Split(strings.TrimSuffix(v.m.convo[0].collapsed, "\n"), "\n")
		if len(raw) < len(tc.art) {
			t.Fatalf("%d: %d rows, shorter than the foreman's %d", tc.w, len(raw), len(tc.art))
		}
		for i, a := range tc.art {
			if !strings.HasPrefix(raw[i], a) {
				t.Errorf("%d: row %d does not start with the foreman's row", tc.w, i)
			}
		}
		rows := strings.Split(splashText(v), "\n")
		if !strings.Contains(rows[0], tc.tagline) {
			t.Errorf("%d: tagline row %q", tc.w, rows[0])
		}
		for _, want := range []string{"folder", "model", "tools", "context"} {
			if !strings.Contains(splashText(v), want) {
				t.Errorf("%d: the rows lost %q:\n%s", tc.w, want, splashText(v))
			}
		}
		for i, r := range raw {
			if ansi.StringWidth(r) > v.m.splashW() {
				t.Errorf("%d: row %d is %d wide, pane %d", tc.w, i, ansi.StringWidth(r), v.m.splashW())
			}
		}
	}
}

// Without 256 colours: no foreman, the one line.
func TestSplash6_PlainLine(t *testing.T) {
	v := splash5(t, 100, 30)
	s := splashText(v)
	if !strings.HasPrefix(s, "  dun · git 'er dun\n") || strings.ContainsAny(s, "▀▄") {
		t.Errorf("plain:\n%s", s)
	}
}

// Not on /status, and not on a resumed session (no splash there at all).
func TestSplash6_MascotNotOnStatusOrResume(t *testing.T) {
	withMascot(t)
	v := splash5(t, 180, 50)
	v.m.statusSlash()
	if st := v.m.convo[len(v.m.convo)-1].collapsed; strings.ContainsAny(st, "▀▄") || strings.Contains(st, "git 'er") {
		t.Errorf("/status has the foreman:\n%s", stripANSI(st))
	}
	var sent bytes.Buffer
	m := startFresh(t, &sent, 3, []any{map[string]any{"kind": "user", "content": "earlier"}})
	if txt := convoText(m); strings.ContainsAny(txt, "▀▄") || strings.Contains(txt, "ain't") {
		t.Errorf("a resumed session has the foreman:\n%s", stripANSI(txt))
	}
}

// ── 4. the context row ──

func TestTokN_OnePrecision(t *testing.T) {
	for n, want := range map[int]string{0: "0", 344: "344", 999: "999", 1000: "1.0k", 1100: "1.1k",
		18400: "18.4k", 128300: "128.3k", 2_500_000: "2.50M"} {
		if got := tokN(n); got != want {
			t.Errorf("tokN(%d) = %q, want %q", n, got, want)
		}
	}
}

// Every part is its count and its tokens, zeros included, never "none".
func TestSplash6_ContextPartsWithZeros(t *testing.T) {
	v := splash5(t, 180, 50)
	v.event(map[string]any{"type": "context_cost", "system_tokens": float64(5900), "system_prompt": float64(1100),
		"system_parts": []any{map[string]any{"name": "built-in tools", "tokens": float64(4800)}},
		"window":       float64(32000)})
	s := splashText(v)
	for _, want := range []string{
		"context     ~5.9k tokens before you type (of 32k, 18%) · /context config\n",
		"system 1.1k · tools 0 (4.8k) · mcp 0 servers (0) · skills 0 (0) · rules 0 (0)\n",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("lacks %q:\n%s", want, s)
		}
	}
}

// ── 6. the servers row, the spend row, the header ──

// The mcp row counts the servers and names the one still starting.
func TestSplash6_ServersKeepTheirPlaces(t *testing.T) {
	v := splash5(t, 100, 30)
	v.event(map[string]any{"type": "server", "servers": []any{
		map[string]any{"id": "notes", "running": true, "tools": float64(1)},
		map[string]any{"id": "shell", "running": true, "tools": float64(1)},
		map[string]any{"id": "raglit", "slash": "raglit"},
		map[string]any{"id": "poly-lsp", "slash": "poly-lsp", "starting": true},
	}})
	if s := splashText(v); !strings.Contains(s, "tools       1 · poly-lsp starting") ||
		!strings.Contains(s, " · /mcp info\n") {
		t.Errorf("order:\n%s", s)
	}
}

// The header leaves the model out while the splash's model row is on
// screen, and says it once the splash has scrolled away.
func TestHeader6_ModelOnceOnScreen(t *testing.T) {
	v := splash5(t, 100, 30)
	if head := strings.Split(v.view(), "\n")[0]; strings.Contains(head, "chat-standard") {
		t.Errorf("header with the splash in view: %q", head)
	}
	for range 40 {
		v.m.append("line")
	}
	v.m.refresh()
	v.m.vp.GotoBottom()
	if head := strings.Split(v.view(), "\n")[0]; !strings.Contains(head, "chat-standard") {
		t.Errorf("header with the splash scrolled away: %q", head)
	}
}

// ── 3. /login ──

// The form asks for the address and the key; the name is filled in from
// the address and said in the note. "replaces" only for a name in use.
func TestLogin6_TwoFieldsNameInTheNote(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	m := newTUIModel(nil, ".")
	m.openProviderForm(nil)
	m = pick(m, "https://api.example.com/v1")
	p := stripANSI(m.providerFormPanel())
	if strings.Contains(p, "  name ") || !strings.Contains(p, "address") || !strings.Contains(p, "key") {
		t.Errorf("rows:\n%s", p)
	}
	if !strings.Contains(p, "as example · /login rename changes it") || strings.Contains(p, "replaces") {
		t.Errorf("note:\n%s", p)
	}
	m = pick(m, "enter")
	if m.providerStep != fieldKey {
		t.Fatalf("enter on the address goes to the key, at %d", m.providerStep)
	}
	m = finishLogin(pick(m, "sk-1", "enter"))
	if c, _ := loadConfig().named("example"); c.Key != "sk-1" {
		t.Fatalf("saved %+v", c)
	}
	m.openProviderForm(nil)
	m = pick(m, "https://api.example.com/v1")
	if p := stripANSI(m.providerFormPanel()); !strings.Contains(p, "as example — replaces the example saved before") {
		t.Errorf("the same address again:\n%s", p)
	}
	// Another address that makes the same name is numbered, never saved
	// over the first (a clean first run: localhost and 127.0.0.1 were both
	// "local", and the second login replaced the first).
	m.openProviderForm(nil)
	m = pick(m, "https://api.example.com/v2")
	if p := stripANSI(m.providerFormPanel()); !strings.Contains(p, "as example-2 · /login rename changes it") {
		t.Errorf("a name in use at another address:\n%s", p)
	}
}

// /login rename <old> <new> renames the saved service, in place, the
// default with it.
func TestLogin6_Rename(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	m := newTUIModel(nil, ".")
	m.openProviderForm(nil)
	m = finishLogin(pick(m, "http://127.0.0.1:4000/v1", "enter", "enter"))
	m.provider = "local"
	loginSlash(&m, []string{"rename", "local", "work"})
	c := loadConfig()
	if _, ok := c.named("work"); !ok || c.activeName() != "work" || m.provider != "work" {
		t.Errorf("renamed: %+v active %q provider %q", c.providerList(), c.activeName(), m.provider)
	}
	if !strings.Contains(stripANSI(convoText(m)), "renamed local to work") {
		t.Errorf("no notice:\n%s", stripANSI(convoText(m)))
	}
	loginSlash(&m, []string{"rename", "nope", "x"})
	if !strings.Contains(stripANSI(convoText(m)), "not logged in to nope") {
		t.Errorf("unknown name:\n%s", stripANSI(convoText(m)))
	}
}

// Not logged in: enter on an empty composer opens the login form, and the
// status line says so; with words typed, enter sends (holds) them.
func TestLogin6_EnterOnEmptyOpensTheForm(t *testing.T) {
	var sent bytes.Buffer
	m := startFresh(t, &sent, 0, nil)
	if got := lastRow(m); !strings.HasPrefix(got, "enter log in · / commands") {
		t.Errorf("status: %q", got)
	}
	m = key(m, kEnter)
	if !m.loginPicking {
		t.Fatal("enter on an empty composer did not open the login")
	}
	m = key(m, kEsc)
	m = typeStr(m, "hi")
	if got := lastRow(m); !strings.HasPrefix(got, "enter send") {
		t.Errorf("status with words: %q", got)
	}
}

// Enter on the not-logged-in line (selected in the conversation) opens it.
func TestLogin6_EnterOnTheLineOpensTheForm(t *testing.T) {
	var sent bytes.Buffer
	m := startFresh(t, &sent, 0, nil)
	m.setFocus(focusConvo)
	m.sel = len(m.convo) - 1
	if !m.convo[m.sel].needLogin {
		t.Fatalf("last entry is not the line: %q", stripANSI(m.convo[m.sel].collapsed))
	}
	m = key(m, kEnter)
	if !m.loginPicking || m.focus != focusInput {
		t.Fatalf("login %v focus %d", m.loginPicking, m.focus)
	}
}

// The kept question's line is an instruction: the attention colour, the
// warning mark. It goes when the login form opens, and esc brings it back.
func TestLogin6_KeptLineGoesWhenLoginStarts(t *testing.T) {
	var sent bytes.Buffer
	m := startFresh(t, &sent, 0, nil)
	m = typeStr(m, "Can you help?")
	m = key(m, kEnter)
	if last := m.convo[len(m.convo)-1].collapsed; last != warnLine(heldLine) {
		t.Fatalf("kept line %q", last)
	}
	m = typeStr(m, "/login")
	m = key(m, kEnter)
	if !m.loginPicking || strings.Contains(stripANSI(convoText(m)), "kept your question") {
		t.Fatalf("login %v, convo:\n%s", m.loginPicking, stripANSI(convoText(m)))
	}
	m = key(m, kEsc)
	if !strings.HasSuffix(stripANSI(convoText(m)), "⚠ "+heldLine) {
		t.Errorf("esc did not bring it back:\n%s", stripANSI(convoText(m)))
	}
}

// On a phone, the login notice is followed when the picker opens: both its
// rows are in view (run 5: "· key set" was under the picker).
func TestLogin6_PhoneNoticeNotUnderThePicker(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	v := newVtui(49, 21)
	v.event(map[string]any{"type": "session", "id": "s1", "resumed": float64(0)})
	v.event(map[string]any{"type": "ready", "tools": []any{"eval"}, "servers": []any{
		map[string]any{"id": "shell", "running": true, "tools": float64(1)},
		map[string]any{"id": "raglit", "slash": "raglit"},
		map[string]any{"id": "poly-lsp", "slash": "poly-lsp"},
	}})
	v.event(map[string]any{"type": "settings", "model": "", "url": ""})
	v.m = typeStr(v.m, "Can you help me with this project?")
	v.m = key(v.m, kEnter)
	v.m = typeStr(v.m, "/login")
	v.m = key(v.m, kEnter)
	v.m = pick(v.m, "enter", "http://127.0.0.1:39155/v1", "enter", "sk-test", "enter") // custom, first
	nm, _ := v.m.Update(loginCheckedMsg{models: modelFetchMsg{provider: v.m.providerChecking,
		ids: []string{"chat-fast", "chat-large", "chat-standard", "code-large", "code-standard", "embed-small", "vision-standard"}}})
	v.m = nm.(tuiModel)
	if !v.m.modelPicking || v.m.providerForm {
		t.Fatalf("picker %v form %v", v.m.modelPicking, v.m.providerForm)
	}
	view := strings.Join(shown(v), " ")
	if !strings.Contains(view, "logged in to local") || !strings.Contains(view, "key set") {
		t.Errorf("the notice is not all in view:\n%s", strings.Join(shown(v), "\n"))
	}
	if got, ok := v.m.selectedModel(); !ok || got.id != "chat-standard" {
		t.Errorf("the picker starts on %q, want chat-standard", got.id)
	}
}

// ── placeholders, the palette's gap, the picker's first row ──

// An empty focused field's cursor sits before its placeholder, not on its
// first letter.
func TestPlaceholder6_CursorBeforeTheText(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	m := newTUIModel(nil, ".")
	m.openProviderForm(nil)
	if v := stripANSI(m.providerFields[fieldAddr].View()); !strings.Contains(v, " e.g. https://openrouter.ai/api/v1") {
		t.Errorf("address field: %q", v)
	}
	m.openModelPicker("")
	if v := stripANSI(m.models.query.View()); !strings.Contains(v, "/  type to search") {
		t.Errorf("model search: %q", v)
	}
	// The composer: the caret on the blank cell, the placeholder after it,
	// in the same column focused or not.
	in := newMultilineInput()
	in.Focus()
	focused := in.View()
	if !strings.HasPrefix(focused, "› "+stCaret.Render(" ")) || !strings.HasPrefix(stripANSI(focused), "›  ask dun") {
		t.Errorf("composer: %q", focused)
	}
	in.Blur()
	if got := stripANSI(in.View()); !strings.HasPrefix(got, "›  ask dun") || stripANSI(focused) != got {
		t.Errorf("composer unfocused %q, focused %q", got, stripANSI(focused))
	}
}

// The command list's count line has a blank row above it.
func TestPalette6_GapAboveTheCount(t *testing.T) {
	v := newVtui(100, 30)
	v.m = typeStr(v.m, "/")
	rows := strings.Split(stripANSI(v.m.palettePanel()), "\n")
	for i, r := range rows {
		if strings.Contains(r, " of ") && strings.Contains(r, "commands") {
			if i == 0 || strings.TrimSpace(rows[i-1]) != "" {
				t.Errorf("no gap above the count:\n%s", strings.Join(rows, "\n"))
			}
			return
		}
	}
	t.Errorf("no count line:\n%s", strings.Join(rows, "\n"))
}

// With nothing running or saved, the picker starts on the default, else
// the standard, else a chat model; else the first row.
func TestSuggestedModel(t *testing.T) {
	rows := func(ids ...string) []modelRow {
		out := make([]modelRow, len(ids))
		for i, id := range ids {
			out[i] = modelRow{"p", id}
		}
		return out
	}
	for _, tc := range []struct {
		ids  []string
		want int
	}{
		{[]string{"chat-fast", "chat-large", "chat-standard"}, 2},
		{[]string{"a", "default-model", "standard"}, 1},
		{[]string{"embed-standard", "gpt-chat", "x"}, 1},
		{[]string{"x", "y"}, 0},
	} {
		if got := suggestedModel(rows(tc.ids...)); got != tc.want {
			t.Errorf("%v: %d, want %d", tc.ids, got, tc.want)
		}
	}
}

// ── 2. /mcp add: one field ──

func TestMCPNameFrom(t *testing.T) {
	for in, want := range map[string]string{
		"notes-mcp":         "notes",
		"mcp-notes --stdio": "notes",
		"/opt/bin/jira_mcp": "jira",
		"npx -y @modelcontextprotocol/server-filesystem /tmp": "filesystem",
		"npx @playwright/mcp@latest --headless":               "playwright",
		"uvx mcp-server-git":                                  "git",
		"https://mcp.linear.app/sse":                          "linear",
		"http://127.0.0.1:3845/mcp":                           "local",
		"python3 notes_server.py":                             "notes",
		"":                                                    "",
	} {
		if got := mcpNameFrom(in); got != want {
			t.Errorf("mcpNameFrom(%q) = %q, want %q", in, got, want)
		}
	}
}

// The one field takes a command on PATH, an address or a preset; the name
// is filled in below it; a word that is none of them is said in place.
func TestMCPForm6_OneField(t *testing.T) {
	onPath(t, "notes-mcp")
	var buf bytes.Buffer
	m := newTUIModel(&dunProc{stdin: bufCloser{&buf}}, "/ws")
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = nm.(tuiModel)
	m.openMCPForm()
	p := stripANSI(m.mcpFormPanel())
	if !strings.Contains(p, "server   >  what you were given — a command, an address or a preset") ||
		strings.Contains(p, "replaces") {
		t.Errorf("first sight:\n%s", p)
	}
	// Not a command here.
	m = typeStr(m, "notes")
	m = key(m, kEnter)
	if !m.mcpForm || m.mcpErr != "notes isn't a command here — paste the full command or address" {
		t.Fatalf("form %v err %q", m.mcpForm, m.mcpErr)
	}
	m = typeStr(m, "-mcp")
	if m.mcpErr != "" || m.mcpFields[mcpID].Value() != "notes" {
		t.Fatalf("err %q name %q", m.mcpErr, m.mcpFields[mcpID].Value())
	}
	m = key(m, kEnter)
	if m.mcpForm || !strings.Contains(buf.String(), `\"id\":\"notes\"`) || !strings.Contains(buf.String(), `\"command\":\"notes-mcp\"`) {
		t.Fatalf("form %v sent %s", m.mcpForm, buf.String())
	}
	// An address; the name is changed by hand on the row below.
	m.openMCPForm()
	m = typeStr(m, "https://mcp.linear.app/sse")
	m = key(m, kTab)
	if m.mcpStep != mcpID || m.mcpFields[mcpID].Value() != "linear" {
		t.Fatalf("step %d name %q", m.mcpStep, m.mcpFields[mcpID].Value())
	}
	// A preset that is also a command here: said, not guessed.
	onPath(t, "figma")
	m.openMCPForm()
	m = typeStr(m, "figma")
	m = key(m, kEnter)
	if !strings.Contains(m.mcpErr, "figma is a preset and a command here") {
		t.Errorf("ambiguous: %q", m.mcpErr)
	}
	// A name in use: the note says it replaces it.
	m.serverIDs = []string{"notes"}
	m.openMCPForm()
	onPath(t, "notes-mcp")
	m = typeStr(m, "notes-mcp")
	if p := stripANSI(m.mcpFormPanel()); !strings.Contains(p, "replaces the notes there now") {
		t.Errorf("in use:\n%s", p)
	}
}

// ── 5. starting session ──

// Past three seconds the status line names what the session waits on:
// dund, the session, and an older dund's build with the command that
// replaces it.
func TestStarting6_NamesWhatItWaitsFor(t *testing.T) {
	t.Cleanup(func() { attachNow.Store(nil) })
	m := newTUIModel(nil, "/ws")
	m.w = 100
	m.startingStart = time.Now()
	attachNow.Store(&attachWait{})
	if got := m.startingWhat(); got != "starting session…" {
		t.Errorf("before 3 s: %q", got)
	}
	m.startingStart = time.Now().Add(-4 * time.Second)
	if got := m.startingWhat(); got != "waiting for dund, the session service…" {
		t.Errorf("dialing: %q", got)
	}
	attachNow.Store(&attachWait{session: true})
	if got := m.startingWhat(); got != "waiting for the session · dund is starting its engine…" {
		t.Errorf("creating: %q", got)
	}
	attachNow.Store(&attachWait{session: true, dundBuild: "5f5237b", fix: "dun dund service restart"})
	if got := m.startingWhat(); got != "waiting for the session · dund is an older build (5f5237b) — dun dund service restart" {
		t.Errorf("older dund: %q", got)
	}
	m.w = 49
	if got := m.startingWhat(); got != "old dund — dun dund service restart" {
		t.Errorf("older dund at 49: %q", got)
	}
}
