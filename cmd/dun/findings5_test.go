package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// Run 4 of the walk, findings 4–8 (plan item 40).

// /mcp add is its own row near the top of the command list, in plain
// words; the list has a heading like every panel; at 49 columns a
// description ends at a clause, not mid-sentence.
func TestPalette5_MCPAddHeadingAndClauses(t *testing.T) {
	m := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 49, Height: 21})
	m = nm.(tuiModel)
	m.engineCmds = engineCmdsFromEvent(evMsg{"items": toAny(commandList(nil, false).Items)})
	m = typeStr(m, "/")
	rows := strings.Split(stripANSI(m.palettePanel()), "\n")
	if rows[0] != "commands" {
		t.Errorf("no heading: %q", rows[0])
	}
	want := map[string]string{
		"/login":   "connect to an AI service",
		"/model":   "pick the model",
		"/mcp add": "connect a server you were given",
		"/help":    "list these commands",
	}
	for _, r := range rows[1:5] {
		r = strings.TrimLeft(r, "▎ ")
		ok := false
		for name, desc := range want {
			if strings.HasPrefix(r, name+" ") && strings.Contains(r, "  "+desc) && !strings.Contains(r, "…") {
				ok = true
			}
		}
		if !ok {
			t.Errorf("row %q: not one of the first four, ending at a clause", r)
		}
	}
	for _, r := range rows {
		if ansi.StringWidth(r) > 49 {
			t.Errorf("wider than 49: %q", r)
		}
	}
	// /mcp and enter is /mcp; the /mcp add row, chosen, opens the form.
	if got := fitClause("tool servers (MCP): see them and turn them on or off; /mcp add adds one", 30); got != "tool servers (MCP)" {
		t.Errorf("fitClause: %q", got)
	}
}

// A narrow status line drops hints from its end, but never the way out:
// esc cancel in a form, ctrl+c quit when ready.
func TestHints5_TheWayOutStays(t *testing.T) {
	for _, tc := range []struct{ row, want string }{
		{formKeys("add server", "enter next", "tab more options"), "esc cancel"},
		{"ready  ·  enter send · alt+enter newline · ↑ history · ctrl+c quit", "ctrl+c quit"},
		{"command  ·  ↑/↓ select · enter run · tab complete · esc close", "esc close"},
	} {
		got := ansi.Strip(fitHints(stDim.Render(tc.row), 40))
		if !strings.Contains(got, tc.want) || ansi.StringWidth(got) > 40 {
			t.Errorf("%q at 40: %q", tc.row, got)
		}
	}
	_, hints, _ := strings.Cut("ready  ·  enter send · alt+enter newline · ↑ history · ctrl+c quit", "  ·  ")
	if n := strings.Count(hints, hintSep); n+1 > 4 {
		t.Errorf("ready has %d hints", n+1)
	}
}

// The warning line has its mark in column 0, as a line that worked has ✓;
// the kept-question line is at the body's indent in the secondary grey and
// says what to type first.
func TestLines5_WarningMarkAndKeptQuestion(t *testing.T) {
	if got := warnLine(needLoginLine); got != stNote.Render("⚠ not logged in — type /login to connect") {
		t.Errorf("warn line %q", got)
	}
	// A line that wraps on a phone continues under its text, not its mark.
	if got := ansi.Strip(hangWrap("  "+heldLine, 49)); got != "  kept your question — type /login, then it comes\n  back" {
		t.Errorf("hang wrap %q", got)
	}
	t.Setenv("DUN_HOME", t.TempDir())
	m := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = nm.(tuiModel)
	m = m.handleEvent(evMsg{"type": "ready", "tools": []any{"eval"}})
	m = m.handleEvent(evMsg{"type": "settings", "model": "", "url": ""})
	if !strings.Contains(stripANSI(convoText(m)), "\n⚠ not logged in") && !strings.HasPrefix(stripANSI(convoText(m)), "⚠ not logged in") {
		t.Errorf("no mark:\n%s", stripANSI(convoText(m)))
	}
	m = typeStr(m, "hello")
	m = key(m, kEnter)
	last := m.convo[len(m.convo)-1].collapsed
	if last != warnLine("kept your question — type /login, then it comes back") {
		t.Errorf("kept line %q", last)
	}
}

