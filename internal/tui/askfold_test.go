package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// askPlanEvent is a plan with the intake (3 messages), two open asks (one
// worked), a done ask with a node under it, and a filed ask.
func askPlanEvent(now time.Time) evMsg {
	at := float64(now.Add(-5 * time.Minute).Unix())
	ask := func(state string, working bool) map[string]any {
		return map[string]any{"n": 1.0, "state": state, "at": at, "working": working}
	}
	return evMsg{"type": "plan", "total": 5.0, "done": 2.0,
		"intake": map[string]any{"working": false, "messages": []any{
			map[string]any{"at": at, "text": "the tools and stubs dir is not what I expect\nmore"},
			map[string]any{"at": at, "text": "continue"},
			map[string]any{"at": at, "text": "rety"},
		}},
		"rows": []any{
			map[string]any{"id": "73", "title": "ask: fix the stubs dir", "status": "doing", "by": "user", "ask": ask("open", true)},
			map[string]any{"id": "75", "title": "ask: vehicle telemetry", "status": "doing", "by": "user", "ask": ask("open", false)},
			map[string]any{"id": "76", "title": "ask: add plan nodes", "status": "done", "by": "user", "ask": ask("done", false)},
			map[string]any{"id": "90", "title": "write the plan nodes", "status": "todo", "needs": []any{"76"}},
			map[string]any{"id": "80", "title": "ask: locate tow yards", "status": "done", "by": "user",
				"ask": map[string]any{"n": 4.0, "state": "filed", "at": at, "filed": []any{"90"}}},
		}}
}

// The intake is one row above the asks; only the worked ask shows working,
// the other open one "open · 5m"; done and filed asks fold into one row,
// and the node under the done ask shows as a root. The row opens.
func TestPlanScreen_AsksIntakeWorkingAndFold(t *testing.T) {
	now := time.Now()
	rec := &recWC{}
	m := newTUIModel(&dunProc{stdin: rec}, "/ws")
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = nm.(tuiModel)
	m.openPlan()
	m = m.notePlan(askPlanEvent(now))
	m.planPaging().now = func() time.Time { return now }
	m.busy = true
	m.syncPager()
	plain := ansi.Strip(m.drawView())
	for _, w := range []string{`▸ new · 3 messages · "the tools and stubs dir is not…" +2`, "fix the stubs dir  · working",
		"vehicle telemetry  · open · 5m", "▸ 1 done · 1 filed", "90 ○ write the plan nodes"} {
		if !strings.Contains(plain, w) {
			t.Errorf("lacks %q:\n%s", w, plain)
		}
	}
	if strings.Count(plain, "working") != 1 {
		t.Errorf("working shows %d times:\n%s", strings.Count(plain, "working"), plain)
	}
	if strings.Index(plain, "▸ new") > strings.Index(plain, "fix the stubs dir") {
		t.Errorf("the intake is not above the asks:\n%s", plain)
	}
	if strings.Contains(plain, "locate tow yards") {
		t.Errorf("a filed ask is not folded:\n%s", plain)
	}
	// Open the fold row: the last.
	for range 5 {
		nm, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
		m = nm.(tuiModel)
	}
	nm, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = nm.(tuiModel)
	if plain := ansi.Strip(m.drawView()); !strings.Contains(plain, "80 ✓ ask: locate tow yards  · filed → 90") || !strings.Contains(plain, "76 ✓ ask: add plan nodes  · done") {
		t.Errorf("the fold does not open to its asks:\n%s", plain)
	}
	// Idle: nothing works, the intake shows its age.
	m.busy = false
	m.syncPager()
	if plain := ansi.Strip(m.drawView()); strings.Contains(plain, "working") || !strings.Contains(plain, "fix the stubs dir  · open · 5m") {
		t.Errorf("idle:\n%s", plain)
	}
}

// The journal folds done and filed asks into one row; open asks and other
// entries stay listed.
func TestJournalScreen_AsksFold(t *testing.T) {
	m := newTUIModel(&dunProc{stdin: &recWC{}}, "/ws")
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m = nm.(tuiModel)
	m.openJournal()
	m = m.noteJournal(evMsg{"type": "journal", "entries": []any{
		map[string]any{"n": 1.0, "kind": "ask", "line": "#1 ask 1 · done · \"fix the flaky test\"", "outcome": "done"},
		map[string]any{"n": 2.0, "kind": "ask", "line": "#2 ask 2 · filed · \"locate tow yards\"", "outcome": "filed"},
		map[string]any{"n": 3.0, "kind": "fold", "line": "#3 fold · parser work"},
		map[string]any{"n": 4.0, "kind": "ask", "line": "#4 ask 3 · open · \"update the docs\""},
	}})
	plain := ansi.Strip(m.drawView())
	if !strings.Contains(plain, "▸ 1 done · 1 filed") || strings.Contains(plain, "fix the flaky test") ||
		!strings.Contains(plain, "update the docs") || !strings.Contains(plain, "parser work") {
		t.Fatalf("journal:\n%s", plain)
	}
}
