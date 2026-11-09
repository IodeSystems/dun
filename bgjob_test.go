package dun

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iodesystems/dun/internal/heartbeat"
)

// testHarness is the smallest Harness a background job needs: a store (Notify
// reads its callback) and the wake channel.
func testHarness(t *testing.T) *Harness {
	t.Helper()
	st, err := openSessionStore("")
	if err != nil {
		t.Fatal(err)
	}
	return &Harness{
		store: st,
		wake:  make(chan struct{}, 1),
		cfg:   Config{SessionFile: filepath.Join(t.TempDir(), "s.jsonl")},
	}
}

// testJob is a job built the way production builds one, with the id and the
// `bg` flag a test wants to pin. Tests must not hand-roll the struct literal: a
// job without its channels panics the moment anything finishes it, and one with
// bg unset is provisional, so it stays silent and proves nothing about the
// notifications these tests are about.
func testJob(h *Harness, id int, command string) *bgJob {
	j := h.newJob(command)
	j.id, j.bg = id, true
	return j
}

// notes drains what the harness has buffered for the model.
func (h *Harness) notes() []string {
	h.noteMu.Lock()
	defer h.noteMu.Unlock()
	out := make([]string, 0, len(h.queue))
	for _, q := range h.queue {
		out = append(out, q.text)
	}
	h.queue = nil
	return out
}

// The default has to stay silent. Streaming every background job by default
// would spend the context window narrating a build nobody asked about — the
// monitor exists so the model can OPT IN per job.
func TestBgJob_SilentUntilAsked(t *testing.T) {
	h := testHarness(t)
	j := testJob(h, 1, "build")

	j.Write([]byte("compiling a\ncompiling b\n"))
	if n := h.notes(); len(n) != 0 {
		t.Fatalf("an unmonitored job must not report progress: %q", n)
	}
	j.finish(ExecResult{Output: "compiling a\ncompiling b\n"})
	if note := j.completionNote(); note == "" {
		t.Fatal("completion is always reported unless the job is muted")
	}
}

// every_bytes is the chattiness bound: report once enough has been written,
// and say how much, not what (USER 2026-11-06: the text is the log's).
func TestBgJob_ProgressOnlyAfterEveryBytes(t *testing.T) {
	h := testHarness(t)
	j := testJob(h, 2, "build")
	j.mon = bgMonitor{EveryBytes: 20}

	j.Write([]byte("short\n"))
	if n := h.notes(); len(n) != 0 {
		t.Fatalf("under the threshold must stay quiet: %q", n)
	}
	j.Write([]byte("now this is definitely more than twenty bytes\n"))
	n := h.notes()
	if len(n) != 1 {
		t.Fatalf("crossing the threshold should report exactly once, got %d: %q", len(n), n)
	}
	if !strings.Contains(n[0], "+52 B, +2 lines") || strings.Contains(n[0], "twenty") {
		t.Errorf("a report counts everything since the last one, and carries no text: %q", n[0])
	}
}

// mute is for a job whose answer stopped mattering: no progress AND no
// completion, or it is not actually muted.
func TestBgJob_MuteSilencesCompletionToo(t *testing.T) {
	h := testHarness(t)
	j := testJob(h, 5, "build")
	j.mon = bgMonitor{EveryBytes: 1, Mute: true}

	j.Write([]byte("noise\n"))
	j.finish(ExecResult{Code: 1})
	if note := j.completionNote(); note != "" {
		t.Errorf("a muted job must not announce its completion: %q", note)
	}
	if n := h.notes(); len(n) != 0 {
		t.Errorf("a muted job must not report progress: %q", n)
	}
}

// End to end against a real command: the log file is written, the exit CODE
// survives, and the completion notice names both.
func TestStartBackground_LogsAndReportsTheExitCode(t *testing.T) {
	h := testHarness(t)
	j := h.newJobStarted(HostExec{Dir: t.TempDir()}, "echo hello-bg; exit 2")

	waitFor(t, 10*time.Second, func() bool { return strings.Contains(j.status(), "FAILED") })

	if j.res.Code != 2 {
		t.Errorf("exit code lost: %+v", j.res)
	}
	if j.logPath == "" {
		t.Fatal("no log file was created")
	}
	b, err := os.ReadFile(j.logPath)
	if err != nil {
		t.Fatalf("log unreadable: %v", err)
	}
	if !strings.Contains(string(b), "hello-bg") {
		t.Errorf("the log should hold the output: %q", b)
	}
	notes := h.notes()
	if len(notes) != 1 {
		t.Fatalf("want one completion notice, got %q", notes)
	}
	if !strings.Contains(notes[0], "FAILED (exit 2)") {
		t.Errorf("the notice must carry the exit code: %q", notes[0])
	}
	// The log's path in the isolation tmp, read with file_read: the same
	// file on the host and under --docker (isotmp.go). Not "grep it with
	// exec", which needs a shell that may not see the file.
	if !strings.Contains(notes[0], "job-1.log") || !strings.Contains(notes[0], "(file_read)") {
		t.Errorf("the notice must name the log and how to read it: %q", notes[0])
	}
	if strings.Contains(notes[0], "grep it with exec") {
		t.Error("the notice must not tell the model to shell out for a file it may not be able to reach")
	}
}

