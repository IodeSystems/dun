package psview

import (
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/iodesystems/dun/internal/dundcli"
	"github.com/iodesystems/dun/internal/procps"
	"github.com/iodesystems/dun/proto"
)

const sesID = "20261021-222836"

// fixture is a reading: one session (engine and shell), the poly-lsp
// daemon with two roots, an orphan.
type fixture struct {
	at   time.Time
	cpu  map[int]float64
	gone map[int]bool
}

func newFixture() *fixture {
	return &fixture{at: time.Now(), cpu: map[int]float64{100: 5, 101: 20, 200: 1, 300: 0.5}, gone: map[int]bool{}}
}

func (f *fixture) sample() (dundcli.PSView, procps.Snapshot, error) {
	start := time.Unix(1_700_000_000, 0)
	snap := procps.Snapshot{At: f.at, Procs: map[int]*procps.Proc{
		1:   {PID: 1, Argv: []string{"/sbin/init"}},
		90:  {PID: 90, PPID: 1, Argv: []string{"dun", "dund", "serve"}, Start: start},
		100: {PID: 100, PPID: 90, Argv: []string{"dun", "--engine"}, Start: start},
		101: {PID: 101, PPID: 100, Argv: []string{"bash", "-c", strings.Repeat("make test ", 30)}, Start: start},
		200: {PID: 200, PPID: 1, Argv: []string{"/usr/bin/poly-lsp-mcp", "daemon", "--daemon-home=/h"}, Start: start},
		300: {PID: 300, PPID: 1, Argv: []string{"gopls"}, Start: start},
	}}
	row := func(pid int, group, ses, job string, rss int64, age int64) procps.Row {
		return procps.Row{PID: pid, PPID: snap.Procs[pid].PPID, Group: group, Session: ses, Job: job,
			Argv: strings.Join(snap.Procs[pid].Argv, " "), CPU: f.cpu[pid], RSS: rss, ReadBps: 10, WritBps: 20, AgeSec: age}
	}
	daemon := row(200, procps.GroupShared, procps.GroupShared, "poly-lsp daemon", 500<<20, 1000)
	daemon.Roots = []procps.Root{
		{Root: "/home/u/src/dun", Langs: []string{"go"}, Children: 1, Running: 1, IdleSec: 4, Files: 900, Bytes: 4 << 20, Group: sesID, Session: "1021-2228 dun"},
		{Root: "/home/u/src/" + strings.Repeat("very-long-tree-name-", 6), Langs: []string{"typescript"}, IdleSec: -1, Files: -1, Group: procps.GroupNone, Session: procps.GroupNone},
	}
	rows := []procps.Row{
		row(100, sesID, "1021-2228 dun", "engine", 100<<20, 300),
		row(101, sesID, "1021-2228 dun", "shell › bash -c "+strings.Repeat("make test ", 30), 10<<20, 100),
		daemon,
		row(300, procps.GroupNone, procps.GroupNone, "gopls (orphan) /home/u/src/"+strings.Repeat("x", 80), 50<<20, 5000),
	}
	var kept []procps.Row
	for _, r := range rows {
		if !f.gone[r.PID] {
			kept = append(kept, r)
		}
	}
	for pid := range f.gone {
		delete(snap.Procs, pid)
	}
	v := dundcli.PSView{At: f.at, Window: 2, Groups: procps.Groups(kept), Sessions: 1,
		List: &proto.ProcsList{Sessions: []proto.ProcSession{{ID: sesID, Workspace: "/home/u/src/dun", EnginePID: 100}}}}
	return v, snap, nil
}

// calls records the stubbed actions.
type calls struct {
	kills    []string
	stops    []string
	restarts [][]string
}

