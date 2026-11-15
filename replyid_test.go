package dun

import (
	"context"
	"testing"

	"github.com/iodesystems/agentkit/agent"
)

// A reply's id is its entry's, in history and from ReplyID: two identical
// replies are two ids. A turn that wrote no reply has none (not the last
// turn's).
func TestReplyIDIsTheEntrys(t *testing.T) {
	h := newNoteHarness(t)
	ctx := context.Background()
	add := func(e agent.Entry) { _ = h.store.Append(ctx, "dun", e) }
	add(agent.Entry{ID: "u1", Kind: agent.KindUser, Content: "ping", CreatedAt: 1})
	add(agent.Entry{ID: "r1", Kind: agent.KindAssistant, Content: "pong", CreatedAt: 2})
	if id := h.ReplyID(); id != "r1" {
		t.Fatalf("first reply id %q", id)
	}
	add(agent.Entry{ID: "u2", Kind: agent.KindUser, Content: "ping", CreatedAt: 3})
	if id := h.ReplyID(); id != "" {
		t.Fatalf("a turn with no reply yet has %q", id)
	}
	add(agent.Entry{ID: "r2", Kind: agent.KindAssistant, Content: "pong", CreatedAt: 4})
	if id := h.ReplyID(); id != "r2" {
		t.Fatalf("second reply id %q", id)
	}
	var ids []string
	for _, it := range historyItems(h.store.History()) {
		if it.Kind == "assistant" {
			ids = append(ids, it.ID)
		}
	}
	if len(ids) != 2 || ids[0] != "r1" || ids[1] != "r2" {
		t.Errorf("history reply ids %v", ids)
	}
}
