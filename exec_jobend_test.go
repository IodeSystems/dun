package dun

import (
	"context"
	"strings"
	"testing"
	"time"
)

// The Terminal-Bench case: job #1 runs, and the model waits beside it in exec
// (`sleep 280`). The job's finish ends that wait at once: the exec becomes job
// #2, and #1's completion notice is in the result — delivered there and
// nowhere else.
func TestExec_JobFinishCutsAWaitingExec(t *testing.T) {
	h := testHarness(t)
	d := withLiftedQueue(execDispatcher(t, h), h)
	be := &HostShell{Dir: t.TempDir()}
	t.Cleanup(be.Close)
	h.newJobStarted(be, "sleep 0.5; echo key-found")

	start := time.Now()
	out, err := d(context.Background(), execCall(`{"command":"sleep 30","timeout":600}`))
	if err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el > 5*time.Second {
		t.Fatalf("job #1 finishing must end the wait: %s", el)
	}
	for _, want := range []string{"background job #2", "other background work finished",
		"[background] background job #1 succeeded", "key-found"} {
		if !strings.Contains(out, want) {
			t.Errorf("result missing %q:\n%s", want, out)
		}
	}
	if n := strings.Count(out, "background job #1 succeeded"); n != 1 {
		t.Errorf("the notice must be in the result once, got %d:\n%s", n, out)
	}
	// Not delivered again: nothing is left to lift or to flush.
	if n := h.notes(); len(n) != 0 {
		t.Errorf("the notice was delivered in the result; nothing may be left queued: %q", n)
	}
	if n := h.flushQueued(); n != 0 {
		t.Errorf("flushQueued must find nothing to publish, found %d", n)
	}
	if _, err := h.KillJob(2, "test"); err != nil {
		t.Fatal(err)
	}
}

// A muted job says nothing when it finishes, so it does not cut the wait.
func TestExec_MutedJobFinishDoesNotCut(t *testing.T) {
	h := testHarness(t)
	d := withLiftedQueue(execDispatcher(t, h), h)
	be := &HostShell{Dir: t.TempDir()}
	t.Cleanup(be.Close)
	j := h.newJobStarted(be, "sleep 0.3")
	mute := true
	tuneJob(h, j, nil, nil, &mute)

	start := time.Now()
	out, _ := d(context.Background(), execCall(`{"command":"sleep 2; echo waited-out","timeout":600}`))
	if el := time.Since(start); el < 1500*time.Millisecond {
		t.Fatalf("a muted job's finish must not end the wait: %s\n%s", el, out)
	}
	if !strings.Contains(out, "waited-out") || strings.Contains(out, "still running") {
		t.Fatalf("want the command's own output inline: %q", out)
	}
	if js := h.Jobs(); len(js) != 1 {
		t.Fatalf("the waited exec must not become a job: %+v", js)
	}
}

// A progress report is not an end: it does not cut the wait.
func TestExec_ProgressReportDoesNotCut(t *testing.T) {
	h := testHarness(t)
	d := withLiftedQueue(execDispatcher(t, h), h)
	be := &HostShell{Dir: t.TempDir()}
	t.Cleanup(be.Close)
	j := h.newJobStarted(be, "for i in 1 2 3 4 5 6; do echo tick $i; sleep 0.3; done; sleep 30")
	t.Cleanup(func() { j.killAndWait("test") })
	every := 1
	tuneJob(h, j, &every, nil, nil)

	start := time.Now()
	out, _ := d(context.Background(), execCall(`{"command":"sleep 2; echo waited-out","timeout":600}`))
	if el := time.Since(start); el < 1500*time.Millisecond {
		t.Fatalf("a progress report must not end the wait: %s\n%s", el, out)
	}
	if !strings.Contains(out, "waited-out") || strings.Contains(out, "other background work finished") {
		t.Fatalf("want the command's own output inline: %q", out)
	}
	if !strings.Contains(out, "still running") {
		t.Errorf("the reports still ride the result: %q", out)
	}
}

// A sub-agent's report goes through the same release (subAgent.settleReporting).
func TestNotifyEnded_ReleasesTheWait(t *testing.T) {
	h := testHarness(t)
	ch := h.workEnded()
	h.notifyEnded("agent #1 is idle")
	select {
	case <-ch:
	default:
		t.Fatal("an end must release the wait")
	}
	// A new wait is not released by the old end.
	select {
	case <-h.workEnded():
		t.Fatal("a past end must not release the next wait")
	default:
	}
	if n := h.notes(); len(n) != 1 || n[0] != "agent #1 is idle" {
		t.Fatalf("the report is queued once: %q", n)
	}
}