// A background job is exempt from the foreground deadline, so nothing bounds it
// and nothing announced it: a job wedged in second two produced exactly the same
// output as one doing honest work — none. The heartbeat is what separates them.
func TestBgJob_HeartbeatSaysStillRunning(t *testing.T) {
	h := testHarness(t)
	j := testJob(h, 7, "sleep 300")
	j.hb = fastHeartbeat()
	go j.heartbeat()

	waitFor(t, 3*time.Second, func() bool { return len(h.notes()) > 0 })

	// It keeps going: one reminder is a notification, a heartbeat is a series.
	waitFor(t, 3*time.Second, func() bool {
		for _, n := range h.notes() {
			if strings.Contains(n, "STILL RUNNING") && strings.Contains(n, "sleep 300") {
				return true
			}
		}
		return false
	})

	// And it stops with the job, rather than reminding forever about something
	// that finished.
	j.finish(ExecResult{})
	time.Sleep(60 * time.Millisecond)
	h.notes() // drain whatever was in flight as it exited
	time.Sleep(80 * time.Millisecond)
	if n := h.notes(); len(n) != 0 {
		t.Errorf("a finished job must stop reminding: %q", n)
	}
}

// `mute` means the model stopped caring about this job's outcome, and a
// heartbeat is exactly the outcome-adjacent noise it asked not to hear.
func TestBgJob_HeartbeatRespectsMute(t *testing.T) {
	h := testHarness(t)
	j := testJob(h, 8, "sleep 300")
	j.mon = bgMonitor{Mute: true}
	j.hb = fastHeartbeat()
	go j.heartbeat()
	defer j.finish(ExecResult{})

	time.Sleep(120 * time.Millisecond)
	if n := h.notes(); len(n) != 0 {
		t.Errorf("a muted job must not send heartbeats: %q", n)
	}
}

// fastHeartbeat is the real schedule compressed, so a reminder test runs in
// milliseconds. Per-instance, never a global: writing the package schedule
// while a loop reads it is a data race, which is how the first version failed.
func fastHeartbeat() *heartbeat.Beat {
	return &heartbeat.Beat{
		Sched: []time.Duration{10 * time.Millisecond},
		Every: 20 * time.Millisecond,
		Last:  time.Now(),
	}
}

func waitFor(t *testing.T, limit time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("condition never held within %s", limit)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The human's view of a job is pushed, not polled: once on start (so a running
// job is visible while it runs) and once on finish (so a row never sits on
// "running" after the process is gone). Nothing in between — a push per output
// chunk would repaint on every line of a build log.
func TestJobs_PushedOnStartAndOnFinish(t *testing.T) {
	h := testHarness(t)
	var mu sync.Mutex
	var pushes [][]JobInfo
	h.cfg.OnJobs = func(js []JobInfo) {
		mu.Lock()
		pushes = append(pushes, js)
		mu.Unlock()
	}
	last := func() []JobInfo {
		mu.Lock()
		defer mu.Unlock()
		if len(pushes) == 0 {
			return nil
		}
		return pushes[len(pushes)-1]
	}

	h.newJobStarted(HostExec{Dir: t.TempDir()}, "echo hello-bg; exit 2")

	first := last()
	if len(first) != 1 || first[0].State != "running" {
		t.Fatalf("start must push one running job, got %+v", first)
	}
	if first[0].Log == "" {
		t.Error("the row must carry the log path — it is pushed AFTER the log is opened")
	}
	if first[0].Started == 0 || first[0].Ended != 0 {
		t.Errorf("a running job has a start and no end: %+v", first[0])
	}
	if first[0].Command != "echo hello-bg; exit 2" {
		t.Errorf("the row must carry the command that started it: %q", first[0].Command)
	}

	waitFor(t, 10*time.Second, func() bool {
		l := last()
		return len(l) == 1 && l[0].State != "running"
	})
	fin := last()[0]
	// The verdict is the exit CODE, and the row carries it as a token rather
	// than prose so the UI switches instead of matching strings.
	if fin.State != "failed" || fin.Code != 2 {
		t.Errorf("the row must carry the exit code as a state: %+v", fin)
	}
	if fin.Ended < fin.Started {
		t.Errorf("a finished job needs an end time: %+v", fin)
	}
}

// `ignore` silences the MODEL, not the human. A muted job that vanished from
// the pane would be indistinguishable from a lost one.
func TestJobs_MutedJobIsStillPushed(t *testing.T) {
	h := testHarness(t)
	var mu sync.Mutex
	var finished bool
	h.cfg.OnJobs = func(js []JobInfo) {
		mu.Lock()
		for _, j := range js {
			if j.State != "running" {
				finished = true
			}
		}
		mu.Unlock()
	}

	j := h.newJobStarted(HostExec{Dir: t.TempDir()}, "echo quiet")
	j.mu.Lock()
	j.mon.Mute = true
	j.mu.Unlock()

	waitFor(t, 10*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return finished
	})
	if n := h.notes(); len(n) != 0 {
		t.Errorf("a muted job must still say nothing to the model: %q", n)
	}
}

