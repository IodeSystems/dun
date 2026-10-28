package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// The job view (USER 2026-10-23): the command on top, open by default and
// collapsible; the log as a tail panel that follows, scrolls and searches;
// ← comes back out.

// jobLog writes n numbered lines to a fresh log and returns its path.
func jobLog(t *testing.T, n int) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "job-3.log")
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, "output line %d\n", i)
	}
	if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func appendLog(t *testing.T, p, s string) {
	t.Helper()
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

// jobVtui is a session with one job whose log is at path, opened by keys:
// tab to the activity zone (its row highlighted), → into it.
func jobVtui(t *testing.T, path, command, state string, code int) *vtui {
	t.Helper()
	v := newVtui(80, 24)
	v.event(map[string]any{"type": "ready", "tools": []any{"exec"}})
	now := float64(time.Now().Unix())
	job := map[string]any{"id": 3.0, "command": command, "state": state, "log": path, "started": now - 75, "code": float64(code)}
	if state != "running" {
		job["ended"] = now - 3
	}
	v.event(map[string]any{"type": "jobs", "jobs": []any{job}})
	v.m = key(v.m, kTab)
	v.m = key(v.m, kTab)
	if v.m.focus != focusActivity {
		t.Fatalf("tab twice should reach the job, focus=%d", v.m.focus)
	}
	v.m = key(v.m, kRight)
	if !v.m.jobViewing {
		t.Fatalf("→ on the job should open its view:\n%s", v.view())
	}
	return v
}

// tick fires the view's read timer, as bubbletea would.
func tick(v *vtui) tea.Cmd {
	nm, cmd := v.m.Update(jobTailMsg{gen: v.m.jvGen})
	v.m = nm.(tuiModel)
	return cmd
}

func TestJobView_CommandOnTopOpenByDefaultAndCollapsible(t *testing.T) {
	long := "go test -race -count=1 -run 'TestSomethingLong|TestAnotherOne' ./cmd/dun/... ./internal/... && echo the end of a long command"
	v := jobVtui(t, jobLog(t, 3), long, "running", 0)
	view := v.view()
	if !strings.Contains(view, "▾ command") || !strings.Contains(view, "$ go test -race") {
		t.Fatalf("the command should be the top item, open:\n%s", view)
	}
	if !strings.Contains(view, "the end of a long command") {
		t.Fatalf("open, the whole command should show:\n%s", view)
	}
	if !strings.Contains(view, "running") || !strings.Contains(view, "1m1") {
		t.Errorf("the title should say running and for how long:\n%s", view)
	}
	// ↑ from the top of the output selects the command; enter shuts it.
	v.m = key(v.m, kUp)
	if !v.m.jv.onCmd {
		t.Fatal("↑ at the top of the output should select the command")
	}
	v.m = key(v.m, kEnter)
	view = v.view()
	if !strings.Contains(view, "▸ $ go test -race") || strings.Contains(view, "the end of a long command") {
		t.Fatalf("enter should shut the command to its head:\n%s", view)
	}
	v.m = key(v.m, kRight)
	if !v.m.jv.cmdOpen {
		t.Fatal("→ on the command should open it")
	}
	v.m = key(v.m, kLeft)
	if v.m.jv.cmdOpen || !v.m.jobViewing {
		t.Fatal("← on an open command should shut it and stay")
	}
	v.m = key(v.m, kLeft)
	if v.m.jobViewing {
		t.Fatal("← on a shut command should come back out")
	}
}

func TestJobView_TailFollowsWhileRunning(t *testing.T) {
	p := jobLog(t, 100)
	v := jobVtui(t, p, "make", "running", 0)
	if !strings.Contains(v.view(), "output line 99") || strings.Contains(v.view(), "output line 50") {
		t.Fatalf("the panel should open at the tail:\n%s", v.view())
	}
	if tick(v) == nil {
		t.Fatal("the read timer should re-arm while the view is open")
	}
	appendLog(t, p, "output line 100\noutput line 101\n")
	tick(v)
	if !strings.Contains(v.view(), "output line 101") {
		t.Fatalf("new output should scroll in:\n%s", v.view())
	}
	if !strings.Contains(v.view(), "following") {
		t.Errorf("the panel should say it is following:\n%s", v.view())
	}
	// A timer from an earlier opening stops rather than doubling up.
	nm, cmd := v.m.Update(jobTailMsg{gen: v.m.jvGen - 1})
	v.m = nm.(tuiModel)
	if cmd != nil {
		t.Error("a stale timer should not re-arm")
	}
}

func TestJobView_ScrollStopsFollowingAndGResumes(t *testing.T) {
	p := jobLog(t, 100)
	v := jobVtui(t, p, "make", "running", 0)
	for range 3 {
		v.m = key(v.m, kUp)
	}
	if v.m.jv.follow || v.m.jv.onCmd {
		t.Fatalf("↑ should scroll the output and stop following: follow=%v onCmd=%v", v.m.jv.follow, v.m.jv.onCmd)
	}
	off := v.m.jv.off
	appendLog(t, p, "output line 100\n")
	tick(v)
	if v.m.jv.off != off || strings.Contains(v.view(), "output line 100") {
		t.Fatalf("scrolled up, new output must not move the panel: off %d → %d", off, v.m.jv.off)
	}
	v.m = key(v.m, tea.KeyPressMsg{Code: tea.KeyPgUp})
	if v.m.jv.off >= off-1 {
		t.Fatalf("pgup should page up: off %d → %d", off, v.m.jv.off)
	}
	v.m = key(v.m, runesKey([]rune("G")))
	if !v.m.jv.follow || !strings.Contains(v.view(), "output line 100") {
		t.Fatalf("G should go to the end and follow again:\n%s", v.view())
	}
}

func TestJobView_Search(t *testing.T) {
	p := jobLog(t, 100)
	appendLog(t, p, "the end\n")
	v := jobVtui(t, p, "make", "failed", 2)
	if !strings.Contains(v.view(), "exit 2") {
		t.Errorf("a failed job should show its exit code:\n%s", v.view())
	}
	v.m = key(v.m, kSlash)
	v.m = typeStr(v.m, "line 1")
	if !v.m.jv.searching {
		t.Fatal("/ should open the search line")
	}
	v.m = key(v.m, kEnter)
	// "line 1", "line 10".."line 19" — 11 matches; the first at or after
	// the top of the panel wraps to the first in the log.
	if n := len(v.m.jv.matches); n != 11 {
		t.Fatalf("want 11 matches, got %d", n)
	}
	if !strings.Contains(v.view(), "match 1/11") || v.m.jv.off != 0 {
		t.Fatalf("the first match should be on screen with the count:\n%s", v.view())
	}
	if v.m.jv.follow {
		t.Error("a jump to a match should stop following")
	}
	v.m = key(v.m, kN)
	if v.m.jv.at != 1 || !strings.Contains(v.view(), "match 2/11") {
		t.Fatalf("n should step to the next match: at=%d\n%s", v.m.jv.at, v.view())
	}
	v.m = key(v.m, kEsc)
	if v.m.jv.query != "" || !v.m.jobViewing {
		t.Fatal("esc should clear the search first, and stay")
	}
	v.m = key(v.m, kEsc)
	if v.m.jobViewing {
		t.Fatal("a second esc should come back out")
	}
}

// ← on the output comes back out, to the activity zone with the job still
// highlighted; a killed job says so.
func TestJobView_LeftExits(t *testing.T) {
	v := jobVtui(t, jobLog(t, 5), "sleep 100", "killed", 0)
	if !strings.Contains(v.view(), "killed") {
		t.Errorf("a killed job should say so:\n%s", v.view())
	}
	if !strings.Contains(v.view(), "← or esc back") {
		t.Errorf("the status line should say how to leave:\n%s", v.view())
	}
	v.m = key(v.m, kLeft)
	if v.m.jobViewing || v.m.focus != focusActivity {
		t.Fatalf("← should leave the view for the activity zone: viewing=%v focus=%d", v.m.jobViewing, v.m.focus)
	}
	if sel := v.m.actSelected(); sel == nil || sel.key != (actKey{id: 3}) {
		t.Errorf("the job should still be highlighted, got %+v", sel)
	}
}

// A log line drawn as a terminal would: the last \r wins, escape codes go,
// tabs reach their stops.
func TestJobView_LogLine(t *testing.T) {
	for in, want := range map[string]string{
		"10%\r50%\r100%":     "100%",
		"done\r":             "done",
		"\x1b[31mred\x1b[0m": "red",
		"a\tb":               "a       b",
		"abcdefgh\tx":        "abcdefgh        x",
		"plain":              "plain",
	} {
		if got := logLine(in); got != want {
			t.Errorf("logLine(%q) = %q, want %q", in, got, want)
		}
	}
}

// The view never draws more rows than the pane, at any size.
func TestJobView_FitsThePane(t *testing.T) {
	v := jobVtui(t, jobLog(t, 200), strings.Repeat("very long command ", 40), "running", 0)
	for _, sz := range [][2]int{{80, 24}, {40, 12}, {120, 50}} {
		v.resize(sz[0], sz[1])
		if rows := strings.Split(v.view(), "\n"); len(rows) != sz[1] {
			t.Errorf("%dx%d: the screen is %d rows", sz[0], sz[1], len(rows))
		}
	}
}

// A log bigger than the tail is read from its last jobLogTail bytes, grows by
// increments, and is read again from the start when it is replaced.
func TestJobView_LoadKeepsTheTail(t *testing.T) {
	p := filepath.Join(t.TempDir(), "big.log")
	line := strings.Repeat("x", 99) + "\n"
	if err := os.WriteFile(p, []byte(strings.Repeat(line, jobLogTail/100+50)), 0o644); err != nil {
		t.Fatal(err)
	}
	v := newJobView(1)
	v.load(p)
	if !v.cut || len(v.data) > jobLogTail || len(v.data) == 0 {
		t.Fatalf("want the tail only: cut=%v %d bytes", v.cut, len(v.data))
	}
	before := len(v.data)
	appendLog(t, p, "the newest line\n")
	v.load(p)
	if !strings.HasSuffix(string(v.data), "the newest line\n") || len(v.data) > jobLogTail {
		t.Fatalf("the append should arrive and the cap hold: %d → %d bytes", before, len(v.data))
	}
	v.wrap(80)
	if last := v.rows[len(v.rows)-1]; last != "the newest line" {
		t.Errorf("last row %q", last)
	}
	if err := os.WriteFile(p, []byte("fresh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	v.load(p)
	v.wrap(80)
	if string(v.data) != "fresh\n" || len(v.rows) != 1 || v.rows[0] != "fresh" {
		t.Errorf("a replaced log should be read again: %q rows %q", v.data, v.rows)
	}
}

// A running job's view asks what its processes are doing and shows it under
// the command: one line, the verdict, and with p a row per process
// (USER 2026-10-28).
func TestJobView_ShowsWhatItsProcessesAreDoing(t *testing.T) {
	var rec strings.Builder
	v := jobVtui(t, jobLog(t, 3), "go test ./...", "running", 0)
	v.m.proc = &dunProc{stdin: bufCloser{&rec}}
	v.m.jv.usageAt = time.Time{}
	tick(v)
	if !strings.Contains(rec.String(), `"type":"job_usage"`) || !strings.Contains(rec.String(), `"job":3`) {
		t.Fatalf("an open running job should ask for its usage: %q", rec.String())
	}
	if !strings.Contains(v.view(), "processes (p)  reading…") {
		t.Errorf("before the answer:\n%s", v.view())
	}
	v.event(map[string]any{"type": "job_usage", "job": 3.0, "verdict": "idle: every process sleeping, no CPU, no IO, no output for 4m — looks stuck",
		"usage": map[string]any{"rated": true, "cpu": 0.0, "rss": 9e6, "threads": 2.0, "fds": 6.0, "read_bps": 0.0, "write_bps": 0.0,
			"procs": []any{
				map[string]any{"pid": 4100.0, "state": "S", "cpu": 0.0, "rss": 2e6, "threads": 1.0, "fds": 3.0, "read_bps": 0.0, "write_bps": 0.0, "wchan": "do_wait", "name": "sh"},
				map[string]any{"pid": 4101.0, "state": "S", "cpu": 0.0, "rss": 7e6, "threads": 1.0, "fds": 3.0, "read_bps": 0.0, "write_bps": 0.0, "wchan": "futex_wait", "name": "go"},
			}}})
	view := v.view()
	for _, want := range []string{"cpu 0% · 2 procs (S2)", "6 fds", "looks stuck"} {
		if !strings.Contains(view, want) {
			t.Errorf("lacks %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "futex_wait") {
		t.Error("the table is shut until p")
	}
	v.m = key(v.m, runesKey([]rune("p")))
	if view = v.view(); !strings.Contains(view, "futex_wait") || !strings.Contains(view, "4101") {
		t.Errorf("p should open a row per process:\n%s", view)
	}
}
