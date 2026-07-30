package dun

// The mid-turn message path. A message typed while the agent is working must
// reach it INSIDE the running turn — lifted into the next tool result, the same
// way a background job's completion travels — and several must batch, without
// limit.

import (
	"context"
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
)

// A message typed mid-turn rides back inside the running tool's result, labelled
// so the model can tell the user from the machinery.
func TestSayLiftsIntoToolResult(t *testing.T) {
	h := newNoteHarness(t)
	inner := func(ctx context.Context, tc llm.ToolCall) (string, error) {
		h.Say("actually, skip the tests for now")
		return "wrote 3 lines", nil
	}
	out, err := withLiftedQueue(inner, h)(context.Background(), llm.ToolCall{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "wrote 3 lines") {
		t.Errorf("tool result lost: %q", out)
	}
	if !strings.Contains(out, "[user] actually, skip the tests") {
		t.Errorf("message was not lifted into the tool result: %q", out)
	}
	if h.Pending() != 0 || h.Queued() != 0 {
		t.Errorf("lifted message should leave nothing buffered (pending=%d queued=%d)", h.Pending(), h.Queued())
	}
}

// tool call + message + message + tool call: the buffer is a slice, so any number
// of messages accumulate and the NEXT result carries all of them, in order, mixed
// with background news.
func TestQueueBatchesWithoutLimit(t *testing.T) {
	h := newNoteHarness(t)
	h.Say("one")
	h.Notify("background job #1 finished")
	h.Say("two")
	if h.Queued() != 3 {
		t.Fatalf("Queued() = %d; want 3 buffered", h.Queued())
	}
	out := h.liftQueued("tool output")
	for _, want := range []string{"tool output", "[user] one", "[background] background job #1", "[user] two"} {
		if !strings.Contains(out, want) {
			t.Errorf("lifted result missing %q:\n%s", want, out)
		}
	}
	if i, j := strings.Index(out, "[user] one"), strings.Index(out, "[user] two"); i > j {
		t.Error("messages delivered out of order")
	}
	// And the next round starts empty, so a third message rides the following
	// result rather than being repeated.
	h.Say("three")
	if out2 := h.liftQueued("second tool output"); strings.Contains(out2, "one") {
		t.Errorf("a delivered message was repeated: %q", out2)
	}
}

// Nothing is dropped when no tool runs: whatever the turn did not pick up becomes
// a real inbox arrival, a user message as a user turn.
func TestQueueFlushesAsUserTurn(t *testing.T) {
	h := newNoteHarness(t)
	h.Say("one more thing")
	if n := h.flushQueued(); n != 1 {
		t.Fatalf("flushQueued() = %d; want 1", n)
	}
	entries, err := h.store.Context(context.Background(), "dun")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Kind != agent.KindUser {
		t.Fatalf("entries = %+v; want one user entry", entries)
	}
	if h.store.pending() != 1 {
		t.Errorf("store.pending() = %d; want the turn to see it", h.store.pending())
	}
}

// A turn killed between persisting a tool CALL and recording its result leaves the
// history structurally invalid — providers reject an assistant(tool_calls) with no
// matching tool message before the model is even reached, so EVERY later request
// fails identically. prepareTurn must pair it off, and the user's follow-up rides
// inside that same result: one batch, no extra turn.
func TestPrepareTurnHealsOrphanCallAndLiftsMessage(t *testing.T) {
	h := newNoteHarness(t)
	ctx := context.Background()
	_ = h.store.Append(ctx, "dun", agent.Entry{ID: "1", Kind: agent.KindUser, Content: "fix the build"})
	_ = h.store.Append(ctx, "dun", agent.Entry{
		ID: "2", Kind: agent.KindToolCall, Content: `{"command":"go build ./..."}`,
		ToolCallID: "call_1", ToolName: "exec",
	})
	h.Say("connection dropped — carry on")

	h.prepareTurn(ctx)

	entries, err := h.store.Context(ctx, "dun")
	if err != nil {
		t.Fatal(err)
	}
	last := entries[len(entries)-1]
	if last.Kind != agent.KindToolResult {
		t.Fatalf("last entry = %+v; want a tool result pairing off the orphan call", last)
	}
	if last.ToolCallID != "call_1" {
		t.Errorf("ToolCallID = %q; want call_1, or the pair is still broken", last.ToolCallID)
	}
	if !strings.Contains(last.Content, "INTERRUPTED") {
		t.Errorf("healed result does not say what happened: %q", last.Content)
	}
	if !strings.Contains(last.Content, "[user] connection dropped") {
		t.Errorf("the queued message did not ride the healed result: %q", last.Content)
	}
	if h.store.pending() == 0 {
		t.Error("healed result left nothing pending; the next turn would be a no-op")
	}
	// Idempotent: a second pass has nothing left to heal.
	before := len(entries)
	h.prepareTurn(ctx)
	after, _ := h.store.Context(ctx, "dun")
	if len(after) != before {
		t.Errorf("prepareTurn wrote %d more entries on a healed session", len(after)-before)
	}
}

// A resumed session whose calls all have results is left completely alone.
func TestPrepareTurnLeavesPairedCallsAlone(t *testing.T) {
	h := newNoteHarness(t)
	ctx := context.Background()
	_ = h.store.Append(ctx, "dun", agent.Entry{ID: "1", Kind: agent.KindToolCall, Content: "{}", ToolCallID: "c1", ToolName: "exec"})
	_ = h.store.Append(ctx, "dun", agent.Entry{ID: "2", Kind: agent.KindToolResult, Content: "ok", ToolCallID: "c1", ToolName: "exec"})
	h.prepareTurn(ctx)
	entries, _ := h.store.Context(ctx, "dun")
	if len(entries) != 2 {
		t.Errorf("entries = %d; want the paired exchange untouched", len(entries))
	}
	if h.store.pending() != 0 {
		t.Errorf("pending = %d; want nothing to react to", h.store.pending())
	}
}
