package dun

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
)

// execDispatcher wires withExec the way servers_runtime does, over a REAL
// persistent shell, so these tests exercise the promotion path rather than a
// stand-in for it.
func execDispatcher(t *testing.T, h *Harness) agent.ToolDispatcher {
	t.Helper()
	be := &HostShell{Dir: t.TempDir()}
	t.Cleanup(be.Close)
	return withExec(nil, be, nil, func(command string) *bgJob {
		return h.startJob(be, command)
	}, nil)
}

func execCall(args string) llm.ToolCall {
	var c llm.ToolCall
	c.Function.Name = "exec"
	c.Function.Arguments = args
	return c
}

// The common case has to stay ONE round trip. Making every command a job would
// trade a rare ten-minute stall for a constant extra turn on every `ls`, which
// is a worse deal.
func TestExec_FastCommandAnswersInline(t *testing.T) {
	h := testHarness(t)
	d := execDispatcher(t, h)

	out, err := d(context.Background(), execCall(`{"command":"echo quick"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "quick") {
		t.Fatalf("want the output inline: %q", out)
	}
	if strings.Contains(out, "background job") {
		t.Errorf("a fast command must not become a job: %q", out)
	}
	if n := h.notes(); len(n) != 0 {
		t.Errorf("a fast command has nothing to announce: %q", n)
	}
	if js := h.Jobs(); len(js) != 0 {
		t.Errorf("a fast command must not leave a row in the pane: %+v", js)
	}
}

// The whole point. A command that outstays its grace hands itself over instead
// of holding the turn: the call returns at the grace, the command keeps
// running, and the model is told where its answer will come from.
func TestExec_SlowCommandBecomesAJob(t *testing.T) {
	h := testHarness(t)
	d := execDispatcher(t, h)

	start := time.Now()
	out, err := d(context.Background(), execCall(`{"command":"echo early; sleep 2; echo late","timeout":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el > 1500*time.Millisecond {
		t.Fatalf("the call must return at the grace, not at the command: %s", el)
	}
	if !strings.Contains(out, "STILL RUNNING") || !strings.Contains(out, "NOT killed") {
		t.Fatalf("the notice must say the command is alive: %q", out)
	}
	if !strings.Contains(out, "background job #1") || !strings.Contains(out, "exec_monitor(job:1)") {
		t.Fatalf("the notice must name the job and where to look: %q", out)
	}

	// Output produced BEFORE the handover is not lost: a provisional job holds
	// it in memory, and promotion flushes it into the log it just opened.
	js := h.Jobs()
	if len(js) != 1 || js[0].Log == "" {
		t.Fatalf("the promoted job needs a row and a log: %+v", js)
	}
	b, err := os.ReadFile(js[0].Log)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "early") {
		t.Errorf("pre-handover output must reach the log: %q", b)
	}

	// It really keeps running, and it really reports back.
	waitFor(t, 10*time.Second, func() bool {
		for _, n := range h.notes() {
			if strings.Contains(n, "late") {
				return true
			}
		}
		return false
	})
}

// Donation is what makes promotion possible: the promoted command keeps the
// shell it is already in, and the shell backend starts a fresh one for whoever
// is next. Neither command waits for the other.
func TestHostShell_DonationFreesTheShellForTheNextCommand(t *testing.T) {
	hs := &HostShell{Dir: t.TempDir()}
	t.Cleanup(hs.Close)

	// Something in the shell's memory, to prove the next command lands on a NEW
	// shell rather than the donated one.
	if r := hs.Run(context.Background(), "export DUN_BEFORE=set", nil); r.Failed() {
		t.Fatalf("export failed: %v", r)
	}

	promote := make(chan struct{})
	done := make(chan ExecResult, 1)
	go func() {
		done <- hs.RunPromotable(context.Background(), "sleep 3; echo slow-done", nil, promote)
	}()
	time.Sleep(200 * time.Millisecond) // let it reach the read loop
	close(promote)

	start := time.Now()
	r := hs.Run(context.Background(), "echo free", nil)
	if el := time.Since(start); el > time.Second {
		t.Fatalf("the next command queued behind the donated shell: %s", el)
	}
	if strings.TrimSpace(r.Output) != "free" {
		t.Fatalf("next command output: %q", r.Output)
	}

	// The documented cost: the donated shell took the environment with it, so
	// an export from before the handover is gone from the fresh one.
	if r := hs.Run(context.Background(), "echo [$DUN_BEFORE]", nil); strings.TrimSpace(r.Output) != "[]" {
		t.Errorf("a fresh shell must not carry the donated shell's exports: %q", r.Output)
	}

	// And the donated command was neither killed nor restarted.
	select {
	case res := <-done:
		if !strings.Contains(res.Output, "slow-done") {
			t.Errorf("the donated command must still finish and report: %q", res.Output)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the donated command never finished")
	}
}

// The shell dying under the command is a RESULT, not a hang. `exit 7` ends the
// shell before the sentinel can be printed; waiting for a sentinel that will
// never come is what held a session silent for ten minutes.
func TestHostShell_KeepsOutputWhenTheCommandExitsTheShell(t *testing.T) {
	hs := &HostShell{Dir: t.TempDir()}
	t.Cleanup(hs.Close)

	r := hs.Run(context.Background(), "echo before-exit; exit 3", nil)
	if r.Code != 3 {
		t.Errorf("the shell's own status is the command's: want 3, got %d (%q)", r.Code, r.Output)
	}
	if !strings.Contains(r.Output, "before-exit") {
		t.Errorf("output written before the shell died must survive: %q", r.Output)
	}
	// And the backend recovers: the next command gets a new shell.
	if r := hs.Run(context.Background(), "echo recovered", nil); strings.TrimSpace(r.Output) != "recovered" {
		t.Fatalf("the backend must start a fresh shell after one dies: %q (%v)", r.Output, r.Err)
	}
}

// The persistent shell is per-AGENT. Everything else in a child's config is
// inherited, and the shell is a POINTER — so without an explicit copy a child's
// `export` would land in its parent's environment and its siblings', and every
// agent's commands would queue behind each other in one shell.
func TestChildConfig_ChildGetsItsOwnShell(t *testing.T) {
	dir := t.TempDir()
	own := &HostShell{Dir: dir}
	parent := &Harness{cfg: Config{Exec: own}}

	cfg := childConfig(parent, 1, "", nil)
	child, ok := cfg.Exec.(*HostShell)
	if !ok {
		t.Fatalf("the child lost the shell backend: %T", cfg.Exec)
	}
	if child == own {
		t.Fatal("the child shares the parent's shell")
	}
	if child.Dir != dir {
		t.Errorf("the child's shell must work in the same directory: %q, want %q", child.Dir, dir)
	}
}
