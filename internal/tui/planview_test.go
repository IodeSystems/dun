package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func planEvent(extra map[string]any) evMsg {
	ev := evMsg{"type": "plan", "mode": true, "total": 4.0, "done": 1.0, "rows": []any{
		map[string]any{"id": "1", "title": "Parse the file", "status": "done", "done_when": "tests pass", "account": "go test ok"},
		map[string]any{"id": "2", "title": "Emit events", "status": "doing", "goal": "write the encoder", "done_when": "round trip", "needs": []any{"1"}, "depth": 1.0},
		map[string]any{"id": "3", "title": "Document it in the README", "status": "todo", "done_when": "README has it", "needs": []any{"1", "2"},
			"waiting": []any{"needs 2 (doing)"}, "depth": 2.0},
		map[string]any{"id": "3", "title": "Document it in the README", "status": "todo", "needs": []any{"1", "2"}, "waiting": []any{"needs 2 (doing)"},
			"depth": 1.0, "repeat": true},
		map[string]any{"id": "4", "title": "Old way", "status": "dropped", "why": "wrong approach", "ready": false},
	}}
	for k, v := range extra {
		ev[k] = v
	}
	return ev
}

func planModel(t *testing.T, w, h int) (tuiModel, *recWC) {
	t.Helper()
	rec := &recWC{}
	m := newTUIModel(&dunProc{stdin: rec}, "/ws")
	nm, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	m = nm.(tuiModel)
	m.openPlan()
	if !strings.Contains(rec.String(), `"plan"`) || !strings.Contains(rec.String(), `"view"`) {
		t.Fatalf("not asked: %s", rec.String())
	}
	return m.notePlan(planEvent(nil)), rec
}

// /plan is a screen: the status, then the graph as a tree with a node that
// has two parents shown under each, the repeat marked. It holds at a small
// terminal: every row fits the width and the screen fits the height.
func TestPlanScreen(t *testing.T) {
	for _, sz := range [][2]int{{100, 30}, {48, 24}, {30, 12}} {
		w, h := sz[0], sz[1]
		m, _ := planModel(t, w, h)
		if m.activeScreen() == nil {
			t.Fatalf("%dx%d: no screen", w, h)
		}
		plain := ansi.Strip(m.drawView())
		flat := strings.ReplaceAll(plain, "\n", " ")
		for _, want := range []string{"/plan · 1 of 4 done", "plan mode: on", "Parse the file", "Emit events"} {
			if !strings.Contains(flat, want) {
				t.Errorf("%dx%d: lacks %q:\n%s", w, h, want, plain)
			}
		}
		lines := strings.Split(plain, "\n")
		if len(lines) > h {
			t.Errorf("%dx%d: %d rows", w, h, len(lines))
		}
		for _, l := range lines {
			if n := len([]rune(l)); n > w {
				t.Errorf("%dx%d: row of %d cells: %q", w, h, n, l)
			}
		}
		if w >= 100 {
			// Both rows of the node with two parents, the second an "again".
			if strings.Count(plain, "Document it in the README") != 2 || !strings.Contains(plain, "again") {
				t.Errorf("repeat not shown:\n%s", plain)
			}
			for _, want := range []string{"○ Document", "◐ Emit", "✓ Parse", "✗ Old way", "1 wait"} {
				if !strings.Contains(plain, want) {
					t.Errorf("lacks %q:\n%s", want, plain)
				}
			}
			// The tree is indented by depth.
			if !strings.Contains(plain, "  2 ◐ Emit events") || !strings.Contains(plain, "    3 ○ Document") {
				t.Errorf("not indented:\n%s", plain)
			}
		}
	}
}

// enter opens a node: its done-when and why it waits show.
func TestPlanScreen_NodeDetails(t *testing.T) {
	m, _ := planModel(t, 100, 30)
	for i := 0; i < 2; i++ {
		nm, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
		m = nm.(tuiModel)
	}
	nm, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = nm.(tuiModel)
	plain := ansi.Strip(m.drawView())
	for _, want := range []string{"done when: README has it", "needs: 1, 2", "waiting: needs 2 (doing)"} {
		if !strings.Contains(plain, want) {
			t.Errorf("lacks %q:\n%s", want, plain)
		}
	}
}

// a approves; d and e fill the composer with the command, for the person to
// finish; there is no key that marks a node done.
func TestPlanScreen_Keys(t *testing.T) {
	m, rec := planModel(t, 100, 30)
	rec.Reset()
	nm, _ := m.Update(tea.KeyPressMsg{Code: 'a', Text: "a"})
	m = nm.(tuiModel)
	if !strings.Contains(rec.String(), `"plan"`) || !strings.Contains(rec.String(), `"approve"`) {
		t.Errorf("a sent %s", rec.String())
	}
	for _, key := range []string{"x", "m", "enter-done"} {
		rec.Reset()
		if len(key) == 1 {
			nm, _ = m.Update(tea.KeyPressMsg{Code: rune(key[0]), Text: key})
			m = nm.(tuiModel)
		}
		if strings.Contains(rec.String(), "done") {
			t.Errorf("%q marked something: %s", key, rec.String())
		}
	}
	for key, want := range map[string]string{"e": "/plan edit 1 title Parse the file", "d": "/plan drop 1 "} {
		m, _ := planModel(t, 100, 30)
		nm, _ := m.Update(tea.KeyPressMsg{Code: rune(key[0]), Text: key})
		m = nm.(tuiModel)
		if m.activeScreen() != nil {
			t.Errorf("%s: the screen stayed up", key)
		}
		if got := m.input.Value(); got != want {
			t.Errorf("%s: composer %q, want %q", key, got, want)
		}
	}
}

// A plan event while the screen is up refreshes it; one nobody asked for does
// not open it.
func TestPlanScreen_RefreshesAndDoesNotOpenUnasked(t *testing.T) {
	m, _ := planModel(t, 100, 30)
	m = m.notePlan(planEvent(map[string]any{"approved": true, "done": 2.0}))
	if plain := ansi.Strip(m.drawView()); !strings.Contains(plain, "2 of 4 done") || !strings.Contains(plain, "plan mode: on · approved") {
		t.Errorf("not refreshed:\n%s", plain)
	}
	n := newTUIModel(&dunProc{stdin: &recWC{}}, "/ws")
	n = n.notePlan(planEvent(nil))
	if n.activeScreen() != nil {
		t.Error("an unasked plan event opened the screen")
	}
}

func TestPlanScreen_Empty(t *testing.T) {
	m, _ := planModel(t, 60, 20)
	m = m.notePlan(evMsg{"type": "plan", "total": 0.0, "done": 0.0, "rows": []any{}})
	if plain := ansi.Strip(m.drawView()); !strings.Contains(plain, "plan is empty") {
		t.Errorf("empty:\n%s", plain)
	}
}
