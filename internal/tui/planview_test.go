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
			// "Old way" is a tree on its own, dropped: finished, so it is in
			// the one row at the end, not in the tree.
			for _, want := range []string{"○ Document", "◐ Emit", "✓ Parse", "≡ 1 tree finished · 1 node", "1 wait"} {
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

// ownedPlan is a plan with a finished tree first, a tree agent #2 owns, and
// the main agent's node in progress.
func ownedPlan() evMsg {
	return evMsg{"type": "plan", "total": 5.0, "done": 2.0, "rows": []any{
		map[string]any{"id": "1", "title": "Old work", "status": "done"},
		map[string]any{"id": "2", "title": "Old child", "status": "dropped", "depth": 1.0},
		map[string]any{"id": "3", "title": "Decide the db", "status": "todo", "owner": "agent:2"},
		map[string]any{"id": "4", "title": "Migrate", "status": "todo", "owner": "agent:2", "depth": 1.0},
		map[string]any{"id": "5", "title": "Main's step", "status": "doing"},
	}}
}

func ownedPlanModel(t *testing.T) (tuiModel, *recWC) {
	t.Helper()
	m, rec := planModel(t, 100, 30)
	m.agents = []agentRow{{id: 2, state: "running", prompt: "decide"}}
	m = m.notePlan(ownedPlan())
	m.syncPager()
	return m, rec
}

// A finished tree (every node done or dropped) leaves the tree for one row at
// the end, which opens to it; the keys still act on the node under the
// cursor, whatever moved.
func TestPlanScreen_FinishedTreesFoldAway(t *testing.T) {
	m, _ := ownedPlanModel(t)
	plain := ansi.Strip(m.drawView())
	if strings.Contains(plain, "Old work") || !strings.Contains(plain, "≡ 1 tree finished · 2 nodes") {
		t.Fatalf("the finished tree is not folded:\n%s", plain)
	}
	if strings.Index(plain, "Decide the db") > strings.Index(plain, "≡ 1 tree finished") {
		t.Errorf("the finished row is not last:\n%s", plain)
	}
	// The first node shown is 3 now, not the row-0 node 1.
	nm, _ := m.Update(tea.KeyPressMsg{Code: 'e', Text: "e"})
	em := nm.(tuiModel)
	if got := em.input.Value(); got != "/plan edit 3 title Decide the db" {
		t.Errorf("e acted on %q", got)
	}
	m, _ = ownedPlanModel(t)
	for i := 0; i < 3; i++ {
		nm, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
		m = nm.(tuiModel)
	}
	nm, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = nm.(tuiModel)
	if plain := ansi.Strip(m.drawView()); !strings.Contains(plain, "1 ✓ Old work") || !strings.Contains(plain, "  2 ✗ Old child") {
		t.Errorf("the finished row does not open to its trees:\n%s", plain)
	}
}

// An agent's nodes carry its colour and say what it is doing; a working
// agent's open nodes spin, and stop when it goes idle. The main agent's node
// in progress spins while a turn runs and shows ? while it asks.
func TestPlanScreen_OwnerColourAndMotion(t *testing.T) {
	m, _ := ownedPlanModel(t)
	plain := ansi.Strip(m.drawView())
	if !strings.Contains(plain, "agent 2 working") || strings.Contains(plain, "3 ○ Decide") {
		t.Errorf("a working agent's node does not spin:\n%s", plain)
	}
	if !m.planAnimates() || !m.pulseWanted() {
		t.Error("a node in motion must run the pulse")
	}
	if !strings.Contains(m.drawView(), planOwnerColors[1].Render("3 ")[:8]) {
		t.Errorf("agent 2's colour is not on its node")
	}
	m.agents[0].state = "idle"
	m.syncPager()
	plain = ansi.Strip(m.drawView())
	if !strings.Contains(plain, "3 ○ Decide the db") || !strings.Contains(plain, "agent 2 idle") || m.planAnimates() {
		t.Errorf("an idle agent's node still moves:\n%s", plain)
	}
	m.busy = true
	m.syncPager()
	if plain := ansi.Strip(m.drawView()); !strings.Contains(plain, "Main's step  · working") || !m.planAnimates() {
		t.Errorf("the main agent's node in progress:\n%s", plain)
	}
	m.asking = true
	m.syncPager()
	if plain := ansi.Strip(m.drawView()); !strings.Contains(plain, "5 ? Main's step  · asking you") {
		t.Errorf("asking:\n%s", plain)
	}
}

// enter on a node an agent works dives into that agent; t lends the composer
// to telling the node's owner, and sending puts /plan back.
func TestPlanScreen_DiveAndTell(t *testing.T) {
	m, rec := ownedPlanModel(t)
	rec.Reset()
	nm, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = nm.(tuiModel)
	if m.activeScreen() != nil || m.scopeAgent != 2 {
		t.Fatalf("enter did not dive: screen %v, scope %d", m.activeScreen() != nil, m.scopeAgent)
	}

	m, rec = ownedPlanModel(t)
	nm, _ = m.Update(tea.KeyPressMsg{Code: 't', Text: "t"})
	m = nm.(tuiModel)
	if m.activeScreen() != nil || m.planTelling == nil || !strings.Contains(m.input.placeholder, "tell agent #2 about node 3") {
		t.Fatalf("t did not lend the composer: %q", m.input.placeholder)
	}
	if plain := ansi.Strip(m.drawView()); !strings.Contains(plain, "tell agent #2 about node 3") || !strings.Contains(plain, "esc back to /plan") {
		t.Errorf("the status does not say where the words go:\n%s", plain)
	}
	rec.Reset()
	m.input.SetValue("use postgres")
	nm, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = nm.(tuiModel)
	if !strings.Contains(rec.String(), `"tell 3 use postgres"`) {
		t.Errorf("sent %s", rec.String())
	}
	if m.planTelling != nil || !m.planWant || m.input.placeholder == "" || strings.Contains(m.input.placeholder, "tell agent") {
		t.Errorf("the composer is not given back, or /plan not asked for again: %q", m.input.placeholder)
	}

	m, rec = ownedPlanModel(t)
	nm, _ = m.Update(tea.KeyPressMsg{Code: 't', Text: "t"})
	m = nm.(tuiModel)
	rec.Reset()
	nm, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = nm.(tuiModel)
	if m.planTelling != nil || !m.planWant || strings.Contains(rec.String(), "tell") {
		t.Errorf("esc must send nothing and go back to /plan: %s", rec.String())
	}

	// A tell that starts with "/" is still the words, not a command.
	m, rec = ownedPlanModel(t)
	nm, _ = m.Update(tea.KeyPressMsg{Code: 't', Text: "t"})
	m = nm.(tuiModel)
	rec.Reset()
	m.input.SetValue("/src/db should be postgres")
	nm, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = nm.(tuiModel)
	if !strings.Contains(rec.String(), `"tell 3 /src/db should be postgres"`) || m.planTelling != nil {
		t.Errorf("a leading / ran as a command: sent %s", rec.String())
	}
}