// jobs(job:N, …) tunes a command's job and reports on it.
func TestJobs_TuneACommandsJob(t *testing.T) {
	h := testHarness(t)
	j := testJob(h, 9, "build")
	h.bgMu.Lock()
	h.bgJobs = map[int]*bgJob{9: j}
	h.bgMu.Unlock()

	out := jobsCall(t, h, "jobs", `{"job":9,"every_bytes":100,"every_secs":30,"mute":false}`)
	if !strings.Contains(out, "#9 RUNNING") || !strings.Contains(out, "every 100 B or 30s") {
		t.Errorf("the report should lead with the job's state and say its monitor: %q", out)
	}
	j.mu.Lock()
	mon := j.mon
	j.mu.Unlock()
	if mon.EveryBytes != 100 || mon.EverySecs != 30 || mon.Mute {
		t.Errorf("settings were not applied: %+v", mon)
	}
}

// The reminder is DEBOUNCED by anything the job itself says. A job reporting
// progress is already evidence of life, so a chatty job should never trigger a
// reminder at all — and the reminder must not reset the silence it is reporting
// on, or it would only ever fire once.
//
// The beat runs on a stepped clock: with real sleeps (a write every 5ms
// against a 10ms reminder) a loaded CI runner held the writer past the
// interval and the reminder fired, correctly, at a job that had gone quiet
// (the v0.4.0 release run).
func TestBgJob_HeartbeatIsDebouncedByTheJobsOwnOutput(t *testing.T) {
	h := testHarness(t)
	j := testJob(h, 10, "chatty")
	j.mon = bgMonitor{EveryBytes: 1}
	clk := newStepClock()
	j.hb = &heartbeat.Beat{Sched: []time.Duration{10 * time.Millisecond}, Every: 20 * time.Millisecond,
		Last: clk.Now(), Now: clk.Now, Sleep: clk.Sleep}
	go j.heartbeat()
	defer clk.release()
	defer j.finish(ExecResult{})

	reminded := func() int {
		n := 0
		for _, s := range h.notes() {
			if strings.Contains(s, "STILL RUNNING") {
				n++
			}
		}
		return n
	}
	// Keep talking, each time before the reminder is due, for many intervals.
	d := clk.slept()
	for range 50 {
		clk.add(d / 2)
		j.Write([]byte("still going\n"))
		clk.add(d - d/2)
		d = clk.step()
	}
	if n := reminded(); n != 0 {
		t.Fatalf("a job that keeps reporting must not also be reminded about: %d reminders", n)
	}

	// Stop talking, and the reminder comes back; and again, since a reminder
	// is not the job speaking.
	for i := range 2 {
		clk.add(d)
		d = clk.step()
		if n := reminded(); n != 1 {
			t.Fatalf("quiet interval %d: %d reminders, want 1", i+1, n)
		}
	}
}

// stepClock is a heartbeat.Beat clock the test steps: Sleep hands the
// duration to the test and blocks until step.
type stepClock struct {
	mu    sync.Mutex
	t     time.Time
	sleep chan time.Duration
	wake  chan struct{}
}

func newStepClock() *stepClock {
	return &stepClock{t: time.Unix(1_800_000_000, 0), sleep: make(chan time.Duration), wake: make(chan struct{})}
}

func (c *stepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *stepClock) Sleep(d time.Duration) {
	c.sleep <- d
	<-c.wake
}