func setup(t *testing.T) (*model, *fixture, *calls) {
	t.Helper()
	f := newFixture()
	c := &calls{}
	act := Actions{
		Kill: func(pid int, force bool) error {
			s := "TERM"
			if force {
				s = "KILL"
			}
			c.kills = append(c.kills, s+" "+strconv.Itoa(pid))
			return nil
		},
		Stop:    func(id string) error { c.stops = append(c.stops, id); return nil },
		Restart: func(argv []string) error { c.restarts = append(c.restarts, argv); return nil },
	}
	m := newModel(f.sample, act, 2*time.Second, func(int) string { return "/home/u/src/dun" })
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	refresh(m, f)
	return m, f, c
}

func refresh(m *model, f *fixture) {
	v, s, err := f.sample()
	m.Update(sampleMsg{v: v, snap: s, err: err})
}

func press(m *model, keys ...string) tea.Cmd {
	var cmd tea.Cmd
	for _, k := range keys {
		cmd = m.key(k)
	}
	return cmd
}

// run runs cmd and feeds its message back, as the program does.
func run(t *testing.T, m *model, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		t.Fatal("no command")
	}
	m.Update(cmd())
}

// selectPID moves the selection onto pid's row.
func selectPID(t *testing.T, m *model, pid int) {
	t.Helper()
	for i, it := range m.items() {
		if it.kind == kProc && it.row.PID == pid {
			m.moveTo(i)
			return
		}
	}
	t.Fatalf("pid %d not listed", pid)
}

func firstProcs(m *model) []int {
	var out []int
	for _, it := range m.items() {
		if it.kind == kProc {
			out = append(out, it.row.PID)
		}
	}
	return out
}

func TestSortCycle(t *testing.T) {
	m, _, _ := setup(t)
	if got := firstProcs(m); got[0] != 101 || got[1] != 100 {
		t.Fatalf("CPU order %v, want the session's shell (20%%) first", got)
	}
	if !strings.Contains(m.render(), "[CPU]") {
		t.Error("the header does not show the sort")
	}
	press(m, "S")
	if m.sort != sortMEM || firstProcs(m)[0] != 200 {
		t.Fatalf("MEM: sort %v, first %v; want the daemon (500M) first", m.sort, firstProcs(m))
	}
	if r := m.render(); !strings.Contains(r, "[MEM]") || !strings.Contains(r, "MEM↓") {
		t.Error("the header does not show MEM as the sort")
	}
	press(m, "S")
	if m.sort != sortIO {
		t.Fatalf("sort %v, want IO", m.sort)
	}
	press(m, "S")
	if m.sort != sortAGE || firstProcs(m)[0] != 300 {
		t.Fatalf("AGE: first %v, want the oldest (300)", firstProcs(m))
	}
	press(m, "S")
	if m.sort != sortCPU {
		t.Fatalf("sort %v after four presses, want CPU again", m.sort)
	}
}

func TestCollapseGroup(t *testing.T) {
	m, _, _ := setup(t)
	if it, ok := m.selected(); !ok || it.kind != kGroup || it.group.Group != sesID {
		t.Fatalf("first selection %+v, want the session's header", m.sel)
	}
	n := len(m.items())
	press(m, "enter")
	if len(m.items()) != n-2 || !strings.Contains(m.render(), "▸ 1021-2228 dun") {
		t.Fatalf("folded: %d items (was %d)", len(m.items()), n)
	}
	refresh(m, newFixture())
	if !m.folded[sesID] {
		t.Error("a reading unfolded the group")
	}
	press(m, "space")
	if len(m.items()) != n || !strings.Contains(m.render(), "▾ 1021-2228 dun") {
		t.Fatalf("space did not open the group: %d items", len(m.items()))
	}
}

// The selection stays on its process when the sort moves it, and goes to
// a neighbour when the process ends.
func TestSelectionByIdentity(t *testing.T) {
	m, f, _ := setup(t)
	selectPID(t, m, 100)
	f.cpu[100] = 90
	refresh(m, f)
	if m.sel.pid != 100 || firstProcs(m)[0] != 100 {
		t.Fatalf("sel %+v, order %v", m.sel, firstProcs(m))
	}
	f.gone[100] = true
	refresh(m, f)
	if _, ok := m.selected(); !ok {
		t.Fatal("no selection after the selected process ended")
	}
}

