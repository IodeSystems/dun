package dun

import (
	"context"
	"strings"
	"testing"
	"time"
)

// The user's case: the model asked to wait ten minutes for an analysis, and the
// user wants to talk to it now. Their message ends the wait — the job is
// promoted, keeps running, and the message rides the handoff, so the model has
// the job number and the question in one result.
func TestExec_SayDuringWaitHandsOverAtOnce(t *testing.T) {
	h := testHarness(t)
	d := withLiftedQueue(execDispatcher(t, h), h)

	go func() {
		waitFor(t, 5*time.Second, func() bool { return len(h.liveJobs()) == 1 })
		time.Sleep(200 * time.Millisecond)
		h.Say("how far along is it?")
	}()
	start := time.Now()
	out, err := d(context.Background(), execCall(`{"command":"echo row 1; sleep 30","timeout":600}`))
	if err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("the message must end the wait, not the timeout: %s", el)
	}
	for _, want := range []string{"STILL RUNNING", "background job #1", "user wrote", "[user] how far along is it?"} {
		if !strings.Contains(out, want) {
			t.Errorf("result missing %q:\n%s", want, out)
		}
	}
	js := h.Jobs()
	if len(js) != 1 || js[0].State != "running" {
		t.Fatalf("the job must keep running: %+v", js)
	}
	if _, err := h.KillJob(1, "test"); err != nil {
		t.Fatal(err)
	}
}

// A message typed while the model was still writing the call is just as stuck
// behind the wait, so one already queued releases it too.
func TestExec_MessageQueuedBeforeTheCallReleasesIt(t *testing.T) {
	h := testHarness(t)
	d := withLiftedQueue(execDispatcher(t, h), h)

	h.Say("stop and look")
	start := time.Now()
	out, _ := d(context.Background(), execCall(`{"command":"sleep 30","timeout":600}`))
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("a queued message must not wait out the grace: %s", el)
	}
	if !strings.Contains(out, "background job #1") || !strings.Contains(out, "[user] stop and look") {
		t.Fatalf("want the handoff and the message: %q", out)
	}
	if _, err := h.KillJob(1, "test"); err != nil {
		t.Fatal(err)
	}
}

// With nothing waiting, Say only queues: no job, no channel, nothing closed.
func TestSay_NoWaiterChangesNothing(t *testing.T) {
	h := testHarness(t)
	h.Say("hello")
	if got := h.notes(); len(got) != 1 || got[0] != "hello" {
		t.Fatalf("Say must queue the message and nothing else: %q", got)
	}
	if js := h.Jobs(); len(js) != 0 {
		t.Fatalf("no jobs expected: %+v", js)
	}
	h.noteMu.Lock()
	ch := h.userWaitCh
	h.noteMu.Unlock()
	if ch != nil {
		t.Fatal("Say must not leave a wait channel behind")
	}
	// Once delivered, a new wait is not released by the old message.
	_ = h.liftQueued("result")
	select {
	case <-h.userWaiting():
		t.Fatal("a delivered message must not release the next wait")
	default:
	}
}

// A command that finishes inside its grace answers inline as always; a message
// typed after that does nothing to it.
func TestExec_FinishedBeforeTheWakeAnswersInline(t *testing.T) {
	h := testHarness(t)
	d := withLiftedQueue(execDispatcher(t, h), h)

	out, err := d(context.Background(), execCall(`{"command":"echo done-fast","timeout":600}`))
	if err != nil {
		t.Fatal(err)
	}
	h.Say("too late")
	if !strings.Contains(out, "done-fast") || strings.Contains(out, "STILL RUNNING") {
		t.Fatalf("want the normal output: %q", out)
	}
	if strings.Contains(out, "too late") {
		t.Fatalf("a message typed after the result cannot be in it: %q", out)
	}
	if js := h.Jobs(); len(js) != 0 {
		t.Fatalf("a finished command must not become a job: %+v", js)
	}
}

// One message releases every exec that is waiting, not just the first.
func TestExec_SayReleasesEveryWait(t *testing.T) {
	h := testHarness(t)
	d := withLiftedQueue(execDispatcher(t, h), h)

	outs := make(chan string, 2)
	for range 2 {
		go func() {
			out, _ := d(context.Background(), execCall(`{"command":"sleep 30","timeout":600}`))
			outs <- out
		}()
	}
	waitFor(t, 5*time.Second, func() bool { return len(h.liveJobs()) == 2 })
	time.Sleep(200 * time.Millisecond)
	h.Say("both of you")

	lifted := 0
	for range 2 {
		select {
		case out := <-outs:
			if !strings.Contains(out, "STILL RUNNING") {
				t.Errorf("want a handoff: %q", out)
			}
			if strings.Contains(out, "[user] both of you") {
				lifted++
			}
		case <-time.After(3 * time.Second):
			t.Fatal("a wait was not released")
		}
	}
	if lifted != 1 {
		t.Errorf("the message must be delivered exactly once, got %d", lifted)
	}
	for _, id := range []int{1, 2} {
		if _, err := h.KillJob(id, "test"); err != nil {
			t.Fatal(err)
		}
	}
}

// agent_monitor(wait:true) is the same wait over a child: the message ends it,
// and the child keeps running.
func TestAgentMonitor_SayEndsTheWait(t *testing.T) {
	h, sa := testAgent(t, 1, "do the thing")
	h.agMu.Lock()
	if h.agents == nil {
		h.agents = map[int]*subAgent{}
	}
	h.agents[sa.num] = sa
	h.agMu.Unlock()
	sa.mu.Lock()
	sa.done = make(chan struct{})
	sa.startRunLocked()
	sa.state = agentRunning
	sa.mu.Unlock()

	id := sa.num
	out := make(chan string, 1)
	go func() { out <- agentMonitor(context.Background(), h, &id, 0, "", true, false, false) }()
	time.Sleep(200 * time.Millisecond)
	h.Say("status?")
	select {
	case r := <-out:
		if !strings.Contains(r, "user wrote") {
			t.Errorf("the report must say why it stopped waiting: %s", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("agent_monitor(wait:true) did not stop for the message")
	}
}
