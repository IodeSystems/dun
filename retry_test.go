package dun

// Turn-scope retry + the queue that rides with it. The failure these exist for:
// a stream that dies MID-generation cannot be retried by llm.Client (tokens are
// already out), so before this dun printed one line and exited — "a generic
// connection error and dun dies".

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
)

// instantRetries removes the real backoff waits, recording what each one WOULD
// have been so a test can assert the schedule without living through it.
func instantRetries(t *testing.T) *[]time.Duration {
	t.Helper()
	var slept []time.Duration
	prev := turnRetrySleep
	turnRetrySleep = func(ctx context.Context, d time.Duration) bool {
		slept = append(slept, d)
		return ctx.Err() == nil
	}
	t.Cleanup(func() { turnRetrySleep = prev })
	return &slept
}

// midStreamDeath is the error shape a killed server produces: readStream reports
// the http2 reset as a stream error, and agent.Session wraps it.
func midStreamDeath() error {
	return errors.New("agent: chat: stream error: stream ID 49; CANCEL; received from peer")
}

// A turn killed mid-generation is retried, with the wait NARRATED, and the retry
// succeeds where the first attempt could not.
func TestRunTurn_RetriesMidStreamDeath(t *testing.T) {
	slept := instantRetries(t)
	h := newNoteHarness(t)
	var notes []RetryNote
	h.onRetry = func(n RetryNote) { notes = append(notes, n) }

	attempts := 0
	res, err := h.runTurn(context.Background(), func(context.Context) (agent.TurnResult, error) {
		attempts++
		if attempts < 3 {
			return agent.TurnResult{}, midStreamDeath()
		}
		return agent.TurnResult{Reply: "finished"}, nil
	})
	if err != nil {
		t.Fatalf("runTurn: %v", err)
	}
	if res.Reply != "finished" || attempts != 3 {
		t.Fatalf("reply = %q after %d attempts; want the third attempt's result", res.Reply, attempts)
	}
	if len(*slept) != 2 {
		t.Fatalf("waits = %v; want one per failed attempt", *slept)
	}
	if (*slept)[1] <= (*slept)[0] {
		t.Errorf("backoff not growing: %v", *slept)
	}
	if len(notes) != 3 {
		t.Fatalf("notes = %d (%v); want 2 waits + 1 recovered", len(notes), notes)
	}
	for i, n := range notes[:2] {
		if n.Scope != "turn" || n.Kind != "interrupted" {
			t.Errorf("note %d = %+v; want a turn-scope interruption", i, n)
		}
		if !strings.Contains(n.Reason, "interrupted") {
			t.Errorf("note %d reason = %q; want it to say the turn was interrupted", i, n.Reason)
		}
		if n.Delay <= 0 {
			t.Errorf("note %d has no delay; a UI cannot count down", i)
		}
	}
	if notes[2].Kind != "recovered" {
		t.Errorf("last note = %+v; want recovered so a UI takes its banner down", notes[2])
	}
}

// A fault that is NOT the provider is returned untouched. Retrying a model that
// wrote a bad tool call, or a store that failed, is pure delay before the same
// failure — and it buries the real error.
func TestRunTurn_DoesNotRetryLocalFailure(t *testing.T) {
	slept := instantRetries(t)
	h := newNoteHarness(t)
	var notes []RetryNote
	h.onRetry = func(n RetryNote) { notes = append(notes, n) }

	want := errors.New("agent: persist llm reply: disk full")
	attempts := 0
	_, err := h.runTurn(context.Background(), func(context.Context) (agent.TurnResult, error) {
		attempts++
		return agent.TurnResult{}, want
	})
	if !errors.Is(err, want) {
		t.Fatalf("err = %v; want the original error", err)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d; want no retry", attempts)
	}
	if len(*slept) != 0 || len(notes) != 0 {
		t.Errorf("waited %v / reported %v for a local failure", *slept, notes)
	}
}

// Cancellation wins over the transient classification. A cancelled request
// arrives wrapped in *url.Error, which satisfies net.Error and therefore LOOKS
// transient — retrying it would ignore the user's ctrl+c.
func TestRunTurn_StopsOnCancel(t *testing.T) {
	instantRetries(t)
	h := newNoteHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	attempts := 0
	_, err := h.runTurn(ctx, func(context.Context) (agent.TurnResult, error) {
		attempts++
		return agent.TurnResult{}, midStreamDeath()
	})
	if err == nil {
		t.Fatal("want an error")
	}
	if attempts != 1 {
		t.Errorf("attempts = %d; want no retry past a cancelled context", attempts)
	}
}

