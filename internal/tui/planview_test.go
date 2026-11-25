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
		map[string]any{"id": "2", "title": "Emit events", "status": "doing", "details": "write the encoder", "done_when": "round trip", "needs": []any{"1"}, "depth": 1.0},
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
	m = m.notePlan(planEvent(nil))
	m.planPaging().view = planOrder // these tests read the graph's own order
	m.syncPager()
	m.pg.cur = 0
	return m, rec
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
			for _, want := range []string{"○ Document", "◐ Emit", "✓ Parse", "≡ 1 tree finished · 1 node", "waits 1"} {
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
		map[string]any{"id": "2", "title": "Old child", "status": "dropped", "needs": []any{"1"}, "depth": 1.0},
		map[string]any{"id": "3", "title": "Decide the db", "status": "todo", "owner": "agent:2"},
		map[string]any{"id": "4", "title": "Migrate", "status": "todo", "owner": "agent:2", "needs": []any{"3"}, "depth": 1.0},
		map[string]any{"id": "5", "title": "Main's step", "status": "doing"},
	}}
}

func ownedPlanModel(t *testing.T) (tuiModel, *recWC) {
	t.Helper()
	m, rec := planModel(t, 100, 30)
	m.agents = []agentRow{{id: 2, state: "running", prompt: "decide"}}
	m = m.notePlan(ownedPlan())
	m.planPaging().view = planOrder // these tests read the graph's own order
	m.syncPager()
	m.pg.cur = 0
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

// The selection follows its node when an update moves the rows: a tree that
// finishes above the cursor folds away to the end, and e still edits the node
// the person was on.
func TestPlanScreen_SelectionFollowsItsNode(t *testing.T) {
	m, _ := ownedPlanModel(t)
	for range 2 { // 3, 4, then 5
		nm, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
		m = nm.(tuiModel)
	}
	ev := ownedPlan()
	rows := ev["rows"].([]any)
	rows[2].(map[string]any)["status"] = "done"
	rows[3].(map[string]any)["status"] = "done"
	m = m.notePlan(ev)
	nm, _ := m.Update(tea.KeyPressMsg{Code: 'e', Text: "e"})
	em := nm.(tuiModel)
	if got := em.input.Value(); got != "/plan edit 5 title Main's step" {
		t.Errorf("e acted on %q", got)
	}
}

func planRowsText(m tuiModel) string { return ansi.Strip(m.drawView()) }

func planKey(m tuiModel, k string) tuiModel {
	nm, _ := m.Update(tea.KeyPressMsg{Code: rune(k[0]), Text: k})
	return nm.(tuiModel)
}

func indexOf(s, sub string) int { return strings.Index(s, sub) }

// Outline is the default: a goal over what it needs, repeats marked "again".
func TestPlanScreen_Outline(t *testing.T) {
	m, _ := planModel(t, 100, 30)
	if (&planView{}).view != planOutline {
		t.Fatal("outline is not the default")
	}
	m.planPaging().view = planOutline
	m.syncPager()
	// 3 needs 1 and 2; 2 needs 1: roots (nothing needs them) are 3, then 4.
	p := planRowsText(m)
	if h := ansi.Strip(m.planPaging().hint()); !strings.Contains(h, "view: outline") {
		t.Errorf("hint lacks the view: %s", h)
	}
	i3, i1, i2 := indexOf(p, "3 ○ Document it"), indexOf(p, "  1 ✓ Parse"), indexOf(p, "  2 ◐ Emit")
	if i3 < 0 || i1 < i3 || i2 < i1 {
		t.Fatalf("goal 3 is not over 1 and 2:\n%s", p)
	}
	if !strings.Contains(p, "    1 ✓ Parse the file  · again") {
		t.Errorf("1 under 2 is not marked again:\n%s", p)
	}
}

func TestPlanScreen_ViewsCycleAndFold(t *testing.T) {
	m, _ := ownedPlanModel(t) // order view
	if m.planPaging().view != planOrder {
		t.Fatal("not order")
	}
	m = planKey(m, "v")
	if m.planPaging().view != planFlat || !strings.Contains(ansi.Strip(m.planPaging().hint()), "view: flat") {
		t.Fatalf("v from order is not flat:\n%s", planRowsText(m))
	}
	p := planRowsText(m)
	for _, want := range []string{"\n▎▸ 3 ", "4 ⠋ Migrate", "5 ◐ Main", "≡ 1 tree finished · 2 nodes"} {
		if !strings.Contains(p, want) {
			t.Errorf("flat lacks %q:\n%s", want, p)
		}
	}
	if strings.Contains(p, "Old work") || strings.Contains(p, "  4 ") {
		t.Errorf("flat shows a folded or indented row:\n%s", p)
	}
	m = planKey(m, "v")
	if m.planPaging().view != planOutline {
		t.Fatal("v from flat is not outline")
	}
	p = planRowsText(m)
	if !strings.Contains(p, "≡ 1 tree finished · 2 nodes") || strings.Contains(p, "Old work") {
		t.Errorf("outline does not fold:\n%s", p)
	}
	// Migrate (4) needs Decide (3): the outline has 4 over 3.
	if indexOf(p, "4 ⠋ Migrate") > indexOf(p, "  3 ⠋ Decide") || indexOf(p, "  3 ⠋ Decide") < 0 {
		t.Errorf("outline does not put 4 over 3:\n%s", p)
	}
	// The view survives a refresh of the open screen.
	m = m.notePlan(ownedPlan())
	if m.planPaging().view != planOutline {
		t.Error("a refresh reset the view")
	}
}

// c on the folded row asks the engine to clear; elsewhere it does nothing.
func TestPlanScreen_ClearKey(t *testing.T) {
	m, rec := ownedPlanModel(t)
	rec.Reset()
	m = planKey(m, "c")
	if strings.Contains(rec.String(), "clear") {
		t.Errorf("c on a node sent %s", rec.String())
	}
	for i := 0; i < 3; i++ {
		nm, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
		m = nm.(tuiModel)
	}
	m = planKey(m, "c")
	if !strings.Contains(rec.String(), `"clear"`) {
		t.Errorf("c on the finished row sent %q", rec.String())
	}
}

// A plan event with a message (auto-archive) shows once in the status line.
func TestPlanScreen_ArchiveNoteFlashes(t *testing.T) {
	m, _ := planModel(t, 100, 30)
	m = m.notePlan(planEvent(map[string]any{"note": "archived 2 trees · 5 nodes"}))
	if !m.flashing() || m.flashText != "archived 2 trees · 5 nodes" {
		t.Errorf("flash %q", m.flashText)
	}
}

// Flat folds only the nodes /plan clear would archive: a done node in a tree
// with work left stays.
func TestPlanScreen_FlatFoldsFinishedComponentsOnly(t *testing.T) {
	m, _ := planModel(t, 100, 30)
	m.planPaging().view = planFlat
	m.syncPager()
	p := planRowsText(m)
	if !strings.Contains(p, "1 ✓ Parse the file") || !strings.Contains(p, "≡ 1 tree finished · 1 node") || strings.Contains(p, "Old way") {
		t.Errorf("flat:\n%s", p)
	}
}

// A repeat is not another node in the finished row's count.
func TestPlanScreen_OutlineFinishedCountsDistinctNodes(t *testing.T) {
	m, _ := planModel(t, 100, 30)
	done := func(id string, needs ...any) map[string]any {
		return map[string]any{"id": id, "title": "n" + id, "status": "done", "needs": needs}
	}
	m = m.notePlan(evMsg{"type": "plan", "total": 3.0, "done": 3.0, "rows": []any{
		done("1"), done("2", "1"), done("3", "1", "2")}})
	m.planPaging().view = planOutline
	m.syncPager()
	if p := planRowsText(m); !strings.Contains(p, "≡ 1 tree finished · 3 nodes") {
		t.Errorf("count:\n%s", p)
	}
}

// A node the idle suggestion call recorded (by "suggest") shows "auto" beside
// its mark, in a live tree and in the finished row; the agent's do not.
func TestPlanScreen_AutoTag(t *testing.T) {
	m, _ := planModel(t, 100, 30)
	m = m.notePlan(evMsg{"type": "plan", "total": 4.0, "done": 3.0, "rows": []any{
		map[string]any{"id": "1", "title": "Parse the file", "status": "doing", "done_when": "tests pass"},
		map[string]any{"id": "2", "title": "Wrote the parser tests", "status": "done", "needs": []any{"1"}, "depth": 1.0, "by": "suggest"},
		map[string]any{"id": "3", "title": "Fixed the lint", "status": "done", "by": "suggest"},
		map[string]any{"id": "4", "title": "Old work", "status": "done"},
	}})
	plain := ansi.Strip(m.drawView())
	if !strings.Contains(plain, "2 ✓ auto Wrote the parser tests") {
		t.Errorf("live row lacks the tag:\n%s", plain)
	}
	for i := 0; i < 2; i++ { // to the finished row, and open it
		nm, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
		m = nm.(tuiModel)
	}
	nm, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = nm.(tuiModel)
	plain = ansi.Strip(m.drawView())
	for _, want := range []string{"3 ✓ auto Fixed the lint", "4 ✓ Old work"} {
		if !strings.Contains(plain, want) {
			t.Errorf("lacks %q:\n%s", want, plain)
		}
	}
	if strings.Contains(plain, "auto Parse") || strings.Contains(plain, "auto Old") {
		t.Errorf("agent's node tagged:\n%s", plain)
	}
}

// A node under two parents: 9 is needed by 7 and 8 and waits on 5 and 6.
func sharedPlanModel(t *testing.T, w, h int) tuiModel {
	t.Helper()
	m, _ := planModel(t, w, h)
	row := func(id, status string, needs ...any) map[string]any {
		return map[string]any{"id": id, "title": "Node " + id, "status": status, "needs": needs}
	}
	r9 := row("9", "todo", "5", "6")
	r9["waiting"] = []any{"needs 5 (doing)", "needs 6 (todo)"}
	m = m.notePlan(evMsg{"type": "plan", "mode": true, "total": 5.0, "done": 0.0, "rows": []any{
		row("7", "todo", "9"), r9, row("5", "doing"), row("6", "todo"), row("8", "todo", "9"),
	}})
	m.planPaging().view = planOutline
	m.syncPager()
	m.pg.cur = 0
	return m
}

func nodeIndexes(m tuiModel, id string) []int {
	var at []int
	for i, n := range m.pg.doc.nodes {
		if n.group == id {
			at = append(at, i)
		}
	}
	return at
}

func TestPlanScreen_SameNodeMarkedAndJump(t *testing.T) {
	m := sharedPlanModel(t, 100, 30)
	at := nodeIndexes(m, "9")
	if len(at) != 2 {
		t.Fatalf("9 shown %d times", len(at))
	}
	m.pg.cur = at[1] // the "again" row
	m.syncPager()
	p := planRowsText(m)
	var gut []string
	for _, l := range strings.Split(p, "\n") {
		if strings.Contains(l, "Node 9") {
			j := strings.Index(l, "▸")
			gut = append(gut, strings.TrimSpace(l[:j]))
		}
	}
	if len(gut) != 2 || !strings.HasSuffix(gut[0], "◆") || !strings.HasSuffix(gut[1], "▎") {
		t.Errorf("gutters of 9 = %v:\n%s", gut, p)
	}
	if strings.Count(p, "◆") != 1 {
		t.Errorf("other nodes marked:\n%s", p)
	}
	m = planKey(m, "]")
	if m.pg.cur != at[0] {
		t.Errorf("] went to %d, want %d", m.pg.cur, at[0])
	}
	m = planKey(m, "[")
	if m.pg.cur != at[1] {
		t.Errorf("[ went to %d, want %d", m.pg.cur, at[1])
	}
	m.pg.cur = 0 // a node shown once: the key stays put
	m = planKey(m, "]")
	if m.pg.cur != 0 {
		t.Errorf("] on a single row moved to %d", m.pg.cur)
	}
}

func TestPlanScreen_EveryOccurrenceNamesAllBlockersAndParents(t *testing.T) {
	for _, w := range []int{100, 60} {
		m := sharedPlanModel(t, w, 40)
		at := nodeIndexes(m, "9")
		var bodies []string
		for _, i := range at {
			n := m.pg.doc.nodes[i]
			tags := n.head + " " + n.sub
			for _, want := range []string{"waits 2", "under 7, 8"} {
				if !strings.Contains(tags, want) {
					t.Errorf("%d: row %d tags lack %q: %q", w, i, want, tags)
				}
			}
			bodies = append(bodies, strings.ReplaceAll(n.body, "(already shown under another parent)\n", ""))
		}
		if bodies[0] != bodies[1] {
			t.Errorf("%d: bodies differ:\n%s\n--\n%s", w, bodies[0], bodies[1])
		}
		for _, want := range []string{"needs 5 (doing) · Node 5", "needs 6 (todo) · Node 6", "under: 7 · 8"} {
			if !strings.Contains(bodies[0], want) {
				t.Errorf("%d: body lacks %q:\n%s", w, want, bodies[0])
			}
		}
		for i := range m.pg.doc.nodes { // open all: every line fits
			m.pg.open[i] = true
		}
		m.syncPager()
		for _, l := range strings.Split(planRowsText(m), "\n") {
			if n := len([]rune(l)); n > w {
				t.Errorf("%d: row of %d cells: %q", w, n, l)
			}
		}
	}
}

// d, e and m lend the composer and /plan comes back where it was once the
// line is sent, or put aside with esc: dropping several nodes is d, enter
// for each, not a reopen and a walk back to the node each time.
func TestPlanScreen_DropComesBackToTheNode(t *testing.T) {
	m, rec := ownedPlanModel(t)
	nm, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyDown}) // node 4
	m = planKey(nm.(tuiModel), "d")
	if m.activeScreen() != nil || m.input.Value() != "/plan drop 4 " {
		t.Fatalf("d: screen up %v, composer %q", m.activeScreen() != nil, m.input.Value())
	}
	if plain := ansi.Strip(m.drawView()); !strings.Contains(plain, "esc back to /plan") {
		t.Errorf("the status does not say esc goes back:\n%s", plain)
	}
	rec.Reset()
	nm, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = nm.(tuiModel)
	sent := rec.String()
	if !strings.Contains(sent, `"plan drop 4"`) || !strings.Contains(sent, `"view"`) || strings.Index(sent, `"view"`) < strings.Index(sent, "plan drop 4") {
		t.Fatalf("enter must send the drop, then ask for /plan: %s", sent)
	}
	ev := ownedPlan()
	ev["rows"].([]any)[3].(map[string]any)["status"] = "dropped"
	m = m.notePlan(ev)
	v := m.planPaging()
	if v == nil || v.view != planOrder || v.keys[m.pg.cur] != "4#1" {
		t.Fatalf("/plan did not come back on node 4: open %v", v != nil)
	}

	// The node's tree finished: it folds away, and the row it was on stays
	// selected.
	nm, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyDown}) // node 5
	m = planKey(nm.(tuiModel), "d")
	nm, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = nm.(tuiModel)
	ev["rows"].([]any)[4].(map[string]any)["status"] = "dropped"
	m = m.notePlan(ev)
	if v := m.planPaging(); v == nil || m.pg.cur != 2 {
		t.Fatalf("/plan did not come back on the row: open %v, cur %d", v != nil, m.pg.cur)
	}

	// esc: nothing sent, /plan back.
	m, rec = ownedPlanModel(t)
	m = planKey(m, "d")
	rec.Reset()
	nm, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = nm.(tuiModel)
	if m.planBack != nil || !m.planWant || m.input.Value() != "" || strings.Contains(rec.String(), "drop") {
		t.Errorf("esc must send nothing and go back to /plan: %s", rec.String())
	}

	// The line made into a message: it goes, and /plan stays down.
	m, rec = ownedPlanModel(t)
	m = planKey(m, "d")
	m.input.SetValue("never mind")
	rec.Reset()
	nm, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = nm.(tuiModel)
	if m.planBack != nil || m.planWant || strings.Contains(rec.String(), `"view"`) {
		t.Errorf("a message reopened /plan: %s", rec.String())
	}
}

func TestPlanScreen_FlatNamesParentsToo(t *testing.T) {
	m := sharedPlanModel(t, 100, 30)
	m.planPaging().view = planFlat
	m.syncPager()
	at := nodeIndexes(m, "9")
	if len(at) != 1 || strings.Contains(planRowsText(m), "◆") {
		t.Fatalf("flat shows 9 %d times", len(at))
	}
	if b := m.pg.doc.nodes[at[0]].body; !strings.Contains(b, "under: 7 · 8") || !strings.Contains(b, "needs 6 (todo) · Node 6") {
		t.Errorf("flat body: %s", b)
	}
}
