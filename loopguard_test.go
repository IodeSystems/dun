package dun

// The guard has to be precise in BOTH directions, and the second one is what
// makes it hard: a detector that interrupts normal iteration is worse than no
// detector, because what it interrupts is someone working. So these tests pin
// the refusals AND the things that must never be refused.

import (
	"context"
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
)

// counting is a dispatcher that records what actually reached it.
type counting struct {
	calls  []string
	result string
}

func (c *counting) dispatch(_ context.Context, tc llm.ToolCall) (string, error) {
	c.calls = append(c.calls, tc.Function.Name+" "+tc.Function.Arguments)
	if c.result == "" {
		return "ok", nil
	}
	return c.result, nil
}

func call(name, args string) llm.ToolCall {
	var tc llm.ToolCall
	tc.Type = "function"
	tc.Function.Name = name
	tc.Function.Arguments = args
	return tc
}

// The measured loop, replayed: twelve back-to-back `recap` calls with identical
// arguments (yscr 2026-08-23, calls 39–50). The first folded 379 entries; the
// eleven after it each folded one and wrote a 2,523-byte file. Two should run
// and ten should never reach the tool.
func TestLoopGuard_RefusesTheMeasuredRecapLoop(t *testing.T) {
	h := newNoteHarness(t)
	inner := &counting{result: "Done — recap: 1 entries (~2333 chars) → …recap18.jsonl"}
	d := withLoopGuard(inner.dispatch, h)

	args := `{"from":"Let's get started","summary":"SHIPPED this session"}`
	var refusals int
	for i := 0; i < 12; i++ {
		out, err := d(context.Background(), call("recap", args))
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if strings.HasPrefix(out, "ERROR: this call was NOT run") {
			refusals++
		}
	}
	if len(inner.calls) != defaultLoopRepeats-1 {
		t.Errorf("%d calls reached the tool, want %d", len(inner.calls), defaultLoopRepeats-1)
	}
	if refusals != 12-(defaultLoopRepeats-1) {
		t.Errorf("%d refusals, want %d", refusals, 12-(defaultLoopRepeats-1))
	}
}

// The refusal has to say why repeating cannot help and quote what the model
// already got. Told only "refused", a model reads a transport failure and
// retries — which is the loop.
func TestLoopGuard_RefusalCarriesThePreviousResult(t *testing.T) {
	h := newNoteHarness(t)
	inner := &counting{result: "nothing to fold: 0 entries matched"}
	d := withLoopGuard(inner.dispatch, h)

	var out string
	for i := 0; i < defaultLoopRepeats; i++ {
		out, _ = d(context.Background(), call("recap", `{"from":"x"}`))
	}
	for _, want := range []string{"NOT run", "recap", "nothing to fold", "something DIFFERENT"} {
		if !strings.Contains(out, want) {
			t.Errorf("refusal is missing %q:\n%s", want, out)
		}
	}
}

// Legitimate repetition, from the same session: `ship {"mode":"push"}` eight
// times and one `git log` three times, every pair separated by other work. The
// run is what is being counted, not the total.
func TestLoopGuard_LeavesSeparatedRepeatsAlone(t *testing.T) {
	h := newNoteHarness(t)
	inner := &counting{}
	d := withLoopGuard(inner.dispatch, h)

	for i := 0; i < 8; i++ {
		if out, _ := d(context.Background(), call("ship", `{"mode":"push"}`)); strings.HasPrefix(out, "ERROR") {
			t.Fatalf("ship #%d was refused; separated repeats are normal work", i)
		}
		if out, _ := d(context.Background(), call("exec", `{"command":"go test ./..."}`)); strings.HasPrefix(out, "ERROR") {
			t.Fatalf("exec #%d was refused", i)
		}
	}
	if len(inner.calls) != 16 {
		t.Errorf("%d calls ran, want all 16", len(inner.calls))
	}
}

// Polling IS repetition: exec_monitor, agent_monitor and ask_user exist to be
// called again with the same arguments. Guarding them would break the only way
// dun waits for anything.
func TestLoopGuard_ExemptsPollingTools(t *testing.T) {
	h := newNoteHarness(t)
	inner := &counting{}
	d := withLoopGuard(inner.dispatch, h)

	for name := range pollingTools {
		inner.calls = nil
		for i := 0; i < 10; i++ {
			if out, _ := d(context.Background(), call(name, `{"id":1}`)); strings.HasPrefix(out, "ERROR") {
				t.Fatalf("%s poll #%d was refused", name, i)
			}
		}
		if len(inner.calls) != 10 {
			t.Errorf("%s: %d polls ran, want 10", name, len(inner.calls))
		}
	}
}

// A poll between two identical calls means something was being waited on, which
// is exactly the "the world changed in between" the run is looking for.
func TestLoopGuard_APollBreaksTheRun(t *testing.T) {
	h := newNoteHarness(t)
	inner := &counting{}
	d := withLoopGuard(inner.dispatch, h)

	for i := 0; i < 6; i++ {
		d(context.Background(), call("exec", `{"command":"make"}`))
		d(context.Background(), call("exec_monitor", `{"id":1}`))
	}
	if len(inner.calls) != 12 {
		t.Errorf("%d calls ran, want 12 — a poll must reset the run", len(inner.calls))
	}
}

