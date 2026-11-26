package harness

// The mid-turn message path. A message typed while the agent is working must
// reach it INSIDE the running turn — a user message right after the next tool
// result (midreply.go), while a background job's completion is lifted into the
// result itself — and several must batch, without limit.

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
)

// Every kind must be in queuedPolicies. The table decides the flush partition
// (causesTurn), so a kind missing from it does not fail — it quietly inherits
// the notification policy and starts costing a turn it was never meant to cost.
// That is precisely the mistake the table replaced four scattered switches to
// prevent, and only this test notices it.
func TestEveryQueuedKindHasAPolicy(t *testing.T) {
	for k := queuedKind(0); k < queuedKindCount; k++ {
		if _, ok := queuedPolicies[k]; !ok {
			t.Errorf("queuedKind %d has no queuedPolicies entry; it would "+
				"silently inherit the notification policy", k)
		}
	}
}

// Each kind must render and persist as SOMETHING. An empty prefix lifts into a
// tool result unlabelled (the model cannot tell the user from the machinery);
// an empty entryKind persists a row whose Kind no shaper knows how to treat.
func TestEveryQueuedKindRendersAndPersists(t *testing.T) {
	for k := queuedKind(0); k < queuedKindCount; k++ {
		p := queued{kind: k}.policy()
		if p.prefix == "" {
			t.Errorf("kind %d has no prefix; it would lift in unlabelled", k)
		}
		if p.entryKind == "" {
			t.Errorf("kind %d has no entryKind; it would persist with an empty Kind", k)
		}
	}
}

// The two flushes must partition the buffer exactly — every kind taken by one
// of them, none by both, nothing stranded. Driven through the real drains with
// one item of every kind buffered, because "by construction" is a claim about
// today's code and this is the invariant a future edit would quietly break.
func TestFlushesPartitionEveryKind(t *testing.T) {
	h := newNoteHarness(t)
	for k := queuedKind(0); k < queuedKindCount; k++ {
		h.noteMu.Lock()
		h.queue = append(h.queue, queued{kind: k, text: "item"})
		h.noteMu.Unlock()
	}

	waking := h.flushQueued()
	silent := h.flushSilent()

	if got := waking + silent; got != int(queuedKindCount) {
		t.Errorf("flushes delivered %d of %d kinds; some kind is in neither partition",
			got, int(queuedKindCount))
	}
	h.noteMu.Lock()
	left := len(h.queue)
	h.noteMu.Unlock()
	if left != 0 {
		t.Errorf("%d item(s) stranded in the buffer after both flushes", left)
	}
	// Re-flushing must find nothing: an item taken by both partitions would be
	// delivered twice, which is how a user message gets said to the model twice.
	if n := h.flushQueued() + h.flushSilent(); n != 0 {
		t.Errorf("re-flush delivered %d item(s); the partitions overlap", n)
	}
}

// A message typed mid-turn is taken with the running tool's result, to follow
// it as a user message of its own (midreply.go).
func TestSayLiftsIntoToolResult(t *testing.T) {
	h := newNoteHarness(t)
	inner := func(ctx context.Context, tc llm.ToolCall) (llm.ToolResult, error) {
		h.Say("actually, skip the tests for now")
		return llm.TextResult("wrote 3 lines"), nil
	}
	out, err := withLiftedQueue(inner, h)(context.Background(), llm.ToolCall{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Text, "wrote 3 lines") {
		t.Errorf("tool result lost: %q", out.Text)
	}
	// Not in the result: held to follow it as a user message (midreply.go).
	if strings.Contains(out.Text, "skip the tests") || !slices.Contains(liftedTexts(h), "actually, skip the tests for now") {
		t.Errorf("message not held for after the result: %q, held %q", out.Text, liftedTexts(h))
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
	// The person's messages follow the result, in order (midreply.go).
	if want := "tool output\n\n[background] background job #1 finished"; out != want {
		t.Errorf("lifted result:\n%s\nwant:\n%s", out, want)
	}
	if got := liftedTexts(h); !slices.Equal(got, []string{"one", "two"}) {
		t.Errorf("held %q, want [one two]", got)
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
	h.store.onToolResult = h.deliverLifted // as Start installs it

	h.prepareTurn(ctx)

	entries, err := h.store.Context(ctx, "dun")
	if err != nil {
		t.Fatal(err)
	}
	// The message follows the healed result as a user message (midreply.go).
	if u := entries[len(entries)-1]; u.Kind != agent.KindUser || u.Content != "connection dropped — carry on" {
		t.Fatalf("last entry = %+v; want the message after the healed result", u)
	}
	last := entries[len(entries)-2]
	if last.Kind != agent.KindToolResult {
		t.Fatalf("last entry = %+v; want a tool result pairing off the orphan call", last)
	}
	if last.ToolCallID != "call_1" {
		t.Errorf("ToolCallID = %q; want call_1, or the pair is still broken", last.ToolCallID)
	}
	if !strings.Contains(last.Content, "cut off by a restart") || !strings.Contains(last.Content, "Try it again") {
		t.Errorf("healed result does not say what happened: %q", last.Content)
	}
	if strings.Contains(last.Content, "connection dropped") {
		t.Errorf("the message is in the healed result too: %q", last.Content)
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

// Held for dund's upgrade (HoldPending), the news that would start a turn
// goes into the conversation now, and stays pending for the next turn: no
// turn runs for it, and a restart keeps it.
func TestHoldPendingKeepsNewsForTheNextTurn(t *testing.T) {
	h := newNoteHarness(t)
	h.Notify("background job #1 finished")
	h.Aside("context only")
	if n := h.HoldPending(); n != 1 {
		t.Fatalf("held %d items, want the notification alone", n)
	}
	if h.store.pending() != 1 {
		t.Errorf("pending = %d: the next turn must react to it", h.store.pending())
	}
	found := false
	for _, e := range h.store.entries {
		found = found || strings.Contains(e.Content, "job #1 finished")
	}
	if !found {
		t.Error("the notification is not in the conversation")
	}
	if h.Queued() != 0 {
		t.Errorf("still buffered: %d", h.Queued())
	}
}
