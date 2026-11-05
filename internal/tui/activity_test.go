package tui

import (
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
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

// The open table is most recent activity first, as of when it opened: what
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
	want := []actKey{{id: 2}, {agent: true, id: 2}, {id: 1}, {agent: true, id: 1}}
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
