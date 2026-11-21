package tui

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/iodesystems/dun/internal/dundcli"
	"github.com/iodesystems/dun/internal/procps"
)

// fakePS is a process source: a reading per call, the CPU of each pid as set.
type fakePS struct {
	calls atomic.Int32
	cpu   map[int]float64
	gone  map[int]bool
}

func (f *fakePS) sample() (dundcli.PSView, error) {
	f.calls.Add(1)
	var rows []procps.Row
	for _, r := range []struct {
		pid        int
		group, job string
	}{{100, "s1", "engine"}, {101, "s1", "shell › make"}, {300, procps.GroupNone, "gopls (orphan)"}} {
		if f.gone[r.pid] {
			continue
		}
		rows = append(rows, procps.Row{PID: r.pid, Group: r.group, Session: r.group, Job: r.job,
			CPU: f.cpu[r.pid], RSS: 100 << 20, ReadBps: 10, WritBps: 20, AgeSec: 90})
	}
	return dundcli.PSView{At: time.Now(), Window: 2, Groups: procps.Groups(rows), Sessions: 1}, nil
}

// psOpen opens /ps over f and takes its first reading.
func psOpen(t *testing.T, f *fakePS) tuiModel {
	t.Helper()
	m := newTUIModel(&dunProc{stdin: &recWC{}}, "/ws")
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = nm.(tuiModel)
	m.psSample = f.sample
	cmd := psSlash(&m, nil)
	return psFeed(t, m, cmd)
}

// psFeed runs cmd (a reading) and gives its message to the model.
func psFeed(t *testing.T, m tuiModel, cmd tea.Cmd) tuiModel {
	t.Helper()
	if cmd == nil {
		t.Fatal("no command")
	}
	nm, _ := m.Update(cmd())
	return nm.(tuiModel)
}

func TestPSScreen_Render(t *testing.T) {
	f := &fakePS{cpu: map[int]float64{100: 5, 101: 20, 300: 0.5}}
	m := psOpen(t, f)
	if m.activeScreen() == nil {
		t.Fatal("no screen")
	}
	plain := ansi.Strip(m.drawView())
	for _, want := range []string{"/ps", "PID", "CPU%", "MEM", "IO R/W", "engine", "shell › make", "gopls (orphan)", "100M", "20.0", "3 processes"} {
		if !strings.Contains(plain, want) {
			t.Errorf("lacks %q:\n%s", want, plain)
		}
	}
	if strings.Index(plain, "shell › make") > strings.Index(plain, "engine") {
		t.Errorf("not by CPU, most first:\n%s", plain)
	}
}

// A reading schedules the next one; the tick takes it; a screen closed in
// between ends the chain without another reading.
func TestPSScreen_RefreshStopsOnClose(t *testing.T) {
	f := &fakePS{cpu: map[int]float64{100: 5, 101: 20}}
	m := psOpen(t, f)
	if f.calls.Load() != 1 {
		t.Fatalf("%d readings after open", f.calls.Load())
	}
	// The reading is followed by a tick (not run here: it is a timer).
	nm, tick := m.Update(psMsg{gen: m.psGen, v: mustPS(t, f)})
	m = nm.(tuiModel)
	if tick == nil {
		t.Fatal("no next tick while open")
	}
	// The tick takes the next reading, off Update.
	nm, read := m.Update(psTickMsg{gen: m.psGen})
	m = nm.(tuiModel)
	if read == nil {
		t.Fatal("tick did not start a reading")
	}
	before := f.calls.Load()
	// Close the screen with a reading in flight and a tick pending.
	m.popScreen()
	msg := read()
	nm, next := m.Update(msg)
	m = nm.(tuiModel)
	if next != nil {
		t.Error("a reading after close scheduled another tick")
	}
	nm, again := m.Update(psTickMsg{gen: m.psGen})
	m = nm.(tuiModel)
	if again != nil {
		t.Error("a tick after close started a reading")
	}
	if f.calls.Load() != before+1 {
		t.Errorf("readings %d, want %d (the one in flight)", f.calls.Load(), before+1)
	}
	// Reopened: the old opening's tick does nothing.
	old := m.psGen
	_ = psSlash(&m, nil)
	if _, c := m.notePSTick(psTickMsg{gen: old}); c != nil {
		t.Error("a tick of an earlier opening started a reading")
	}
}

func mustPS(t *testing.T, f *fakePS) dundcli.PSView {
	t.Helper()
	v, err := f.sample()
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// Moving freezes the order: a refresh keeps it and the selected process; a
// new process goes last, an exited one drops; t sorts again.
func TestPSScreen_FrozenOrder(t *testing.T) {
	f := &fakePS{cpu: map[int]float64{100: 5, 101: 20, 300: 0.5}, gone: map[int]bool{}}
	m := psOpen(t, f)
	pos := func(s string) int { return strings.Index(ansi.Strip(m.drawView()), s) }
	m = typeKey(t, m, "j") // select the second row (engine)
	if v := m.psPaging(); v.order == nil || v.pids[m.pg.cur] != 100 {
		t.Fatalf("not frozen on 100: %+v", v.pids)
	}
	if !strings.Contains(ansi.Strip(m.drawView()), "sort paused") {
		t.Error("no sort paused marker")
	}
	f.cpu[100] = 90 // live sorting would put it first
	nm, _ := m.Update(psMsg{gen: m.psGen, v: mustPS(t, f)})
	m = nm.(tuiModel)
	if pos("shell › make") > pos("engine") || m.psPaging().pids[m.pg.cur] != 100 {
		t.Errorf("order or selection moved:\n%s", ansi.Strip(m.drawView()))
	}
	f.gone[101] = true
	nm, _ = m.Update(psMsg{gen: m.psGen, v: mustPS(t, f)})
	m = nm.(tuiModel)
	if pos("shell › make") >= 0 || m.psPaging().pids[m.pg.cur] != 100 {
		t.Errorf("exited process stays or selection lost:\n%s", ansi.Strip(m.drawView()))
	}
	m = typeKey(t, m, "t")
	if m.psPaging().order != nil || strings.Contains(ansi.Strip(m.drawView()), "sort paused") {
		t.Error("t did not go back to live sorting")
	}
}
