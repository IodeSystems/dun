package dun

// The rescue ladder: what happens when the Shaper's estimate was right about the
// budget but wrong about the TOKENIZER, and the endpoint refused the prompt.
// These tests cover the split-and-fold mechanics and the pass cap — with a fake
// runner, because the whole point is that this fires on an error the model never
// gets to answer.

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
)

// refusal is the error shape a too-large prompt produces.
func refusal() error {
	return &llm.InputTooLargeError{Status: 500, Body: "input (35871 tokens) is too large to process"}
}

// summaryRunner answers every ChatStream with a fixed summary — the compaction
// worker's job, and nothing else. It fails loudly if anything but a single user
// message reaches it, so a rescue that accidentally sends the whole conversation
// as the fold prompt (reproducing the overflow) is caught here.
type summaryRunner struct {
	t        *testing.T
	calls    int
	lastUser string
}

func (r *summaryRunner) ChatStream(ctx context.Context, msgs []llm.Message, _ []llm.ToolDef, _ *llm.ChatOpts) (<-chan llm.StreamChunk, error) {
	r.calls++
	for i, m := range msgs {
		if m.Role == "user" {
			r.lastUser = m.Content
		} else if m.Role != "system" {
			r.t.Errorf("rescue call %d: unexpected role %q at %d", r.calls, m.Role, i)
		}
	}
	ch := make(chan llm.StreamChunk, 1)
	ch <- llm.StreamChunk{Content: "OPEN ASKS: finish the thing.\nSTATE: x done.\nNEXT: y.", Done: true}
	close(ch)
	return ch, nil
}

// seedHistory fills a harness store with n user/assistant pairs plus a few big
// tool results — the shape of a session that has been working for a while.
func seedHistory(t *testing.T, h *Harness, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		h.store.Append(context.Background(), "dun", agent.Entry{
			ID: "u" + itoa(i), Kind: agent.KindUser, Content: "ask " + itoa(i), CreatedAt: int64(100 + i*2),
		})
		h.store.Append(context.Background(), "dun", agent.Entry{
			ID: "a" + itoa(i), Kind: agent.KindAssistant, Content: "reply " + itoa(i), CreatedAt: int64(101 + i*2),
		})
	}
	// A dominant tool result: the thing that actually overflows a window.
	h.store.Append(context.Background(), "dun", agent.Entry{
		ID: "big", Kind: agent.KindToolResult, ToolName: "exec",
		Content: strings.Repeat("log line\n", 4000), CreatedAt: int64(100 + n*2),
	})
}

func itoa(i int) string { return strconv.Itoa(i) }

// A turn that dies with an endpoint refusal is rescued: the older half of the
// history becomes one marker at the FRONT, and the turn retries on the re-rooted
// session. The retry succeeds where the original could not.
func TestRescue_FoldsAndRetries(t *testing.T) {
	h := newNoteHarness(t)
	runner := &summaryRunner{t: t}
	h.client = runner
	seedHistory(t, h, 8)

	attempts := 0
	res, err := h.runTurnWithRescue(context.Background(), func(context.Context) (agent.TurnResult, error) {
		attempts++
		if attempts == 1 {
			return agent.TurnResult{}, refusal()
		}
		return agent.TurnResult{Reply: "back in budget"}, nil
	})
	if err != nil {
		t.Fatalf("runTurnWithRescue: %v", err)
	}
	if res.Reply != "back in budget" || attempts != 2 {
		t.Fatalf("reply=%q attempts=%d; want the retry's result after one refusal", res.Reply, attempts)
	}
	if runner.calls != 1 {
		t.Fatalf("fold prompt sent %d times; want exactly one", runner.calls)
	}

	entries, _ := h.store.Context(context.Background(), "dun")
	// The marker must be FIRST: it is "what happened earlier", and a marker
	// appended at the end would float newer than the live tail.
	if entries[0].Kind != agent.KindCompaction {
		t.Fatalf("front entry is %q; want the compaction marker at the head", entries[0].Kind)
	}
	if !strings.Contains(entries[0].Content, "OPEN ASKS") {
		t.Errorf("marker lost the handoff: %q", entries[0].Content)
	}
	// Half of the 17 foldable entries (8 pairs + big result) were folded.
	wantFolded := 17 / 2
	gotFolded := runner.calls // one fold per call
	if gotFolded != 1 {
		t.Fatalf("folded %d times; want 1", gotFolded)
	}
	remaining := len(entries) - 1 // minus the marker
	if remaining != 17-wantFolded {
		t.Errorf("history after fold = %d entries; want %d (17 folded by half)", remaining, 17-wantFolded)
	}
	// The fold prompt must have carried the OPEN ASKS instruction — that is the
	// whole point of a state handoff rather than a transcript digest.
	if !strings.Contains(runner.lastUser, "OPEN ASKS") || !strings.Contains(runner.lastUser, "STATE OF WORK") {
		t.Errorf("fold prompt does not demand the handoff fields:\n%s", runner.lastUser)
	}
}

// A second refusal folds again (chaining), and a third — with nothing left to
// fold — fails with a clear error instead of looping forever.
func TestRescue_ChainsThenGivesUp(t *testing.T) {
	h := newNoteHarness(t)
	runner := &summaryRunner{t: t}
	h.client = runner
	seedHistory(t, h, 8)

	attempts := 0
	_, err := h.runTurnWithRescue(context.Background(), func(context.Context) (agent.TurnResult, error) {
		attempts++
		return agent.TurnResult{}, refusal()
	})
	if err == nil {
		t.Fatal("a prompt that never fits must not return success")
	}
	if !strings.Contains(err.Error(), "still does not fit after 2 rescue passes") {
		t.Fatalf("error should name the exhausted ladder: %v", err)
	}
	if attempts != 3 { // original + retry after each of the 2 folds
		t.Fatalf("attempts = %d; want 3 (1 + maxRescuePasses)", attempts)
	}
	if runner.calls != maxRescuePasses {
		t.Fatalf("folds = %d; want %d", runner.calls, maxRescuePasses)
	}
}

// A non-refusal error is not a rescue case: it flows through untouched.
func TestRescue_IgnoresOtherErrors(t *testing.T) {
	h := newNoteHarness(t)
	runner := &summaryRunner{t: t}
	h.client = runner
	seedHistory(t, h, 8)

	_, err := h.runTurnWithRescue(context.Background(), func(context.Context) (agent.TurnResult, error) {
		// A deterministic failure: runTurn's transient retry would otherwise sit
		// out the real backoff schedule before giving up.
		return agent.TurnResult{}, errors.New("model looped 3 times in a row")
	})
	if err == nil || strings.Contains(err.Error(), "rescue") {
		t.Fatalf("a stream death is not a refusal; got %v", err)
	}
	if runner.calls != 0 {
		t.Fatalf("no fold may run for a non-refusal; runner called %d times", runner.calls)
	}
}

// inputTooLarge recognizes both the typed error and a text-only one from a
// runner that is not *llm.Client.
func TestInputTooLarge(t *testing.T) {
	if !inputTooLarge(refusal()) {
		t.Error("typed InputTooLargeError must match")
	}
	if !inputTooLarge(errors.New("agent: chat: context length exceeded — prompt exceeds the available context")) {
		t.Error("text-only refusal must match")
	}
	if inputTooLarge(midStreamDeath()) {
		t.Error("a stream death is not a refusal")
	}
}
