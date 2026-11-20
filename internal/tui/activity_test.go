package tui

import (
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// The engine pushes agents on a change only, so a running child's time is
// counted on from the push; a child that is not running keeps its figure.
func TestAgentRow_RunningTimeCountsOn(t *testing.T) {
	at := time.Now()
	a := agentRow{id: 1, state: "running", seconds: 5, at: at}
	if got := agentActRow(a, at.Add(70*time.Second)).label; !strings.Contains(got, compactDur(75)) {
		t.Errorf("running: %q, want %s", got, compactDur(75))
	}
	a.state = "idle"
	if got := agentActRow(a, at.Add(70*time.Second)).label; !strings.Contains(got, compactDur(5)) {
		t.Errorf("idle: %q, want %s", got, compactDur(5))
	}
}

// A child a model tier picked the model of says the tier after its id.
func TestAgentRow_ShowsTier(t *testing.T) {
	a := agentRow{id: 3, state: "idle", tier: "think"}
	if got := agentActRow(a, time.Now()).label; !strings.HasPrefix(got, "#3 think idle") {
		t.Errorf("label %q", got)
	}
	a.tier = ""
	if got := agentActRow(a, time.Now()).label; !strings.HasPrefix(got, "#3 idle") {
		t.Errorf("untiered label %q", got)
	}
	// The level its requests send, as the engine said it.
	a.tier, a.effort = "fast", "minimal"
	if got := agentActRow(a, time.Now()).label; !strings.HasPrefix(got, "#3 fast idle · minimal") {
		t.Errorf("effort label %q", got)
	}
}

// A line wrapped while the pane was narrow is wrapped again when it widens,
// also to a width it still does not fit.
func TestNotice_RewrapsWhenThePaneWidens(t *testing.T) {
	v := newVtui(48, 20)
	v.event(map[string]any{"type": "ready", "tools": []any{"eval"}})
	words := strings.TrimSpace(strings.Repeat("notice ", 30)) // 209 columns
	v.m.append(words)
	widest := func() int {
		n := 0
		for _, l := range strings.Split(v.view(), "\n") {
			if strings.Contains(l, "notice") {
				n = max(n, len(strings.TrimSpace(l)))
			}
		}
		return n
	}
	if w := widest(); w > 48 {
		t.Fatalf("setup: %d columns at 48", w)
	}
	nm, _ := v.m.Update(tea.WindowSizeMsg{Width: 120, Height: 20})
	v.m = nm.(tuiModel)
	if w := widest(); w <= 48 {
		t.Errorf("widened to 120, the notice is still %d columns wide", w)
	}
}

// Dismissed agents are one stack row, in the strip and in the table; →
// lists them under it, ← folds them back.
func TestActivity_DismissedStack(t *testing.T) {
	m := newTUIModel(&dunProc{stdin: &recWC{}}, "/ws")
	m.w = 120
	m.agents = []agentRow{
		{id: 1, state: "dismissed", tokens: 1000},
		{id: 2, state: "running"},
		{id: 3, state: "dismissed", tokens: 2500},
	}
	plain := func(s string) string { return ansi.Strip(s) }
	line := plain(m.activityView())
	if !strings.Contains(line, "≡2 dismissed 3.5kt") || strings.Contains(line, "#1") || strings.Contains(line, "#3 dismissed") {
		t.Errorf("strip: %q", line)
	}
	m.setFocus(focusActivity)
	if rows := m.actRows(); len(rows) != 2 || rows[1].key != stackKey {
		t.Fatalf("table rows: %+v", rows)
	}
	if got := plain(m.activityView()); !strings.Contains(got, "▾ 3 agents") {
		t.Errorf("head: %q", got)
	}
	m.actSel = stackKey
	m.actKeys("right")
	rows := m.actRows()
	if len(rows) != 4 || !rows[2].member || rows[2].key.id != 1 || rows[3].key.id != 3 {
		t.Fatalf("open stack: %+v", rows)
	}
	if got := plain(m.activityView()); !strings.Contains(got, "▾ 3 agents") || !strings.Contains(got, "    · #3 dismissed") {
		t.Errorf("open table:\n%s", got)
	}
	m.actMove(2)
	m.actKeys("left")
	if m.actStackOpen || m.actSel != stackKey || len(m.actRows()) != 2 {
		t.Errorf("left did not fold the stack: open=%v sel=%+v", m.actStackOpen, m.actSel)
	}
	m.actStackOpen = true
	m.setFocus(focusInput)
	if got := plain(m.activityView()); strings.Contains(got, "#1 dismissed") || !strings.Contains(got, "≡2 dismissed") {
		t.Errorf("strip with the stack open lists members: %q", got)
	}
}

// The open table is grouped by state (working, idle agents, finished), recent first within a group, as of when it opened: what
// runs, then what changed or ended last; it does not reorder while open, and
// a row that arrives meanwhile goes at the end. The strip stays in id order.
func TestActivity_RecentFirst(t *testing.T) {
	m := newTUIModel(&dunProc{stdin: &recWC{}}, "/ws")
	m.w = 120
	now := time.Now()
	m.agents = []agentRow{
		{id: 1, state: "idle", changed: now.Add(-time.Hour)},
		{id: 2, state: "idle", changed: now.Add(-time.Minute)},
	}
	m.jobs = []jobRow{
		{id: 1, state: "ok", started: now.Add(-2 * time.Hour).Unix(), ended: now.Add(-30 * time.Minute).Unix()},
		{id: 2, state: "running", started: now.Add(-time.Minute).Unix()},
	}
	keys := func() []actKey {
		var ks []actKey
		for _, r := range m.actRows() {
			ks = append(ks, r.key)
		}
		return ks
	}
	if got := keys(); got[0] != (actKey{agent: true, id: 1}) {
		t.Errorf("collapsed: %v, want id order", got)
	}
	m.setFocus(focusActivity)
	want := []actKey{{id: 2}, {agent: true, id: 2}, {agent: true, id: 1}, {id: 1}}
	if got := keys(); !slices.Equal(got, want) {
		t.Fatalf("open: %v, want %v", got, want)
	}
	if m.actSel != want[0] {
		t.Errorf("selected %v, want the most recent", m.actSel)
	}
	// Agent #1 starts running: it does not jump while the table is open.
	m.agents[0].state, m.agents[0].changed = "running", now
	m.jobs = append(m.jobs, jobRow{id: 3, state: "running", started: now.Unix()})
	want = append(want, actKey{id: 3})
	if got := keys(); !slices.Equal(got, want) {
		t.Errorf("while open: %v, want %v", got, want)
	}
	m.setFocus(focusInput)
	m.setFocus(focusActivity)
	if got := keys(); got[0] == (actKey{id: 1}) || got[len(got)-1] != (actKey{id: 1}) {
		t.Errorf("reopened: %v, want the ended job last", got)
	}
}

// A push lists every child: one that did not move keeps its changed time.
func TestAgentRows_KeepChanged(t *testing.T) {
	then := time.Now().Add(-time.Hour)
	old := []agentRow{{id: 1, state: "idle", changed: then}, {id: 2, state: "running", changed: then}}
	cur := keepChanged(old, []agentRow{{id: 1, state: "idle", changed: time.Now()}, {id: 2, state: "idle", changed: time.Now()}})
	if !cur[0].changed.Equal(then) || cur[1].changed.Equal(then) {
		t.Errorf("changed: %v %v", cur[0].changed, cur[1].changed)
	}
}

// Every activity row is one screen row, whatever its detail holds: an opened
// stack of agents with many-line tasks drew every line and pushed the input
// off the screen.
func TestActivity_MultiLineDetailIsOneRow(t *testing.T) {
	m := newTUIModel(&dunProc{stdin: &recWC{}}, "/ws")
	m.w, m.h = 100, 30
	task := strings.Repeat("a line of the task\n", 40)
	m.agents = []agentRow{
		{id: 1, state: "dismissed", prompt: task},
		{id: 2, state: "stopped", prompt: task},
		{id: 3, state: "dismissed", prompt: task},
	}
	m.jobs = []jobRow{{id: 1, state: "running", command: "make\ntest\n"}}
	if h := lipgloss.Height(m.activityView()); h != 1 {
		t.Errorf("the strip is %d rows", h)
	}
	m.setFocus(focusActivity)
	m.actSel = stackKey
	m.actKeys("right")
	rows := m.actRows()
	if h := lipgloss.Height(m.activityView()); h != min(len(rows), actMaxRows)+1 {
		t.Errorf("open table is %d rows for %d items:\n%s", h, len(rows), m.activityView())
	}
	if !strings.Contains(ansi.Strip(m.activityView()), "a line of the task a line") {
		t.Errorf("the task's lines are not joined:\n%s", m.activityView())
	}
}

// The open table groups by state, agents and jobs interleaved: what needs
// the person (a failed job), working, idle agents, finished, the dismissed
// stack; recent first within a group.
func TestActivityTableGroupsByState(t *testing.T) {
	m := newTUIModel(&dunProc{stdin: &recWC{}}, "/ws")
	now := time.Now()
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	m.agents = []agentRow{
		{id: 1, state: "dismissed", changed: ago(time.Second)},
		{id: 2, state: "idle", changed: ago(time.Hour)},
		{id: 3, state: "running", changed: ago(time.Hour)},
		{id: 4, state: "idle", changed: ago(time.Minute)},
		{id: 5, state: "stopped", changed: ago(time.Second)},
	}
	m.jobs = []jobRow{
		{id: 1, state: "ok", ended: ago(time.Minute).Unix()},
		{id: 2, state: "running"},
		{id: 3, state: "failed", ended: ago(5 * time.Second).Unix()},
	}
	order := m.takeActOrder()
	want := []actKey{
		{id: 3},
		{agent: true, id: 3}, {id: 2}, // tie on recent: agents first
		{agent: true, id: 4}, {agent: true, id: 2},
		{agent: true, id: 5}, {id: 1},
		stackKey,
	}
	if !slices.Equal(order, want) {
		t.Fatalf("order %+v, want %+v", order, want)
	}
}

// A failed job's strip says what happened in words and the keys that reach
// its log; a narrow strip cuts the command, never the state or the keys.
func TestActivityStripFailedJobSaysWhy(t *testing.T) {
	m := newTUIModel(&dunProc{stdin: &recWC{}}, "/ws")
	m.w = 100
	now := time.Now().Unix()
	m.agents = []agentRow{{id: 1, state: "idle"}}
	m.jobs = []jobRow{
		{id: 2, state: "ok", command: "echo hi", started: now - 9, ended: now - 8},
		{id: 3, state: "failed", code: 2, command: "make test-pkg P=./internal/tui/...", started: now - 4, ended: now},
	}
	got := ansi.Strip(m.activityView())
	want := "▸ ✗ job 3 failed (exit 2) · shift+tab, enter: log · "
	if !strings.HasPrefix(got, want) {
		t.Fatalf("strip %q, want it to start %q", got, want)
	}
	if !strings.Contains(got, "⟩ make test") {
		t.Errorf("the command is the detail: %q", got)
	}
	m.focus = focusConvo
	if got := ansi.Strip(m.activityView()); !strings.Contains(got, "· tab, enter: log") {
		t.Errorf("from the conversation the key is tab: %q", got)
	}
	m.focus = focusInput
	m.w = 40
	got = ansi.Strip(m.activityView())
	if !strings.HasPrefix(got, "▸ ✗ job 3 failed (exit 2) · shift+tab") || lipgloss.Width(got) > m.w-1 {
		t.Errorf("narrow strip %q (%d cols): the state and keys must stay, within %d", got, lipgloss.Width(got), m.w-1)
	}
}

// Focusing the strip opens the table on the failed job, and the status line
// says what happened and the keys that act on it.
func TestActivityHintFailedJobKeys(t *testing.T) {
	v := newVtui(120, 24)
	v.event(map[string]any{"type": "ready", "tools": []any{"exec"}})
	now := float64(time.Now().Unix())
	v.event(map[string]any{"type": "jobs", "jobs": []any{
		map[string]any{"id": 3.0, "command": "go test ./...", "state": "running", "started": now - 120},
		map[string]any{"id": 4.0, "command": "make lint", "state": "failed", "code": 1.0, "started": now - 5, "ended": now - 4},
	}})
	v.m.setFocus(focusActivity)
	if r := v.m.actSelected(); r == nil || r.key != (actKey{id: 4}) {
		t.Fatalf("the table should open on the failed job, selected %+v", r)
	}
	if !strings.Contains(v.view(), "✗ job 4 failed (exit 1) · enter log · c clear done") {
		t.Errorf("the hint should name the failure and its keys:\n%s", v.view())
	}
	v.m.actMove(1) // the running job: the general keys again
	if !strings.Contains(v.view(), "x kill job") {
		t.Errorf("a running job's hint names x:\n%s", v.view())
	}
}

// A child whose plan node is blocked says so and why, in the attention
// colour, and sorts above the working rows; dund sends the why (blocked).
func TestActivityBlockedAgentShown(t *testing.T) {
	v := newVtui(120, 24)
	v.event(map[string]any{"type": "ready", "tools": []any{"exec"}})
	v.event(map[string]any{"type": "agents", "agents": []any{
		map[string]any{"id": 1.0, "state": "running", "prompt": "build it"},
		map[string]any{"id": 2.0, "state": "idle", "prompt": "deploy", "blocked": "needs the deploy token"},
	}})
	strip := ansi.Strip(v.m.activityView())
	if !strings.HasPrefix(strip, "▸ ⚠ agent #2 blocked: needs the deploy token · shift+tab, enter: open") {
		t.Errorf("strip %q", strip)
	}
	v.m.setFocus(focusActivity)
	rows := v.m.actRows()
	if rows[0].key != (actKey{agent: true, id: 2}) || v.m.actSel != rows[0].key {
		t.Fatalf("the blocked agent sorts first and is selected: %+v sel %+v", rows[0].key, v.m.actSel)
	}
	view := v.view()
	if !strings.Contains(view, "⚠ #2 idle  blocked: needs the deploy token") {
		t.Errorf("the table row says blocked and why:\n%s", view)
	}
	if !strings.Contains(view, "⚠ agent #2 blocked: needs the deploy token · enter open its conversation") {
		t.Errorf("the hint says why and the key:\n%s", view)
	}
	// A dismissed child's node is the main agent's: no attention.
	v.event(map[string]any{"type": "agents", "agents": []any{
		map[string]any{"id": 2.0, "state": "dismissed", "blocked": "needs the deploy token"},
	}})
	if strings.Contains(ansi.Strip(v.m.activityView()), "blocked") {
		t.Errorf("a dismissed agent is not blocked: %q", ansi.Strip(v.m.activityView()))
	}
}