// Key order is not meaning; whitespace inside a value is. The arguments being
// compared are shell commands and file contents, where collapsing a space would
// make two DIFFERENT calls look identical — the one failure mode a guard that
// refuses work must not have.
func TestLoopGuard_NormalizesKeyOrderButNotContent(t *testing.T) {
	if a, b := normalizeToolArgs(`{"a":1,"b":2}`), normalizeToolArgs(`{"b":2,"a":1}`); a != b {
		t.Errorf("key order should not matter: %q vs %q", a, b)
	}
	if a, b := normalizeToolArgs(`{"c":"x  y"}`), normalizeToolArgs(`{"c":"x y"}`); a == b {
		t.Error("whitespace inside a value is data and must not be normalized away")
	}
	// Non-JSON arguments still compare, just literally.
	if got := normalizeToolArgs("  not json  "); got != "not json" {
		t.Errorf("non-JSON = %q, want it trimmed", got)
	}
}

// A model that reads the refusal and repeats the call anyway is not going to be
// argued out of it. The human decides, once.
func TestLoopGuard_EscalatesToTheUserOnce(t *testing.T) {
	h := newNoteHarness(t)
	h.cfg.Ask = func(context.Context, string, []string, bool) (string, error) { return "", nil }
	d := withLoopGuard((&counting{}).dispatch, h)

	for i := 0; i < defaultLoopRepeats+loopAskAfter+4; i++ {
		d(context.Background(), call("recap", `{"from":"x"}`))
	}
	forced := h.mergeForcedToolCalls(nil)
	if len(forced) != 1 {
		t.Fatalf("%d forced calls, want exactly 1", len(forced))
	}
	if forced[0].Function.Name != "ask_user" {
		t.Errorf("forced %q, want ask_user", forced[0].Function.Name)
	}
	if !strings.Contains(forced[0].Function.Arguments, "not making progress") {
		t.Errorf("the question should say what is wrong: %s", forced[0].Function.Arguments)
	}
}

// Without an ask handler there is nobody to escalate TO, and forcing a call to a
// tool that is not in the tool set would be a call the model cannot see.
func TestLoopGuard_DoesNotEscalateWithNoAsker(t *testing.T) {
	h := newNoteHarness(t)
	d := withLoopGuard((&counting{}).dispatch, h)

	for i := 0; i < defaultLoopRepeats+loopAskAfter+4; i++ {
		d(context.Background(), call("recap", `{"from":"x"}`))
	}
	if forced := h.mergeForcedToolCalls(nil); len(forced) != 0 {
		t.Errorf("%d forced calls with no AskFunc, want 0", len(forced))
	}
}

// The escape hatch, for a workload this guard reads wrong.
func TestLoopGuard_CanBeDisabled(t *testing.T) {
	t.Setenv("DUN_LOOP_REPEATS", "0")
	h := newNoteHarness(t)
	inner := &counting{}
	d := withLoopGuard(inner.dispatch, h)

	for i := 0; i < 20; i++ {
		d(context.Background(), call("recap", `{"from":"x"}`))
	}
	if len(inner.calls) != 20 {
		t.Errorf("%d calls ran with the guard disabled, want 20", len(inner.calls))
	}
}

// A refusal must still carry anything buffered. The guard sits outside
// withLiftedQueue so it can see every tool, so it has to drain the queue itself
// — and a message the user typed while the agent looped is the last thing that
// should wait for the loop to end.
func TestLoopGuard_RefusalStillCarriesQueuedMessages(t *testing.T) {
	h := newNoteHarness(t)
	d := withLoopGuard((&counting{}).dispatch, h)

	d(context.Background(), call("recap", `{"from":"x"}`))
	h.Say("stop doing that, try the other file")
	var out string
	for i := 0; i < defaultLoopRepeats+2; i++ {
		out, _ = d(context.Background(), call("recap", `{"from":"x"}`))
		if strings.Contains(out, "NOT run") {
			break // the FIRST refusal is the one the queue was waiting for
		}
	}
	if !strings.Contains(out, "stop doing that") {
		t.Errorf("the user's message was stranded behind the refusal:\n%s", out)
	}
	if !strings.Contains(out, "NOT run") {
		t.Errorf("the refusal itself was lost:\n%s", out)
	}
}

// The guard sits outermost, so a refusal is never reconsidered by an inner
// wrapper — and it must not disturb the result of a call it lets through.
func TestLoopGuard_IsTransparentWhenItDoesNotFire(t *testing.T) {
	h := newNoteHarness(t)
	inner := &counting{result: "the real result"}
	var d agent.ToolDispatcher = withLoopGuard(inner.dispatch, h)

	out, err := d(context.Background(), call("exec", `{"command":"ls"}`))
	if err != nil || out != "the real result" {
		t.Errorf("got (%q, %v), want the tool's own result untouched", out, err)
	}
}
