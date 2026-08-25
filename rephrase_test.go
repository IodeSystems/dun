package dun

import (
	"context"
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
)

// cannedRephraseRunner is a throwaway LLMRunner: one response, no tools, and it
// records what it saw so a test can prove the instruction (and nothing else)
// went over the wire.
type cannedRephraseRunner struct {
	reply  string
	err    error
	seen   []llm.Message
	called int
}

func (r *cannedRephraseRunner) ChatStream(_ context.Context, msgs []llm.Message, _ []llm.ToolDef, _ *llm.ChatOpts) (<-chan llm.StreamChunk, error) {
	r.called++
	cp := make([]llm.Message, len(msgs))
	copy(cp, msgs)
	r.seen = cp
	if r.err != nil {
		return nil, r.err
	}
	ch := make(chan llm.StreamChunk, 2)
	if r.reply != "" {
		ch <- llm.StreamChunk{Content: r.reply}
	}
	ch <- llm.StreamChunk{Done: true}
	close(ch)
	return ch, nil
}

func rephraseHarness(t *testing.T, runner agent.LLMRunner) *Harness {
	t.Helper()
	h := newNoteHarness(t)
	h.Session = &agent.Session{SessionID: "dun", Store: h.store, Runner: runner}
	return h
}

// The point of the feature: the rewrite is what gets returned, ready to act on.
func TestRephrase_ReturnsTheRewrite(t *testing.T) {
	r := &cannedRephraseRunner{reply: "Add a /prompt [on|off] slash command that rephrases user prompts for specificity before acting.\n\nAcceptance criteria:\n1. /prompt on flips the mode on\n2. /prompt off flips it off\n3. with it on, a vague feature request comes back with testable criteria"}
	h := rephraseHarness(t, r)
	out, err := h.Rephrase(context.Background(), "add a /prompt command")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Acceptance criteria") {
		t.Fatalf("expected the rewrite, got %q", out)
	}
	if r.called != 1 {
		t.Fatalf("one round-trip expected, runner called %d times", r.called)
	}
	if len(r.seen) != 1 || !strings.HasPrefix(r.seen[0].Content, rephraseInstruction) {
		t.Fatalf("expected a single instruction+prompt message, got %d messages: %+v", len(r.seen), r.seen)
	}
}

// A model that already agrees the prompt is concrete must not change it —
// returning the input unchanged is the "already concrete" path.
func TestRephrase_UnchangedStaysUnchanged(t *testing.T) {
	orig := "Run `go test ./...` and report the failures."
	r := &cannedRephraseRunner{reply: orig}
	h := rephraseHarness(t, r)
	out, err := h.Rephrase(context.Background(), orig)
	if err != nil {
		t.Fatal(err)
	}
	if out != orig {
		t.Fatalf("unchanged input should come back unchanged: %q", out)
	}
}

// The guards keep directives and one-word answers verbatim WITHOUT paying a
// round-trip for them.
func TestRephrase_SkipsDirectivesAndTinyMessages(t *testing.T) {
	for _, in := range []string{"/prompt on", "/suggest", "yes", "ok"} {
		r := &cannedRephraseRunner{reply: "REWRITTEN"}
		h := rephraseHarness(t, r)
		out, err := h.Rephrase(context.Background(), in)
		if err != nil {
			t.Fatal(err)
		}
		if out != in {
			t.Errorf("%q must pass through unchanged, got %q", in, out)
		}
		if r.called != 0 {
			t.Errorf("%q must not cost a round-trip, runner called %d times", in, r.called)
		}
	}
}

// Best-effort: a dead provider, an empty reply, or a nil runner all fall back
// to the original. Rephrasing is never the reason a message is not acted on.
func TestRephrase_FallsBackOnFailure(t *testing.T) {
	orig := "figure out why the tests are flaky"

	t.Run("runner error", func(t *testing.T) {
		r := &cannedRephraseRunner{err: context.DeadlineExceeded}
		h := rephraseHarness(t, r)
		out, err := h.Rephrase(context.Background(), orig)
		if err != nil {
			t.Fatalf("Rephrase must not surface errors, got %v", err)
		}
		if out != orig {
			t.Fatalf("want original, got %q", out)
		}
	})

	t.Run("empty reply", func(t *testing.T) {
		r := &cannedRephraseRunner{reply: "   \n"}
		h := rephraseHarness(t, r)
		out, _ := h.Rephrase(context.Background(), orig)
		if out != orig {
			t.Fatalf("want original on empty reply, got %q", out)
		}
	})

	t.Run("no runner", func(t *testing.T) {
		h := newNoteHarness(t)
		h.Session = &agent.Session{SessionID: "dun"}
		out, err := h.Rephrase(context.Background(), orig)
		if err != nil || out != orig {
			t.Fatalf("want original with no runner, got %q (err %v)", out, err)
		}
	})
}

// Empty input is a no-op, not an error.
func TestRephrase_EmptyInput(t *testing.T) {
	h := rephraseHarness(t, &cannedRephraseRunner{reply: "X"})
	out, err := h.Rephrase(context.Background(), "   ")
	if err != nil || out != "   " {
		t.Fatalf("empty input should pass through: %q (err %v)", out, err)
	}
}