func TestKillConfirm(t *testing.T) {
	m, f, c := setup(t)
	selectPID(t, m, 101)
	press(m, "k")
	if m.ask == nil || !strings.Contains(m.render(), "send SIGTERM to 101") {
		t.Fatal("k asked nothing")
	}
	press(m, "n")
	if m.ask != nil || len(c.kills) != 0 {
		t.Fatalf("n: ask %v, kills %v", m.ask, c.kills)
	}
	press(m, "k")
	if press(m, "x") != nil || m.ask == nil {
		t.Fatal("another key answered the question")
	}
	run(t, m, press(m, "y"))
	if len(c.kills) != 1 || c.kills[0] != "TERM 101" {
		t.Fatalf("kills %v", c.kills)
	}
	if !strings.Contains(m.render(), "SIGTERM sent to 101") {
		t.Error("no status after the kill")
	}
	// Still there a reading later: SIGKILL is offered.
	f.at = time.Now().Add(3 * time.Second)
	refresh(m, f)
	if m.ask == nil || !strings.Contains(m.ask.prompt, "SIGKILL") {
		t.Fatalf("no SIGKILL offer: %+v", m.ask)
	}
	run(t, m, press(m, "y"))
	if len(c.kills) != 2 || c.kills[1] != "KILL 101" {
		t.Fatalf("kills %v", c.kills)
	}
	// A process that went away is not offered SIGKILL.
	selectPID(t, m, 100)
	press(m, "k")
	run(t, m, press(m, "y"))
	f.gone[100] = true
	f.at = time.Now().Add(3 * time.Second)
	refresh(m, f)
	if m.ask != nil {
		t.Fatalf("SIGKILL offered for an ended process: %s", m.ask.prompt)
	}
	// k on a group's header kills nothing.
	m.moveTo(0)
	press(m, "k")
	if m.ask != nil || !m.statusErr {
		t.Error("k on a header asked")
	}
}

func TestRestartConfirm(t *testing.T) {
	m, _, c := setup(t)
	selectPID(t, m, 101)
	press(m, "r")
	if m.ask != nil || !m.statusErr {
		t.Fatal("r on another row asked to restart")
	}
	selectPID(t, m, 200)
	if !strings.Contains(m.render(), "r restart") {
		t.Error("the daemon row's hints do not offer r")
	}
	press(m, "r")
	if m.ask == nil || !strings.Contains(m.ask.prompt, "daemon --restart") {
		t.Fatal("r on the daemon asked nothing")
	}
	press(m, "esc")
	if m.ask != nil || len(c.restarts) != 0 {
		t.Fatal("esc did not cancel")
	}
	press(m, "r")
	run(t, m, press(m, "y"))
	if len(c.restarts) != 1 || c.restarts[0][0] != "/usr/bin/poly-lsp-mcp" {
		t.Fatalf("restarts %v", c.restarts)
	}
	if !strings.Contains(m.status, "restarted") {
		t.Errorf("status %q", m.status)
	}
}

func TestStopAndOpen(t *testing.T) {
	m, _, c := setup(t)
	selectPID(t, m, 200)
	press(m, "s")
	if m.ask != nil || !strings.Contains(m.status, "not a session") {
		t.Fatalf("s on (shared): ask %v, status %q", m.ask, m.status)
	}
	selectPID(t, m, 100)
	press(m, "s")
	if m.ask == nil {
		t.Fatal("s asked nothing")
	}
	run(t, m, press(m, "y"))
	if len(c.stops) != 1 || c.stops[0] != sesID {
		t.Fatalf("stops %v", c.stops)
	}
	cmd := press(m, "o")
	if m.open == nil || m.open.id != sesID || m.open.workspace != "/home/u/src/dun" {
		t.Fatalf("open %+v", m.open)
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Error("o did not quit the view")
	}
}