// The tool row: only ✓ is green; the call's words are neutral and the
// result secondary, so the row does not outrank the answer.
func TestToolRow5_OnlyTheMarkIsGreen(t *testing.T) {
	v := newVtui(100, 30)
	v.event(map[string]any{"type": "ready", "tools": []any{"notes__project_notes"}})
	v.send("What do the team notes say?")
	v.event(map[string]any{"type": "tool_call", "tool": "notes__project_notes", "args": map[string]any{}})
	v.event(map[string]any{"type": "tool_result", "tool": "notes__project_notes", "result": "Notes: keep the greeting short."})
	var row string
	for _, e := range v.m.convo {
		if e.run != nil {
			row = renderBox(&e, 99, 30)
		}
	}
	if !strings.HasPrefix(row, stTool.Render("✓ ")+"notes · project notes") {
		t.Errorf("tool row %q", row)
	}
	if strings.Count(row, "38;5;42m") != 1 {
		t.Errorf("green more than once: %q", row)
	}
}

// At 180 columns the divider splits at the middle of the 100-column
// content, not at the terminal's middle (inside the text's column).
func TestDivider5_SplitsAtTheContentMiddle(t *testing.T) {
	// runs is the width of each styled run: the dim and the accent halves.
	runs := func(d string) []int {
		var n []int
		for _, r := range strings.Split(strings.TrimSuffix(d, "\x1b[m"), "\x1b[m") {
			n = append(n, ansi.StringWidth(r))
		}
		return n
	}
	for _, tc := range []struct {
		w    int
		up   bool
		want []int
	}{
		{180, false, []int{50, 130}}, // dim to 50, accent
		{180, true, []int{50, 130}},  // accent to 50, dim
		{49, false, []int{24, 25}},
	} {
		d := divider(tc.w, tc.up)
		if got := runs(d); !slices.Equal(got, tc.want) || ansi.StringWidth(d) != tc.w {
			t.Errorf("%d up=%v: runs %v, want %v", tc.w, tc.up, got, tc.want)
		}
	}
}

// "saved privately": the config file is 0600 after a save, even when it
// was there before with a looser mode.
func TestConfig5_SavedPrivately(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DUN_HOME", home)
	p := filepath.Join(home, "config.json")
	if err := os.WriteFile(p, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := saveConfig(dunConfig{Key: "sk-secret"}); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(p)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("config mode %v, %v", st.Mode().Perm(), err)
	}
	m := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	m.openProviderForm(nil)
	if !strings.Contains(stripANSI(m.providerFormPanel()), "saved privately to ") {
		t.Errorf("form:\n%s", stripANSI(m.providerFormPanel()))
	}
}

// At 49 columns the forms' placeholders and notes are whole.
func TestForms5_FitAPhone(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("DUN_HOME", filepath.Join(home, ".dun"))
	m := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 49, Height: 21})
	m = nm.(tuiModel)
	m.openProviderForm(nil)
	f := m
	f.openMCPForm()
	for name, p := range map[string]string{"login": stripANSI(fitRows(m.providerFormPanel(), 49)), "mcp": stripANSI(fitRows(f.mcpFormPanel(), 49))} {
		if strings.Contains(p, "…") {
			t.Errorf("%s form cut at 49:\n%s", name, p)
		}
	}
}

// A notice that wraps on a phone continues under its words, not under ✓
// (run 4: "· key set" alone at column 0).
func TestNotice5_WrapsUnderItsWords(t *testing.T) {
	v := newVtui(49, 21)
	v.m.appendNotice(convoEntry{collapsed: okNotice("logged in to local", "http://127.0.0.1:42299/v1", "key set")})
	all := strings.Join(shown(v), "\n")
	if !strings.Contains(all, " ✓ logged in to local · http://127.0.0.1:42299/v1\n   key set") {
		t.Errorf("notice:\n%s", all)
	}
}
