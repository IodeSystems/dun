package harness

import (
	"context"
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/dun/internal/memory"
)

func triggerHarness(t *testing.T) (*Harness, *forkRunner) {
	defs := []llm.ToolDef{memory.ToolDef()}
	h := newNoteHarness(t)
	r := &forkRunner{}
	h.Session = &agent.Session{SessionID: "dun", Store: h.store, Runner: r, System: "You are dun.", Tools: defs,
		Build: func(ctx context.Context, sid, system string) ([]llm.Message, error) {
			return agent.DefaultContextBuilder(ctx, h.store, sid, system)
		}}
	m := testNotes(t)
	h.memory.Store(m)
	return h, r
}

func TestMemoryTriggers_FireOnAnAskNotOnHistoryOrASave(t *testing.T) {
	h, r := triggerHarness(t)
	ctx := context.Background()
	_ = h.store.Append(ctx, "dun", agent.Entry{ID: "old", Kind: agent.KindUser, Content: "Remember, old history.", CreatedAt: 1})
	h.markTriggerScan() // as Start does: a resumed session's history is not scanned
	if msg := h.checkMemoryTriggers(ctx); msg != "" {
		t.Fatalf("history fired: %q", msg)
	}

	_ = h.store.Append(ctx, "dun", agent.Entry{ID: "u1", Kind: agent.KindUser, Content: "Remember, never push without asking.", CreatedAt: 2})
	if _, err := h.Session.Turn(ctx); err != nil {
		t.Fatal(err)
	}
	msg := h.checkMemoryTriggers(ctx)
	if !strings.HasPrefix(msg, "memory (you said remember): saved ") {
		t.Fatalf("got %q", msg)
	}
	if last := r.msgs[len(r.msgs)-1]; !strings.Contains(last[len(last)-1].Content, "never push without asking") {
		t.Error("the fork must be told what to remember")
	}
	if msg := h.checkMemoryTriggers(ctx); msg != "" {
		t.Errorf("a message is checked once, got %q", msg)
	}

	// The model saved it itself: nothing to do.
	_ = h.store.Append(ctx, "dun", agent.Entry{ID: "u2", Kind: agent.KindUser, Content: "Remember the port is 8080.", CreatedAt: 10})
	_ = h.store.Append(ctx, "dun", agent.Entry{ID: "c2", Kind: agent.KindToolCall, ToolName: "memory", Content: "{}", CreatedAt: 11})
	if msg := h.checkMemoryTriggers(ctx); msg != "" {
		t.Errorf("a turn that called memory fired %q", msg)
	}
}

func TestMemoryTriggers_OffWithoutTheIndexOrWhenDisabled(t *testing.T) {
	h, _ := triggerHarness(t)
	h.cfg.DisableMemoryTriggers = true
	_ = h.store.Append(context.Background(), "dun", agent.Entry{ID: "u", Kind: agent.KindUser, Content: "Remember x.", CreatedAt: 5})
	if msg := h.checkMemoryTriggers(context.Background()); msg != "" {
		t.Errorf("disabled fired %q", msg)
	}
	h.cfg.DisableMemoryTriggers = false
	if msg := h.checkMemoryTriggers(context.Background()); msg != "" {
		t.Errorf("a disabled pass still moves the mark; got %q", msg)
	}
}
