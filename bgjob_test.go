package dun

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
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

// buffer_bytes is the chattiness bound: report only once enough has piled up.
func TestBgJob_ProgressOnlyAfterBufferBytes(t *testing.T) {
	h := testHarness(t)
	j := testJob(h, 2, "build")
	j.mon = bgMonitor{BufferBytes: 20}

	j.Write([]byte("short\n"))
	if n := h.notes(); len(n) != 0 {
		t.Fatalf("under the threshold must stay quiet: %q", n)
	}
	j.Write([]byte("now this is definitely more than twenty bytes\n"))
	n := h.notes()
	if len(n) != 1 {
		t.Fatalf("crossing the threshold should report exactly once, got %d: %q", len(n), n)
	}
	if !strings.Contains(n[0], "short") || !strings.Contains(n[0], "twenty bytes") {
		t.Errorf("a report must carry everything held back so far: %q", n[0])
	}
}

// A partial line is not reportable: the model cannot act on half a line and a
// regexp cannot judge one.
func TestBgJob_HoldsPartialLines(t *testing.T) {
	h := testHarness(t)
	j := testJob(h, 3, "build")
	j.mon = bgMonitor{BufferBytes: 1}

	j.Write([]byte("no newline yet"))
	if n := h.notes(); len(n) != 0 {
		t.Fatalf("half a line must not be reported: %q", n)
	}
	j.Write([]byte(" — and now there is\n"))
	n := h.notes()
	if len(n) != 1 || !strings.Contains(n[0], "no newline yet — and now there is") {
		t.Fatalf("the line should arrive whole once it is complete: %q", n)
	}
}

func TestBgJob_GrepLimitsWhatIsReported(t *testing.T) {
	h := testHarness(t)
	j := testJob(h, 4, "test")
	j.mon = bgMonitor{BufferBytes: 1, Grep: "FAIL"}
	j.re = regexp.MustCompile("FAIL")

	j.Write([]byte("ok pkg/a\nFAIL pkg/b\nok pkg/c\n"))
	n := h.notes()
	if len(n) != 1 {
		t.Fatalf("want one report, got %q", n)
	}
	if !strings.Contains(n[0], "FAIL pkg/b") {
		t.Errorf("the matching line must be reported: %q", n[0])
	}
	if strings.Contains(n[0], "ok pkg/a") {
		t.Errorf("non-matching lines must be filtered out: %q", n[0])
	}
}

// ignore is for a job whose answer stopped mattering: no progress AND no
// completion, or it is not actually muted.
func TestBgJob_IgnoreMutesCompletionToo(t *testing.T) {
	h := testHarness(t)
	j := testJob(h, 5, "build")
	j.mon = bgMonitor{BufferBytes: 1, Ignore: true}

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
	j := h.startBackgroundJob(HostExec{Dir: t.TempDir()}, "echo hello-bg; exit 2")

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
	// A REF, not a path. "grep it with exec" names a host file the model's
	// container cannot open under --docker, and one it has no shell for when
	// there is no exec backend at all.
	if !strings.Contains(notes[0], `ref "job1"`) || !strings.Contains(notes[0], "recap({ref:") {
		t.Errorf("the notice must name a readable ref, not a path: %q", notes[0])
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

// `ignore` means the model stopped caring about this job's outcome, and a
// heartbeat is exactly the outcome-adjacent noise it asked not to hear.
func TestBgJob_HeartbeatRespectsMute(t *testing.T) {
	h := testHarness(t)
	j := testJob(h, 8, "sleep 300")
	j.mon = bgMonitor{Ignore: true}
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
func fastHeartbeat() *heartbeat {
	return &heartbeat{
		sched: []time.Duration{10 * time.Millisecond},
		every: 20 * time.Millisecond,
		last:  time.Now(),
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

	h.startBackgroundJob(HostExec{Dir: t.TempDir()}, "echo hello-bg; exit 2")

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

	j := h.startBackgroundJob(HostExec{Dir: t.TempDir()}, "echo quiet")
	j.mu.Lock()
	j.mon.Ignore = true
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

func TestExecMonitor_ListsJobsAndAppliesSettings(t *testing.T) {
	h := testHarness(t)
	if out := execMonitor(h, nil, nil, nil, nil); !strings.Contains(out, "No background jobs") {
		t.Errorf("an empty list should say so: %q", out)
	}

	j := testJob(h, 9, "build")
	h.bgMu.Lock()
	h.bgJobs = map[int]*bgJob{9: j}
	h.bgMu.Unlock()

	id, buf := 9, 100
	grep, ignore := "ERROR", false
	out := execMonitor(h, &id, &buf, &grep, &ignore)
	if !strings.Contains(out, "#9 RUNNING") {
		t.Errorf("the report should lead with the job's state: %q", out)
	}
	j.mu.Lock()
	mon, re := j.mon, j.re
	j.mu.Unlock()
	if mon.BufferBytes != 100 || mon.Grep != "ERROR" || re == nil {
		t.Errorf("settings were not applied: %+v", mon)
	}

	bad := "([unclosed"
	if out := execMonitor(h, &id, nil, &bad, nil); !strings.Contains(out, "not a valid regexp") {
		t.Errorf("a bad pattern must be reported, not swallowed: %q", out)
	}
}

// The reminder is DEBOUNCED by anything the job itself says. A job reporting
// progress is already evidence of life, so a chatty job should never trigger a
// reminder at all — and the reminder must not reset the silence it is reporting
// on, or it would only ever fire once.
func TestBgJob_HeartbeatIsDebouncedByTheJobsOwnOutput(t *testing.T) {
	h := testHarness(t)
	j := testJob(h, 10, "chatty")
	j.mon = bgMonitor{BufferBytes: 1}
	j.hb = fastHeartbeat()
	go j.heartbeat()
	defer j.finish(ExecResult{})

	// Keep talking for well past several reminder intervals.
	deadline := time.Now().Add(120 * time.Millisecond)
	for time.Now().Before(deadline) {
		j.Write([]byte("still going\n"))
		time.Sleep(5 * time.Millisecond)
	}
	for _, n := range h.notes() {
		if strings.Contains(n, "STILL RUNNING") {
			t.Fatalf("a job that keeps reporting must not also be reminded about: %q", n)
		}
	}

	// Stop talking, and the reminder comes back.
	waitFor(t, 3*time.Second, func() bool {
		for _, n := range h.notes() {
			if strings.Contains(n, "STILL RUNNING") {
				return true
			}
		}
		return false
	})
}

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
