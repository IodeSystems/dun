package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// askPlanEvent is a plan with one node serving ask 4, and the asks ledger:
// the intake (3 messages), two open asks (one worked), a blocked one, a
// done and a filed one.
func askPlanEvent(now time.Time) evMsg {
	at := float64(now.Add(-5 * time.Minute).Unix())
	ask := func(n float64, title, state string, working bool) map[string]any {
		return map[string]any{"n": n, "title": title, "state": state, "at": at, "working": working}
	}
	filed := ask(5, "locate tow yards", "filed", false)
	filed["nodes"] = []any{"1"}
	blocked := ask(6, "deploy it", "blocked", false)
	blocked["why"] = "which region?"
	return evMsg{"type": "plan", "total": 1.0, "done": 0.0,
		"intake": map[string]any{"working": false, "messages": []any{
			map[string]any{"at": at, "text": "the tools and stubs dir is not what I expect\nmore"},
			map[string]any{"at": at, "text": "continue"},
			map[string]any{"at": at, "text": "rety"},
		}},
		"asks": []any{ask(1, "fix the stubs dir", "open", true), ask(2, "vehicle telemetry", "open", false),
			ask(3, "add plan nodes", "done", false), filed, blocked},
		"rows": []any{
			map[string]any{"id": "1", "title": "write the tow yard list", "status": "todo", "ask": 5.0},
		}}
}

// The ask list is at the top of /plan: the intake is one row, only the
// worked ask shows working, the other open one "open · 5m", done and filed
// asks fold into one row that opens; a node that serves an ask is tagged.
// /plan has no ask roots.
func TestPlanScreen_AskList(t *testing.T) {
	now := time.Now()
	m := newTUIModel(&dunProc{stdin: &recWC{}}, "/ws")
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = nm.(tuiModel)
	m.openPlan()
	m = m.notePlan(askPlanEvent(now))
	m.planPaging().now = func() time.Time { return now }
	m.busy = true
	m.syncPager()
	plain := ansi.Strip(m.drawView())
	for _, w := range []string{`▸ new · 3 messages · "the tools and stubs dir is not…" +2`, "ask 1 fix the stubs dir  · working",
		"ask 2 vehicle telemetry  · open · 5m", "ask 6 deploy it  · blocked: which region?", "▸ 1 done · 1 filed",
		"1 ○ write the tow yard list  · ask 5"} {
		if !strings.Contains(plain, w) {
			t.Errorf("lacks %q:\n%s", w, plain)
		}
	}
	if strings.Count(plain, "working") != 1 {
		t.Errorf("working shows %d times:\n%s", strings.Count(plain, "working"), plain)
	}
	if strings.Index(plain, "▸ new") > strings.Index(plain, "ask 1 fix") || strings.Contains(plain, "ask 3 add plan nodes") {
		t.Errorf("order or fold wrong:\n%s", plain)
	}
	m.pg.cur = 4 // the fold row: after the intake and asks 1, 2 and 6
	nm, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = nm.(tuiModel)
	if plain := ansi.Strip(m.drawView()); !strings.Contains(plain, "ask 5 locate tow yards  · filed → 1") || !strings.Contains(plain, "ask 3 add plan nodes  · done") {
		t.Errorf("the fold does not open to its asks:\n%s", plain)
	}
	m.busy = false
	m.syncPager()
	if plain := ansi.Strip(m.drawView()); strings.Contains(plain, "working") || !strings.Contains(plain, "ask 1 fix the stubs dir  · open · 5m") {
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
