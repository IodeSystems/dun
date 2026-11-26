package harness

import (
	"context"
	"testing"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
)

// A message a tool result carried is delivered AFTER that result is stored:
// OnDelivered names it and the call, and the history shows it as the
// person's message right after the result — where a live client puts it —
// while the model's context holds it only inside the result.
func TestLiftedMessageDeliveredAfterItsResult(t *testing.T) {
	h := newNoteHarness(t)
	h.store.onToolResult = h.deliverLifted
	var got []Delivery
	h.cfg.OnDelivered = func(d Delivery) { got = append(got, d) }
	inner := func(ctx context.Context, tc llm.ToolCall) (llm.ToolResult, error) {
		h.SayAs("m1", "skip the tests")
		return llm.TextResult("ok"), nil
	}
	out, err := withLiftedQueue(inner, h)(context.Background(), llm.ToolCall{ID: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("delivered before the result was stored: %+v", got)
	}
	ctx := context.Background()
	_ = h.store.Append(ctx, "dun", agent.Entry{ID: "r", Kind: agent.KindToolResult, ToolCallID: "c1", ToolName: "exec", Content: out.Text, CreatedAt: 1})
	if len(got) != 1 || got[0].CallID != "c1" || len(got[0].IDs) != 1 || got[0].IDs[0] != "m1" {
		t.Fatalf("delivered %+v; want m1 by c1", got)
	}
	items := historyItems(h.store.History())
	last := items[len(items)-1]
	if last.Kind != "user" || last.ID != "m1" || !last.Queued || last.Content != "skip the tests" {
		t.Errorf("history ends %+v; want the delivered message", last)
	}
	if items[len(items)-2].Kind != "tool_result" {
		t.Errorf("the message is not after its result: %+v", items)
	}
	entries, _ := h.store.Context(ctx, "dun")
	for _, e := range entries {
		if e.Kind == kindDelivered {
			t.Errorf("the model's context holds the delivered entry: it read it in the result")
		}
	}
	// The next result carries nothing more.
	_ = h.store.Append(ctx, "dun", agent.Entry{ID: "r2", Kind: agent.KindToolResult, ToolCallID: "c2", Content: "x", CreatedAt: 2})
	if len(got) != 1 {
		t.Errorf("delivered again: %+v", got)
	}
}

// A message no tool result took becomes the turn's input: delivered with no
// call, and its entry keeps the message's id.
func TestFlushedMessageDelivered(t *testing.T) {
	h := newNoteHarness(t)
	var got []Delivery
	h.cfg.OnDelivered = func(d Delivery) { got = append(got, d) }
	h.SayAs("m2", "one more thing")
	h.Notify("job done") // not the person's: not in the delivery
	h.flushQueued()
	if len(got) != 1 || got[0].CallID != "" || len(got[0].IDs) != 1 || got[0].IDs[0] != "m2" {
		t.Fatalf("delivered %+v; want m2 alone", got)
	}
	items := historyItems(h.store.History())
	if items[0].Kind != "user" || items[0].ID != "m2" || items[0].Queued {
		t.Errorf("history %+v; want the message as a turn's input with its id", items)
	}
}

// An old session file has no delivered entries and user entries with their
// store ids: it replays as before, every user item with an id.
func TestOldLogReplaysWithoutDelivered(t *testing.T) {
	entries := []agent.Entry{
		{ID: "u1", Kind: agent.KindUser, Content: "hi", CreatedAt: 1},
		{ID: "t1", Kind: agent.KindToolCall, ToolCallID: "c", ToolName: "exec", Content: `{}`, CreatedAt: 2},
		{ID: "t2", Kind: agent.KindToolResult, ToolCallID: "c", ToolName: "exec", Content: "ok\n\n[user] more", CreatedAt: 3},
	}
	items := historyItems(entries)
	if len(items) != 3 || items[0].ID != "u1" || items[2].Kind != "tool_result" {
		t.Errorf("old log replayed as %+v", items)
	}
}
