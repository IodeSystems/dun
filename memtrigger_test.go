package dun

import (
	"context"
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
)

// The shapes measured in the logs (paraphrased): clause-initial "remember"
// is an ask; "remember" inside a clause is not.
func TestUserRememberRx_MeasuredShapes(t *testing.T) {
	for _, s := range []string{
		"Remember we don't want the load test to send email.",
		"remember our goals here: realistic physics",
		"Continue with it. Remember, once a sprint is picked it is closed.",
		"And remember, a project without isolation tests stays single-worktree.",
		"Can you write a script for this? remember, we have several aws accounts.",
		"Keep in mind the API is rate limited.",
		"From now on, run the linter before committing.",
		"Don't forget: staging uses a different region.",
	} {
		if !userRememberRx.MatchString(s) {
			t.Errorf("missed: %q", s)
		}
	}
	for _, s := range []string{
		"I remember absolute drift had a good UI for this.",
		"they don't remember sending a password",
		"we don't want personalization to pollute, but we want to remember it",
		"the ability to remember user decisions",
		"What do you remember about the parser?",
	} {
		if userRememberRx.MatchString(s) {
			t.Errorf("fired on: %q", s)
		}
	}
}

func TestAssistantClaimRx(t *testing.T) {
	for _, s := range []string{
		"Understood. I'll remember that.",
		"I've saved that as a standing rule.",
		"I have recorded the toolchain path for future sessions.",
	} {
		if !assistantClaimRx.MatchString(s) {
			t.Errorf("missed: %q", s)
		}
	}
	for _, s := range []string{
		"I've noted it in the plan.",
		"I've noted that in the commit so it's on the record.",
		"I'll note it for its owner.",
	} {
		if assistantClaimRx.MatchString(s) {
			t.Errorf("fired on: %q", s)
		}
	}
}

func triggerHarness(t *testing.T) (*Harness, *forkRunner) {
	defs := []llm.ToolDef{memoryToolDef()}
	h := newNoteHarness(t)
	r := &forkRunner{}
	h.Session = &agent.Session{SessionID: "dun", Store: h.store, Runner: r, System: "You are dun.", Tools: defs,
		Build: func(ctx context.Context, sid, system string) ([]llm.Message, error) {
			return agent.DefaultContextBuilder(ctx, h.store, sid, system)
		}}
	m, _, _ := testMemory(t)
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
