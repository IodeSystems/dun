package dun

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/agentkit/llm"
)

// jobsCall dispatches one call of name (jobs or an alias) with args.
func jobsCall(t *testing.T, h *Harness, name, args string) string {
	t.Helper()
	d := withJobs(func(context.Context, llm.ToolCall) (string, error) { return "inner", nil }, h, nil)
	var tc llm.ToolCall
	tc.Function.Name, tc.Function.Arguments = name, args
	out, err := d(context.Background(), tc)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// registerAgent puts sa in h's agent list, as spawnAgent does.
func registerAgent(h *Harness, sa *subAgent) {
	h.agMu.Lock()
	if h.agents == nil {
		h.agents = map[int]*subAgent{}
	}
	h.agents[sa.num] = sa
	h.agMu.Unlock()
}

// The list is both kinds, typed, one line each.
func TestJobs_ListsBothKinds(t *testing.T) {
	h, sa := testAgent(t, 2, "count the lines")
	if out := jobsCall(t, h, "jobs", `{}`); out != "No background jobs or sub-agents." {
		t.Errorf("empty: %q", out)
	}
	registerAgent(h, sa)
	be := &HostShell{Dir: t.TempDir()}
	t.Cleanup(be.Close)
	j := h.newJobStarted(be, "sleep 30")
	t.Cleanup(func() { j.killAndWait("test") })

	out := jobsCall(t, h, "jobs", `{}`)
	lines := strings.Split(out, "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "job #1 RUNNING") || !strings.Contains(lines[0], "sleep 30") ||
		!strings.HasPrefix(lines[1], "agent #2") || !strings.Contains(lines[1], "count the lines") {
		t.Errorf("list:\n%s", out)
	}
}

// jobs manages, it does not read: a job's report says its state and how
// much it wrote, and where the log is; the output itself is not in it.
func TestJobs_ReportsNoOutput(t *testing.T) {
	h := testHarness(t)
	be := &HostShell{Dir: t.TempDir()}
	t.Cleanup(be.Close)
	j := h.newJobStarted(be, "for i in 1 2 3; do echo line-$i; done")
	<-j.doneCh
	out := jobsCall(t, h, "jobs", fmt.Sprintf(`{"job":%d}`, j.id))
	for _, want := range []string{"succeeded", "written: 21 B, 3 lines", "Output: ", "(file_read)"} {
		if !strings.Contains(out, want) {
			t.Errorf("lacks %q: %s", want, out)
		}
	}
	if strings.Contains(out, "line-2") {
		t.Errorf("the report carries output: %s", out)
	}
}

// What jobs no longer takes (tail, wait, grep, the agent ops, the old names)
// is refused with what it does take; nothing is silently ignored into a
// different call.
func TestJobs_RefusesWhatItNoLongerTakes(t *testing.T) {
	h := testHarness(t)
	for _, args := range []string{`{"job":1,"tail":5}`, `{"job":1,"wait":true}`, `{"job":1,"grep":"x"}`,
		`{"agent":1,"tell":"hi"}`, `{"job":1,"buffer_bytes":10}`, `{"job":1,"ignore":true}`} {
		out := jobsCall(t, h, "jobs", args)
		if !strings.HasPrefix(out, "ERROR: jobs takes job, kill, mute, every_bytes, every_secs") {
			t.Errorf("%s: %q", args, out)
		}
	}
	if out := jobsCall(t, h, "jobs", `{"kill":true}`); !strings.Contains(out, "name the job") {
		t.Errorf("kill with no job: %q", out)
	}
	if out := jobsCall(t, h, "exec_monitor", `{}`); out != "inner" {
		t.Errorf("the old alias still dispatches here: %q", out)
	}
}

// The schema is management only, the same for a root and a child: agents are
// the agent tool's.
func TestJobs_SchemaIsManagementOnly(t *testing.T) {
	props := func(td llm.ToolDef) []string {
		var ks []string
		for k := range td.Function.Parameters.(map[string]any)["properties"].(map[string]any) {
			ks = append(ks, k)
		}
		slices.Sort(ks)
		return ks
	}
	want := []string{"every_bytes", "every_secs", "job", "kill", "mute"}
	for _, agents := range []bool{true, false} {
		if got := props(jobsToolDef(true, agents)); !slices.Equal(got, want) {
			t.Errorf("agents=%v: %v", agents, got)
		}
	}
}

// Every_bytes and every_secs report that output was written and how much,
// never the output; every_secs reports a burst that is followed by silence.
func TestJobs_ProgressSaysHowMuchNotWhat(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	h := testHarness(t)
	be := &HostShell{Dir: t.TempDir()}
	t.Cleanup(be.Close)
	j := h.newJobStarted(be, "sleep 0.3; echo shh | tr s z; sleep 30") // writes "zhh", not in the command
	t.Cleanup(func() { j.killAndWait("test") })
	jobsCall(t, h, "jobs", fmt.Sprintf(`{"job":%d,"every_secs":1}`, j.id))
	deadline := time.Now().Add(4 * time.Second)
	var got string
	for time.Now().Before(deadline) && got == "" {
		for _, n := range h.notes() {
			if strings.Contains(n, "since the last report") {
				got = n
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !strings.Contains(got, "+4 B, +1 line") || strings.Contains(got, "zhh") {
		t.Fatalf("progress report: %q", got)
	}
}

// agent({id, …}) acts on an existing child; a call that is neither shape says
// both, and prompt with id is refused (it would start nothing).
func TestAgent_ActsOnAnExistingChildByID(t *testing.T) {
	h, sa := testAgent(t, 3, "look around")
	registerAgent(h, sa)
	d := withAgent(func(context.Context, llm.ToolCall) (string, error) { return "inner", nil }, h, nil)
	call := func(args string) string {
		var tc llm.ToolCall
		tc.Function.Name, tc.Function.Arguments = "agent", args
		out, _ := d(context.Background(), tc)
		return out
	}
	if out := call(`{"model":"x"}`); !strings.Contains(out, "or {id, tell?") || !strings.Contains(out, "You sent {model}") {
		t.Errorf("neither shape: %q", out)
	}
	if out := call(`{"id":3,"prompt":"new"}`); !strings.Contains(out, "leave id out") {
		t.Errorf("prompt with id: %q", out)
	}
	if out := call(`{"id":3,"dismiss":true}`); !strings.Contains(out, "agent #3") {
		t.Errorf("dismiss: %q", out)
	}
	if state, _, _, _ := sa.snapshot(); state != agentDismissed {
		t.Errorf("state after dismiss: %s", state)
	}
}

// Asked the same thing again with nothing changed (only the clock), the
// second read of a running command waits for it instead of answering the
// same again: a poll cannot loop, and it waits before the loop guard's third
// call would be refused (USER 2026-10-27: the loop guard stopped a session
// polling agent_monitor).
func TestJobs_ARepeatedPollWaits(t *testing.T) {
	h := testHarness(t)
	be := &HostShell{Dir: t.TempDir()}
	t.Cleanup(be.Close)
	j := h.newJobStarted(be, "sleep 1.5; echo at-last")
	q := fmt.Sprintf(`{"job":%d}`, j.id)
	if out := jobsCall(t, h, "jobs", q); !strings.Contains(out, "RUNNING") || strings.Contains(out, "waited") {
		t.Fatalf("first read: %s", out)
	}
	start := time.Now()
	out := jobsCall(t, h, "jobs", q)
	if time.Since(start) < 500*time.Millisecond || !strings.Contains(out, "waited until it did") || !strings.Contains(out, "written: 8 B") {
		t.Errorf("the second read did not wait (%s): %s", time.Since(start), out)
	}
	// A different question in between is not a repeat.
	j2 := h.newJobStarted(be, "sleep 30")
	t.Cleanup(func() { j2.killAndWait("test") })
	q2 := fmt.Sprintf(`{"job":%d}`, j2.id)
	jobsCall(t, h, "jobs", q2)
	jobsCall(t, h, "jobs", `{}`)
	if out := jobsCall(t, h, "jobs", q2); strings.Contains(out, "waited") {
		t.Errorf("a read after another question waited: %s", out)
	}
}

// A wait a message broke ends the run of identical calls: the loop guard must
// not refuse the wait that follows it as a call nothing happened between.
func TestJobs_AMessageEndsTheRun(t *testing.T) {
	h := testHarness(t)
	be := &HostShell{Dir: t.TempDir()}
	t.Cleanup(be.Close)
	j := h.newJobStarted(be, "sleep 30")
	t.Cleanup(func() { j.killAndWait("test") })
	q := fmt.Sprintf(`{"job":%d}`, j.id)
	jobsCall(t, h, "jobs", q) // the first look; the next identical one waits
	h.loop.observe("jobs", q)
	h.loop.observe("jobs", q)
	out := make(chan string, 1)
	go func() { out <- jobsCall(t, h, "jobs", q) }()
	time.Sleep(200 * time.Millisecond)
	h.Say("status?")
	select {
	case r := <-out:
		if !strings.Contains(r, jobsMessageStop) {
			t.Fatalf("not stopped by the message: %s", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the wait did not stop for the message")
	}
	if n := h.loop.observe("jobs", q); n != 1 {
		t.Errorf("run is %d after the message, want 1", n)
	}
}

// jobs is listed from the first request (a tool joining at the first
// background job changed the prompt's prefix — USER 2026-10-28); with
// nothing running it says so.
func TestJobs_ListedFromTheStart(t *testing.T) {
	ws := t.TempDir()
	h, err := Start(context.Background(), Config{Workspace: ws, Client: &scriptRunner{}, Exec: &HostShell{Dir: ws}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if !slices.Contains(h.ToolNames(), "jobs") {
		t.Fatalf("jobs not listed: %v", h.ToolNames())
	}
	if out := jobsCall(t, h, "jobs", `{}`); out != "No background jobs or sub-agents." {
		t.Errorf("with nothing running: %q", out)
	}
}

// jobs lists what this agent started — its commands and its sub-agents —
// and nothing of anyone else's: a parent does not see its child's commands,
// a child sees neither its parent's nor (having no agents) any agent. An
// agent's row says how full its context is (USER 2026-10-28).
func TestJobs_ListsOnlyWhatThisAgentStarted(t *testing.T) {
	h := startParentOf(t, &scriptRunner{rounds: []roundFn{textRound("done")}})
	sa := spawnKid(t, h, "say done")
	waitIdle(t, sa)
	be := &HostShell{Dir: t.TempDir()}
	t.Cleanup(be.Close)
	mine := h.newJobStarted(be, "sleep 31")
	t.Cleanup(func() { mine.killAndWait("test") })
	kb := &HostShell{Dir: t.TempDir()}
	t.Cleanup(kb.Close)
	theirs := sa.h.newJobStarted(kb, "sleep 32")
	t.Cleanup(func() { theirs.killAndWait("test") })

	parent := jobsCall(t, h, "jobs", `{}`)
	if !strings.Contains(parent, "sleep 31") || !strings.Contains(parent, "agent #1") || strings.Contains(parent, "sleep 32") {
		t.Errorf("the parent's list:\n%s", parent)
	}
	if !strings.Contains(parent, "ctx ") {
		t.Errorf("an agent's row should say how full its context is:\n%s", parent)
	}
	child := jobsCall(t, sa.h, "jobs", `{}`)
	if !strings.Contains(child, "sleep 32") || strings.Contains(child, "sleep 31") || strings.Contains(child, "agent #") {
		t.Errorf("the child's list:\n%s", child)
	}
}

// kill (or a monitor setting) with no job is refused with the running jobs
// and what their processes are doing, so the retry can name one: a
// Terminal-Bench model sent {"kill":true} alone three times in 54 trials.
func TestJobs_NoJobNamesTheRunningOnes(t *testing.T) {
	h := testHarness(t)
	if out := jobsCall(t, h, "jobs", `{"kill":true}`); !strings.Contains(out, "name the job") ||
		!strings.Contains(out, "No background command is running.") {
		t.Errorf("none running: %q", out)
	}
	be := &HostShell{Dir: t.TempDir()}
	t.Cleanup(be.Close)
	done := h.newJobStarted(be, "true")
	<-done.doneCh
	j := h.newJobStarted(be, "sleep 30")
	t.Cleanup(func() { j.killAndWait("test") })
	deadline := time.Now().Add(3 * time.Second)
	for j.info().PGID == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond) // processes are read once the job knows its group
	}

	out := jobsCall(t, h, "jobs", `{"kill":true}`)
	if !strings.HasPrefix(out, "ERROR: name the job: job:N. Running now:\n") ||
		!strings.Contains(out, fmt.Sprintf("job #%d RUNNING", j.id)) || !strings.Contains(out, "processes: ") {
		t.Errorf("running list: %q", out)
	}
	if strings.Contains(out, fmt.Sprintf("#%d succeeded", done.id)) {
		t.Errorf("a finished job is listed as one to name: %q", out)
	}
	j.mu.Lock()
	killed := j.done
	j.mu.Unlock()
	if killed {
		t.Error("kill with no job killed something")
	}
}
