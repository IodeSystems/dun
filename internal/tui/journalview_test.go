package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func typeKey(t *testing.T, m tuiModel, k string) tuiModel {
	t.Helper()
	nm, _ := m.Update(tea.KeyPressMsg{Code: rune(k[0]), Text: k})
	return nm.(tuiModel)
}

// s asks the engine for a summary; while it runs the first row says
// "summarizing…" (and s does not ask again); then the three parts show at
// the top, open and wrapped, with when they were made and the cost; s again
// asks for a fresh one.
func TestPlanScreen_SummaryKey(t *testing.T) {
	m, rec := planModel(t, 60, 30)
	rec.Reset()
	m = typeKey(t, m, "s")
	if got := rec.String(); strings.Count(got, `"summary"`) != 1 || !strings.Contains(got, `"plan"`) {
		t.Fatalf("s sent %s", got)
	}
	m = m.notePlan(planEvent(map[string]any{"summary": map[string]any{"running": true}}))
	if plain := ansi.Strip(m.drawView()); !strings.Contains(plain, "summarizing…") {
		t.Errorf("no summarizing…:\n%s", plain)
	}
	rec.Reset()
	m = typeKey(t, m, "s")
	if strings.Contains(rec.String(), `"summary"`) {
		t.Errorf("s while running sent %s", rec.String())
	}
	long := "the encoder for every event type the protocol has, with a round trip test for each"
	m = m.notePlan(planEvent(map[string]any{"summary": map[string]any{
		"text": "## Done\n- 1 parsed\n## Doing\n- 2 " + long + "\n## Next\n- 3 the README",
		"made": "made 14:02:11 · 3.1k tok · $0.0021"}}))
	plain := ansi.Strip(m.drawView())
	for _, want := range []string{"## Done", "## Doing", "## Next", "made 14:02:11", "$0.0021", "round trip test"} {
		if !strings.Contains(strings.ReplaceAll(plain, "\n", " "), want) && !strings.Contains(plain, want) {
			t.Errorf("lacks %q:\n%s", want, plain)
		}
	}
	if i, j := strings.Index(plain, "## Done"), strings.Index(plain, "Parse the file"); i < 0 || j < 0 || i > j {
		t.Errorf("the summary is not at the top:\n%s", plain)
	}
	for _, l := range strings.Split(plain, "\n") {
		if n := len([]rune(l)); n > 60 {
			t.Errorf("row of %d cells: %q", n, l)
		}
	}
	rec.Reset()
	m = typeKey(t, m, "s")
	if !strings.Contains(rec.String(), `"summary"`) {
		t.Errorf("a second s did not refresh: %s", rec.String())
	}
	// Collapsible: left on the summary row closes it.
	m.pg.cur = 0
	m.pg.key("left")
	m.syncPager()
	if plain := ansi.Strip(m.drawView()); strings.Contains(plain, "## Done") {
		t.Errorf("the summary did not collapse:\n%s", plain)
	}
}

func journalEvent() evMsg {
	return evMsg{"type": "journal", "entries": []any{
		map[string]any{"n": 1.0, "kind": "fold", "line": "#1  Nov 17 10:00  fold     find the port", "text": "Found it."},
		map[string]any{"n": 2.0, "kind": "worklog", "line": "#2  Nov 17 11:00  worklog  worklog #1 Fix", "text": "A fix.", "path": "/s.worklog/1-fix.md"},
		map[string]any{"n": 3.0, "kind": "now", "line": "#3  Nov 17 12:00  now      Fixed the parser", "text": "- changed parse.go"},
	}}
}

// /journal opens a screen: newest first; enter opens one; n asks for
// /journal now.
func TestJournalScreen(t *testing.T) {
	rec := &recWC{}
	m := newTUIModel(&dunProc{stdin: rec}, "/ws")
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = nm.(tuiModel)
	if m = m.noteJournal(journalEvent()); m.activeScreen() != nil {
		t.Fatal("an unasked journal event opened the screen")
	}
	m.openJournal()
	if !strings.Contains(rec.String(), `"journal"`) || !strings.Contains(rec.String(), `"screen"`) {
		t.Fatalf("not asked: %s", rec.String())
	}
	m = m.noteJournal(journalEvent())
	if m.activeScreen() == nil {
		t.Fatal("no screen")
	}
	plain := ansi.Strip(m.drawView())
	a, b, c := strings.Index(plain, "#3"), strings.Index(plain, "#2"), strings.Index(plain, "#1")
	if a < 0 || b < 0 || c < 0 || a > b || b > c || !strings.Contains(plain, "3 entries") {
		t.Fatalf("not newest first:\n%s", plain)
	}
	nm, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = nm.(tuiModel)
	if plain := ansi.Strip(m.drawView()); !strings.Contains(plain, "changed parse.go") {
		t.Errorf("enter did not open the entry:\n%s", plain)
	}
	rec.Reset()
	m = typeKey(t, m, "n")
	if !strings.Contains(rec.String(), `"journal"`) || !strings.Contains(rec.String(), `"now"`) {
		t.Errorf("n sent %s", rec.String())
	}
}

// A ledger row (an ask, a turn) is long: at any width the screen's rows fit,
// and the row's start (number, kind, ask, outcome) shows.
func TestJournalScreen_LedgerRowFits(t *testing.T) {
	ask := "#4  Nov 17 12:00  ask      ask 3 · done · \"fix the flaky test\" · turns 12–15 · 6 files (+120/−34) · 1 commit · 11 cmds (1 failed) · 9m"
	turn := "#5  Nov 17 12:30  turn     turn 16 · 2 files (+3/−1) · 4 cmds · 1m"
	for _, w := range []int{40, 60, 100} {
		rec := &recWC{}
		m := newTUIModel(&dunProc{stdin: rec}, "/ws")
		nm, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: 30})
		m = nm.(tuiModel)
		m.openJournal()
		ev := journalEvent()
		ev["entries"] = append(ev["entries"].([]any),
			map[string]any{"n": 4.0, "kind": "ask", "line": ask, "text": "files (6, +120/−34):"},
			map[string]any{"n": 5.0, "kind": "turn", "line": turn, "text": "commands (4, 0 failed):"})
		m = m.noteJournal(ev)
		plain := ansi.Strip(m.drawView())
		for _, l := range strings.Split(plain, "\n") {
			if n := ansi.StringWidth(l); n > w {
				t.Errorf("width %d: row of %d cells: %q", w, n, l)
			}
		}
		if !strings.Contains(plain, "#4") || !strings.Contains(plain, "ask 3") || !strings.Contains(plain, "#5") {
			t.Errorf("width %d: the ledger rows do not show:\n%s", w, plain)
		}
	}
}