func TestDetailsAndMouse(t *testing.T) {
	m, _, _ := setup(t)
	selectPID(t, m, 101)
	press(m, "enter")
	r := m.render()
	for _, want := range []string{"─ details 101", "argv", "cwd      ~/src/dun", "parents  100 dun › 90 dun › 1 init"} {
		if !strings.Contains(r, want) && !strings.Contains(r, strings.Replace(want, "~/src/dun", "/home/u/src/dun", 1)) {
			t.Errorf("details lack %q:\n%s", want, r)
		}
	}
	press(m, "esc")
	if m.details {
		t.Fatal("esc did not close the details")
	}
	// A click selects the line under it; on a header it folds the group.
	m.Update(tea.MouseClickMsg{X: 5, Y: headRows + 1, Button: tea.MouseLeft})
	if it, _ := m.selected(); it.kind != kProc || m.selIdx != 1 {
		t.Fatalf("click selected %+v", m.sel)
	}
	m.Update(tea.MouseClickMsg{X: 5, Y: headRows, Button: tea.MouseLeft})
	if !m.folded[sesID] {
		t.Error("a click on the header did not fold it")
	}
	// The wheel scrolls a short screen and keeps the selection on it.
	m.folded[sesID] = false
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 7})
	m.moveTo(0)
	m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	if m.off == 0 || m.selIdx < m.off || m.selIdx >= m.off+m.listH() {
		t.Fatalf("wheel: off %d sel %d rows %d", m.off, m.selIdx, m.listH())
	}
}

func TestSparkRing(t *testing.T) {
	r := newRing(3)
	for i := 1; i <= 5; i++ {
		r.push(float64(i))
	}
	if got := r.values(); len(got) != 3 || got[0] != 3 || got[2] != 5 {
		t.Fatalf("values %v, want [3 4 5]", got)
	}
	if n := ringSize(2 * time.Second); n != 30 {
		t.Errorf("ringSize(2s) %d, want 30 (a minute)", n)
	}
	if n := ringSize(time.Second); n != 60 {
		t.Errorf("ringSize(1s) %d, want 60", n)
	}
	if s := sparkline(nil, 4); s != "    " {
		t.Errorf("empty %q", s)
	}
	if s := sparkline([]float64{0, 100}, 4); s != "  ▁█" {
		t.Errorf("two readings %q", s)
	}
	// Under the floor, low readings stay low.
	if s := sparkline([]float64{0.5, 1}, 2); s != "▁▁" {
		t.Errorf("idle %q", s)
	}
	// Thirty readings in ten cells: each cell its bucket's most.
	vals := make([]float64, 30)
	vals[29] = 50
	if s := []rune(sparkline(vals, 10)); len(s) != 10 || s[9] != '█' || s[0] != '▁' {
		t.Errorf("bucketed %q", string(s))
	}
	// The model keeps one ring per process, new on a reused pid.
	m, f, _ := setup(t)
	for i := 0; i < 40; i++ {
		f.at = f.at.Add(2 * time.Second)
		refresh(m, f)
	}
	if h := m.sparks[101]; h == nil || h.r.n != 30 {
		t.Fatalf("ring for 101: %+v", h)
	}
	f.gone[300] = true
	refresh(m, f)
	if m.sparks[300] != nil {
		t.Error("an ended process kept its ring")
	}
}

// No line is wider than the terminal; the screen is exactly its height.
func TestViewWidth(t *testing.T) {
	for _, w := range []int{60, 100, 160} {
		for _, details := range []bool{false, true} {
			m, _, _ := setup(t)
			m.Update(tea.WindowSizeMsg{Width: w, Height: 24})
			selectPID(t, m, 101)
			m.details = details
			press(m, "k") // the footer's question too
			lines := strings.Split(m.render(), "\n")
			if len(lines) != 24 {
				t.Errorf("w %d details %v: %d lines, want 24", w, details, len(lines))
			}
			for i, l := range lines {
				if lw := lipgloss.Width(l); lw > w {
					t.Errorf("w %d details %v line %d is %d wide: %q", w, details, i, lw, l)
				}
			}
		}
	}
}