func (c *stepClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// slept waits for the beat to sleep and returns for how long.
func (c *stepClock) slept() time.Duration {
	select {
	case d := <-c.sleep:
		return d
	case <-time.After(10 * time.Second):
		panic("the heartbeat never slept")
	}
}

// step wakes the beat and waits for its next sleep: whatever it fired in
// between has been queued.
func (c *stepClock) step() time.Duration {
	c.wake <- struct{}{}
	return c.slept()
}

// release lets a sleeping beat run to its end (after the job finished).
func (c *stepClock) release() { close(c.wake) }

// A heartbeat reports the ABSENCE of news, and must not buy a turn to do it.
//
// It used to go through notifyAndWake: the wake ran a full autonomous turn, and
// the turn (until the suggestion trigger moved to the UI) bought a second call
// on top. A session with one long job and nobody typing therefore billed two
// requests per reminder, starting one minute in, to say nothing had happened.
func TestBgJob_HeartbeatDoesNotBuyATurn(t *testing.T) {
	h := testHarness(t)
	j := testJob(h, 11, "sleep 300")
	j.hb = fastHeartbeat()
	go j.heartbeat()
	defer j.finish(ExecResult{})

	// The human still hears about it...
	waitFor(t, 3*time.Second, func() bool {
		for _, n := range h.notes() {
			if strings.Contains(n, "STILL RUNNING") {
				return true
			}
		}
		return false
	})
	// ...but it was queued as an ASIDE (never a reason to run) and no wake was
	// posted. The wake is the operative half: it is what drives continueTurn.
	h.noteMu.Lock()
	kinds := make([]queuedKind, 0, len(h.queue))
	for _, q := range h.queue {
		kinds = append(kinds, q.kind)
	}
	h.noteMu.Unlock()
	for _, k := range kinds {
		if k != queuedAside {
			t.Errorf("a reminder must queue as an aside, got kind %d", k)
		}
	}
	select {
	case <-h.Wake():
		t.Error("a reminder must not wake the turn loop")
	default:
	}
}

// The completion of a job is real news and must still wake a turn — otherwise
// the result the model asked for waits for something else to happen first.
// This is j.notify, the path the runner actually takes (finished() only builds
// the line; the goroutine that owns the job publishes it).
func TestBgJob_CompletionStillWakesATurn(t *testing.T) {
	h := testHarness(t)
	j := testJob(h, 12, "true")
	j.hb = fastHeartbeat()
	j.finish(ExecResult{Output: "done"})
	h.noteMu.Lock()
	n, kind := len(h.queue), queuedAside
	if n > 0 {
		kind = h.queue[0].kind
	}
	h.noteMu.Unlock()
	if n == 0 || kind != queuedNotification {
		t.Errorf("a finished job is news, not an aside: %d queued, kind %d", n, kind)
	}
	select {
	case <-h.Wake():
	default:
		t.Error("a finished job must wake the turn loop")
	}
}

// A running job's reply says what its processes are doing (USER 2026-10-28):
// a sleeping command reads as idle, with its fds and where it waits.
func TestJobs_ReplyCarriesProcessUsage(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the full set is read from /proc")
	}
	h := testHarness(t)
	be := &HostShell{Dir: t.TempDir()}
	t.Cleanup(be.Close)
	j := h.newJobStarted(be, "sleep 30")
	t.Cleanup(func() { j.killAndWait("test") })
	deadline := time.Now().Add(3 * time.Second)
	for j.info().PGID == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if j.info().PGID == 0 {
		t.Fatal("the job never learned its process group")
	}
	out := jobReport(j)
	if !strings.Contains(out, "processes: cpu 0%") || !strings.Contains(out, "fds") || !strings.Contains(out, "idle") {
		t.Fatalf("no usage in the reply:\n%s", out)
	}
	t.Log(out)
}

// The handoff says ending the turn is safe (every root driver waits for a
// running job and wakes the model), names the progress report, and says the
// monitor sees only stdout/stderr (Terminal-Bench: a model slept beside a job
// whose output went to files). A sub-agent's run ends with its turn, so it is
// told that instead.
func TestHandoffNotice_SaysEndingTheTurnIsSafe(t *testing.T) {
	h := testHarness(t)
	j := &bgJob{id: 3, command: "make", h: h}
	out := j.handoffNotice(15 * time.Minute)
	for _, want := range []string{"background job #3", "Do not sleep or poll", "ending your turn is safe",
		"wakes you when it finishes", "jobs({job: 3, every_secs: 60})", "every_bytes", "stdout/stderr", "written to a file"} {
		if !strings.Contains(out, want) {
			t.Errorf("handoff missing %q:\n%s", want, out)
		}
	}
	child := &bgJob{id: 1, command: "make", h: &Harness{parent: h}}
	out = child.handoffNotice(time.Minute)
	if strings.Contains(out, "is safe") || !strings.Contains(out, "your run ends") {
		t.Errorf("a sub-agent must not be told ending its turn is safe:\n%s", out)
	}
}
