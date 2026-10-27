package dun

import (
	"context"
	"fmt"
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

// wait blocks until a command finishes and reports its outcome; the job's
// own finish notice is not sent as well. A message ends the wait early.
func TestJobs_WaitOnACommand(t *testing.T) {
	h := testHarness(t)
	be := &HostShell{Dir: t.TempDir()}
	t.Cleanup(be.Close)
	j := h.newJobStarted(be, "sleep 0.5; echo finished-now")
	start := time.Now()
	out := jobsCall(t, h, "jobs", fmt.Sprintf(`{"job":%d,"wait":true}`, j.id))
	if time.Since(start) < 300*time.Millisecond {
		t.Errorf("wait returned before the command finished: %s", out)
	}
	if !strings.Contains(out, "succeeded") || !strings.Contains(out, "finished-now") {
		t.Errorf("wait result: %q", out)
	}
	for _, n := range h.notes() {
		if strings.Contains(n, "finished-now") {
			t.Errorf("the finish was reported twice, as a notice too: %q", n)
		}
	}

	slow := h.newJobStarted(be, "sleep 30")
	t.Cleanup(func() { slow.killAndWait("test") })
	res := make(chan string, 1)
	go func() { res <- jobsCall(t, h, "jobs", fmt.Sprintf(`{"job":%d,"wait":true}`, slow.id)) }()
	time.Sleep(200 * time.Millisecond)
	h.Say("status?")
	select {
	case r := <-res:
		if !strings.Contains(r, "user wrote") || !strings.Contains(r, "RUNNING") {
			t.Errorf("an interrupted wait must say why and that it still runs: %s", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("jobs(job:N, wait:true) did not stop for the message")
	}
}

// tail is the last N lines of a command's log.
func TestJobs_TailOfACommandLog(t *testing.T) {
	h := testHarness(t)
	be := &HostShell{Dir: t.TempDir()}
	t.Cleanup(be.Close)
	j := h.newJobStarted(be, "for i in 1 2 3 4 5 6; do echo line$i; done")
	<-j.doneCh
	out := jobsCall(t, h, "jobs", fmt.Sprintf(`{"job":%d,"tail":2}`, j.id))
	if !strings.Contains(out, "last 2 lines:\nline5\nline6") || strings.Contains(out, "line4") {
		t.Errorf("tail: %q", out)
	}
}

// kill dismisses an agent (agent_monitor's quit, which still works).
func TestJobs_KillAnAgent(t *testing.T) {
	h, sa := testAgent(t, 3, "look around")
	registerAgent(h, sa)
	if out := jobsCall(t, h, "jobs", `{"agent":3,"kill":true}`); !strings.Contains(out, "agent #3") {
		t.Errorf("kill: %q", out)
	}
	if state, _, _, _ := sa.snapshot(); state != agentDismissed {
		t.Errorf("state after kill: %s", state)
	}

	_, sb := testAgent(t, 4, "again")
	sb.parent = h
	registerAgent(h, sb)
	jobsCall(t, h, "agent_monitor", `{"agent":4,"quit":true}`)
	if state, _, _, _ := sb.snapshot(); state != agentDismissed {
		t.Errorf("agent_monitor quit: %s", state)
	}
}

// A parameter for the other kind is an error that says what does apply.
func TestJobs_KindMismatchErrors(t *testing.T) {
	h, sa := testAgent(t, 1, "x")
	registerAgent(h, sa)
	cases := map[string]string{
		`{"job":1,"agent":1}`:          "not both",
		`{"job":1,"tell":"hi"}`:        "takes wait, tail, kill, grep",
		`{"job":1,"resume":true}`:      "for a sub-agent",
		`{"agent":1,"grep":"x"}`:       "takes wait, tail, kill, tell, resume",
		`{"agent":1,"ignore":true}`:    "for a command's job",
		`{"kill":true}`:                "name the job",
		`{"wait":true}`:                "name the job",
		`{"agent":1,"buffer_bytes":1}`: "for a command's job",
	}
	for args, want := range cases {
		out := jobsCall(t, h, "jobs", args)
		if !strings.HasPrefix(out, "ERROR") || !strings.Contains(out, want) {
			t.Errorf("%s: %q, want %q", args, out, want)
		}
	}
}

// The old names still dispatch, and are not in the tool list.
func TestJobs_AliasesDispatch(t *testing.T) {
	h := testHarness(t)
	be := &HostShell{Dir: t.TempDir()}
	t.Cleanup(be.Close)
	j := h.newJobStarted(be, "echo hi")
	<-j.doneCh
	if out := jobsCall(t, h, "exec_monitor", fmt.Sprintf(`{"job":%d}`, j.id)); !strings.Contains(out, "succeeded") {
		t.Errorf("exec_monitor: %q", out)
	}
	if out := jobsCall(t, h, "agent_monitor", `{}`); !strings.Contains(out, "job #1") {
		t.Errorf("agent_monitor list: %q", out)
	}
	if out := jobsCall(t, h, "other", `{}`); out != "inner" {
		t.Errorf("another tool must fall through: %q", out)
	}
	root := jobsToolDef(true, true)
	child := jobsToolDef(true, false)
	props := func(td llm.ToolDef) map[string]any {
		return td.Function.Parameters.(map[string]any)["properties"].(map[string]any)
	}
	if _, ok := props(root)["tell"]; !ok {
		t.Error("a root's jobs must take tell")
	}
	for _, p := range []string{"agent", "tell", "resume"} {
		if _, ok := props(child)[p]; ok {
			t.Errorf("a child's jobs must not offer %q: it has no agents", p)
		}
	}
}

// Asked the same thing again with nothing changed (only the clock), the
// third read of a running command waits for it instead of answering the
// same again: a poll cannot loop (USER 2026-10-27: the loop guard stopped
// a session polling agent_monitor).
func TestJobs_ARepeatedPollWaits(t *testing.T) {
	h := testHarness(t)
	be := &HostShell{Dir: t.TempDir()}
	t.Cleanup(be.Close)
	j := h.newJobStarted(be, "sleep 1.5; echo at-last")
	q := fmt.Sprintf(`{"job":%d}`, j.id)
	for i := 0; i < 2; i++ {
		if out := jobsCall(t, h, "jobs", q); !strings.Contains(out, "RUNNING") || strings.Contains(out, "waited") {
			t.Fatalf("read %d: %s", i+1, out)
		}
	}
	start := time.Now()
	out := jobsCall(t, h, "jobs", q)
	if time.Since(start) < 500*time.Millisecond || !strings.Contains(out, "waited until it did") || !strings.Contains(out, "at-last") {
		t.Errorf("the third read did not wait (%s): %s", time.Since(start), out)
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