// DUN_TURN_RETRY_BUDGET=0 restores the old behaviour for an operator who wants a
// failure reported immediately.
func TestRunTurn_BudgetZeroDisables(t *testing.T) {
	instantRetries(t)
	t.Setenv("DUN_TURN_RETRY_BUDGET", "0")
	h := newNoteHarness(t)

	attempts := 0
	_, err := h.runTurn(context.Background(), func(context.Context) (agent.TurnResult, error) {
		attempts++
		return agent.TurnResult{}, midStreamDeath()
	})
	if err == nil || attempts != 1 {
		t.Fatalf("attempts = %d, err = %v; want one attempt and the error", attempts, err)
	}
}

// The budget bounds the loop: once the next wait would cross it, dun gives up
// WITH the evidence rather than retrying forever.
func TestRunTurn_GivesUpAtBudget(t *testing.T) {
	instantRetries(t)
	t.Setenv("DUN_TURN_RETRY_BUDGET", "1ms")
	h := newNoteHarness(t)
	var notes []RetryNote
	h.onRetry = func(n RetryNote) { notes = append(notes, n) }

	_, err := h.runTurn(context.Background(), func(context.Context) (agent.TurnResult, error) {
		return agent.TurnResult{}, midStreamDeath()
	})
	if err == nil {
		t.Fatal("want the error after the budget is spent")
	}
	if len(notes) != 1 || notes[0].Kind != "giveup" {
		t.Fatalf("notes = %v; want a single giveup", notes)
	}
	if !strings.Contains(notes[0].Reason, "gave up") {
		t.Errorf("giveup reason = %q; want it to say so", notes[0].Reason)
	}
}

// The client's own retry narration reaches the UI hook. Without this the waiting
// is only ever logged, and a TUI's log is not on screen.
func TestWireRetry_CarriesClientEvents(t *testing.T) {
	var got []RetryNote
	c := llm.NewClient("http://127.0.0.1:1", "", "m")
	wireRetry(c, func(n RetryNote) { got = append(got, n) })
	if c.OnRetry == nil {
		t.Fatal("client hook not installed")
	}
	c.OnRetry(llm.RetryEvent{
		Kind: llm.Retry429, Attempt: 2, Status: 429, Delay: 10 * time.Second,
		ServerAsked: true, Reason: "provider at capacity — 4/4 slots busy",
		BP: &llm.Backpressure{Reason: "queue-timeout", Capacity: 4, InFlight: 4, Waiting: 2},
	})
	if len(got) != 1 {
		t.Fatalf("notes = %v; want 1", got)
	}
	n := got[0]
	if n.Scope != "request" || n.Kind != "429" || n.Status != 429 {
		t.Errorf("note = %+v; want a request-scope 429", n)
	}
	if n.Capacity != 4 || n.InFlight != 4 || n.Waiting != 2 || n.Queue != "queue-timeout" {
		t.Errorf("queue detail lost: %+v", n)
	}
	if !n.Queued() {
		t.Error("Queued() = false; the proxy reported a queue")
	}
	if !strings.Contains(n.String(), "provider asked for 10s") {
		t.Errorf("String() = %q; want the server's own wait credited", n.String())
	}
}

// wireRetry must tolerate a runner that is not an *llm.Client (a fake, another
// transport): it simply gets no request-scope narration.
func TestWireRetry_OtherRunnerIsNoOp(t *testing.T) {
	wireRetry(mustNotRunRunner{}, func(RetryNote) { t.Fatal("no events expected") })
}

// The provider-retry policy is an OPERATOR decision, not a compile-time one:
// which numbers are right depends on the endpoint (a single-slot local model
// wants an unbounded 429 wait; a long batch wants more 5xx attempts).
func TestApplyRetryPolicy_FromEnv(t *testing.T) {
	t.Setenv("DUN_RETRY_BUDGET", "-1s")
	t.Setenv("DUN_RETRY_5XX_ATTEMPTS", "20")
	c := llm.NewClient("http://127.0.0.1:1", "", "m")
	applyRetryPolicy(c)

	_, _, budget := c.RetryPolicy()
	if budget < 24*time.Hour {
		t.Errorf("budget = %s; a negative setting means unbounded", budget)
	}
	if c.Retry5xxAttempts != 20 {
		t.Errorf("Retry5xxAttempts = %d; want 20", c.Retry5xxAttempts)
	}
}

// Garbage is ignored rather than silently reinterpreted as "no retries".
func TestApplyRetryPolicy_IgnoresJunk(t *testing.T) {
	t.Setenv("DUN_RETRY_BUDGET", "soon")
	t.Setenv("DUN_RETRY_5XX_ATTEMPTS", "-3")
	c := llm.NewClient("http://127.0.0.1:1", "", "m")
	applyRetryPolicy(c)
	if c.RetryBudget != 0 || c.Retry5xxAttempts != 0 {
		t.Errorf("junk applied: budget=%s attempts=%d", c.RetryBudget, c.Retry5xxAttempts)
	}
}
