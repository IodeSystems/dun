package dun

// The overflow policy has exactly one job that is hard: telling a prompt that
// ate the window apart from a response that ran away. Both arrive as
// finish_reason=length, the remedies are opposites, and picking the wrong one is
// destructive — folding history to make room for a model that will spend it the
// same way costs the session its memory and fixes nothing.

import (
	"context"
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/agent"
)

// overflowHarness is a harness with a window, a calibrated meter and a runner
// that can summarize, so rescueFold has something to fold with.
func overflowHarness(t *testing.T, window int) (*Harness, *summaryRunner) {
	t.Helper()
	h := newNoteHarness(t)
	r := &summaryRunner{t: t}
	h.client = r
	h.window.Store(int64(window))
	return h, r
}

// The measured case: 109k of a 188k window, ~79k of room, and the model spent
// all of it. Nothing about the HISTORY is wrong, so nothing is folded — the
// model is told the arithmetic and asked to work smaller.
func TestOverflow_RunawayResponseGetsAHintNotACompaction(t *testing.T) {
	h, runner := overflowHarness(t, 188160)
	seedHistory(t, h, 8)
	before := len(mustEntries(t, h))

	dec := h.onOverflow(context.Background(), agent.Overflow{
		PromptTokens: 109000, Generated: 79000, Attempt: 1,
		TruncatedToolCalls: []string{"exec"},
	})

	if runner.calls != 0 {
		t.Errorf("a runaway response must not spend an LLM call folding history; %d calls", runner.calls)
	}
	if got := len(mustEntries(t, h)); got != before {
		t.Errorf("history changed: %d entries, was %d", got, before)
	}
	if dec.Note == "" {
		t.Fatal("no note — the model learns nothing and retries into the same wall")
	}
	for _, want := range []string{"109000", "188160", "79000", "CUT OFF"} {
		if !strings.Contains(dec.Note, want) {
			t.Errorf("note is missing %q; a model told to be brief without the numbers\n"+
				"cannot judge how brief:\n%s", want, dec.Note)
		}
	}
	if !strings.Contains(dec.Note, "tool call") {
		t.Errorf("a cut mid-call should say so, not talk about the reply:\n%s", dec.Note)
	}
}

// The other half: 180k of a 188k window leaves less than one response's
// reservation, and no amount of asking the model to be concise gets that back.
// Fold, and say that the room now exists.
func TestOverflow_CrowdedPromptFolds(t *testing.T) {
	h, runner := overflowHarness(t, 188160)
	seedHistory(t, h, 8)
	before := len(mustEntries(t, h))

	dec := h.onOverflow(context.Background(), agent.Overflow{
		PromptTokens: 180000, Generated: 6000, Attempt: 1,
	})

	if runner.calls != 1 {
		t.Errorf("a crowded prompt must fold; summarizer calls = %d", runner.calls)
	}
	if got := len(mustEntries(t, h)); got >= before {
		t.Errorf("nothing was folded: %d entries, was %d", got, before)
	}
	if !strings.Contains(dec.Note, "folded into a summary") {
		t.Errorf("the model should be told room was made:\n%s", dec.Note)
	}
	if !dec.Retry {
		t.Error("a cut REPLY with the history now smaller is exactly the case worth retrying")
	}
}

// A hint that did not work is not worth repeating. The second cut in one turn
// escalates to folding even when the prompt looked roomy — because whatever the
// model is doing with that room, telling it again will not change.
func TestOverflow_SecondAttemptEscalatesToFolding(t *testing.T) {
	h, runner := overflowHarness(t, 188160)
	seedHistory(t, h, 8)

	h.onOverflow(context.Background(), agent.Overflow{PromptTokens: 109000, Generated: 79000, Attempt: 1})
	if runner.calls != 0 {
		t.Fatalf("attempt 1 should only hint; calls = %d", runner.calls)
	}
	h.onOverflow(context.Background(), agent.Overflow{PromptTokens: 109000, Generated: 79000, Attempt: 2})
	if runner.calls != 1 {
		t.Errorf("attempt 2 should fold; calls = %d", runner.calls)
	}
}

// With no window stated there is no arithmetic to report, and inventing one
// would be the chars/4 mistake in a new place. Say what IS known and stop.
func TestOverflow_UnknownWindowStillHints(t *testing.T) {
	h, runner := overflowHarness(t, 0)
	seedHistory(t, h, 4)

	dec := h.onOverflow(context.Background(), agent.Overflow{Generated: 4000, Attempt: 1})

	if runner.calls != 0 {
		t.Errorf("nothing is known to be crowded, so nothing should be folded; calls = %d", runner.calls)
	}
	if !strings.Contains(dec.Note, "4000 tokens before the cut") {
		t.Errorf("the one known number should still be reported:\n%s", dec.Note)
	}
	if strings.Contains(dec.Note, "of a 0-token window") {
		t.Errorf("an unknown window must not be printed as zero:\n%s", dec.Note)
	}
}

// The hint is written in the present tense about the LAST response. Left in
// place it goes on telling the model it was just truncated, for the rest of the
// session — a permanent, unexplained timidity with no cause the model can see.
func TestOverflow_HintsAreDroppedOnceStale(t *testing.T) {
	h := newNoteHarness(t)
	ctx := context.Background()
	h.store.Append(ctx, "dun", agent.Entry{
		ID: "u1", Kind: agent.KindUser, Content: "do the thing", CreatedAt: 1,
	})
	h.store.Append(ctx, "dun", agent.Entry{
		ID: "hint", Kind: agent.KindNotification, Tag: agent.OverflowTag,
		Content: "your last response was CUT OFF", CreatedAt: 2,
	})
	h.store.Append(ctx, "dun", agent.Entry{
		ID: "n1", Kind: agent.KindNotification, Content: "a real notification", CreatedAt: 3,
	})

	h.clearOverflowHints()

	got := mustEntries(t, h)
	if len(got) != 2 {
		t.Fatalf("want 2 entries left, got %d", len(got))
	}
	for _, e := range got {
		if e.Tag == agent.OverflowTag {
			t.Error("the stale hint survived")
		}
		if e.ID == "n1" && e.Content != "a real notification" {
			t.Error("an untagged notification was collateral damage")
		}
	}
}

func mustEntries(t *testing.T, h *Harness) []agent.Entry {
	t.Helper()
	es, err := h.store.Context(context.Background(), "dun")
	if err != nil {
		t.Fatal(err)
	}
	return es
}
