package dun

import (
	"context"
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
)

func newNoteHarness(t *testing.T) *Harness {
	t.Helper()
	s, err := openSessionStore("")
	if err != nil {
		t.Fatal(err)
	}
	return &Harness{store: s, wake: make(chan struct{}, 1)}
}

// A note that arrives while a tool is running rides back inside that tool's
// result, so the model reads it without a turn being scheduled for it.
func TestNotesLiftIntoToolResult(t *testing.T) {
	h := newNoteHarness(t)
	inner := func(ctx context.Context, tc llm.ToolCall) (string, error) {
		h.Notify("background job #1 finished — `go test`:\nok")
		return "wrote 3 lines", nil
	}
	dispatch := withLiftedQueue(inner, h)

	out, err := dispatch(context.Background(), llm.ToolCall{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "wrote 3 lines") {
		t.Errorf("tool result lost: %q", out)
	}
	if !strings.Contains(out, "background job #1 finished") {
		t.Errorf("note was not lifted into the tool result: %q", out)
	}
	if h.Pending() != 0 {
		t.Errorf("lifted note should leave nothing pending, got %d", h.Pending())
	}
}

// A note that arrives with no tool call in flight stays buffered until the
// turn boundary, then becomes a real inbox arrival.
func TestNotesFlushWhenNoToolRuns(t *testing.T) {
	h := newNoteHarness(t)
	h.Notify("job #1 finished")
	h.Notify("job #2 finished")

	if got := h.Pending(); got != 2 {
		t.Fatalf("Pending() = %d, want 2 buffered", got)
	}
	if n := h.flushQueued(); n != 2 {
		t.Fatalf("flushQueued() = %d, want 2", n)
	}
	if got := h.store.pending(); got != 2 {
		t.Fatalf("store.pending() = %d, want 2 unclaimed arrivals", got)
	}
	// Both notes are one turn's worth of input: a single claim takes them all.
	n, err := h.store.ClaimPending(context.Background(), "dun", 0)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("ClaimPending() = %d, want both notes claimed by one turn", n)
	}
	if got := h.store.pending(); got != 0 {
		t.Fatalf("pending after claim = %d, want 0", got)
	}
}

// The regression this all exists for: a duplicate wake must not run a turn.
// An empty turn appends an assistant message straight after the previous one,
// and the provider then rejects the NEXT request with "cannot have 2 or more
// assistant messages at the end of the list".
func TestContinueIsNoOpWithNothingPending(t *testing.T) {
	h := newNoteHarness(t)
	h.Session = &agent.Session{
		SessionID: "dun",
		Store:     h.store,
		Runner:    runnerThatMustNotRun(t),
	}
	res, err := h.Continue(context.Background())
	if err != nil {
		t.Fatalf("Continue with nothing pending: %v", err)
	}
	if strings.TrimSpace(res.Reply) != "" {
		t.Errorf("no-op Continue produced a reply: %q", res.Reply)
	}
}

// runnerThatMustNotRun fails the test if the agent loop calls the model.
type mustNotRunRunner struct{ t *testing.T }

func (r mustNotRunRunner) ChatStream(context.Context, []llm.Message, []llm.ToolDef, *llm.ChatOpts) (<-chan llm.StreamChunk, error) {
	r.t.Fatal("Continue called the model with nothing pending; that appends a second trailing assistant message")
	return nil, nil
}

func runnerThatMustNotRun(t *testing.T) agent.LLMRunner { return mustNotRunRunner{t} }
