package harness

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
)

// cutRunner answers every request one way: "length" (a partial reply the
// provider cut at its ceiling), "empty" (a finish and nothing else) or "ok".
// It keeps what the last request sent.
type cutRunner struct {
	mode string
	mu   sync.Mutex
	seen []llm.Message
}

func (r *cutRunner) ChatStream(_ context.Context, msgs []llm.Message, _ []llm.ToolDef, _ *llm.ChatOpts) (<-chan llm.StreamChunk, error) {
	r.mu.Lock()
	r.seen = append([]llm.Message(nil), msgs...)
	r.mu.Unlock()
	ch := make(chan llm.StreamChunk, 4)
	switch r.mode {
	case "length":
		ch <- llm.StreamChunk{Content: "## Task\nhalf a summ"}
		ch <- llm.StreamChunk{Done: true, StopReason: llm.StopReasonLength, FinishReason: "length"}
		ch <- llm.StreamChunk{Usage: &llm.Usage{CompletionTokens: 10}}
	case "empty":
		ch <- llm.StreamChunk{Done: true, FinishReason: "stop"}
	default:
		ch <- llm.StreamChunk{Content: "fine"}
		ch <- llm.StreamChunk{Done: true, FinishReason: "stop"}
	}
	close(ch)
	return ch, nil
}

func (r *cutRunner) last() []llm.Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.seen
}

// Every side call, given a reply cut at the length limit or an empty one,
// fails as a call and does not panic or take the partial text.
func TestSideGuard_LengthAndEmptyFailEveryCall(t *testing.T) {
	for _, mode := range []string{"length", "empty"} {
		t.Run(mode, func(t *testing.T) {
			r := &cutRunner{mode: mode}
			h := rephraseHarness(t, r)
			var told []string
			h.cfg.OnNotify = func(s string) { told = append(told, s) }
			ctx := context.Background()

			const task = "please add the thing to the other thing"
			if out, _ := h.Rephrase(ctx, task); out != task {
				t.Errorf("rephrase: got %q, want the original", out)
			}
			for _, kind := range []string{"journal", "plan_summary"} {
				if _, _, err := h.askOnce(ctx, kind, "write it"); err == nil {
					t.Errorf("askOnce %s: no error", kind)
				}
			}
			if _, err := h.learnCall(ctx, "a lesson"); err == nil {
				t.Error("learn: no error")
			}
			set := rulesSet{Imports: []rulesDoc{{Name: "a.md", Text: "rule one"}}}
			if _, err := h.summarizeRules(ctx, set); err == nil {
				t.Error("rules: no error")
			}
			fr := foldRunner{inner: r, h: h}
			text, errMsg, err := fr.call(ctx, []llm.Message{{Role: "user", Content: "fold this"}}, nil, nil)
			if err == nil && errMsg == "" {
				t.Errorf("fold: a %s reply was taken as a summary: %q", mode, text)
			}
			for _, kind := range []string{"suggest", "remember", "commit", "repaint", "rescue"} {
				_, errMsg, _, err := collectText(ctx, h.sideRunner(kind, r), []llm.Message{{Role: "user", Content: "x"}}, nil, nil)
				if err == nil && errMsg == "" {
					t.Errorf("%s: no failure", kind)
				}
			}
			// One notice each for the fold and the rescue, the calls that
			// lose history; the others are logged only.
			if len(told) != 2 || !strings.Contains(told[0], "fold failed") {
				t.Errorf("notices: %q, want the fold's and the rescue's", told)
			}
		})
	}
}

// A fold reply that the model returned whole still passes the guard.
func TestSideGuard_OKPasses(t *testing.T) {
	r := &cutRunner{mode: "ok"}
	h := rephraseHarness(t, r)
	text, errMsg, _, err := collectText(context.Background(), h.sideRunner("fold", r), []llm.Message{{Role: "user", Content: "x"}}, nil, nil)
	if err != nil || errMsg != "" || text != "fine" {
		t.Fatalf("got %q %q %v", text, errMsg, err)
	}
}

// An input over the window is clipped before it is sent, with a marker, and
// the caller's messages are not changed.
func TestSideGuard_OversizeInputIsCapped(t *testing.T) {
	r := &cutRunner{mode: "ok"}
	h := rephraseHarness(t, r)
	h.window.Store(4000)
	huge := strings.Repeat("all work and no play ", 20000) // ~420k chars, ~105k tokens
	task := "rewrite this " + huge
	if _, err := h.Rephrase(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	sent := r.last()
	if len(sent) != 1 {
		t.Fatalf("sent %d messages", len(sent))
	}
	got := sent[0].Content
	if n := estimateTokens(got); n > 4000 {
		t.Errorf("sent ~%d tokens into a 4000-token window", n)
	}
	if !strings.Contains(got, "cut to fit the window") {
		t.Error("no clip marker")
	}
	if !strings.HasPrefix(got, rephraseInstruction) {
		t.Error("the head (the instruction) was not kept")
	}

	// Unknown window: the fallback cap applies.
	h.window.Store(0)
	msgs := []llm.Message{{Role: "system", Content: "sys"}, {Role: "user", Content: huge}}
	out := capSideInput(h.sideInputLimit(nil), msgs, nil, nil)
	if n := inputTokens(out, nil, nil); n > sideCapUnknown {
		t.Errorf("unknown window: ~%d tokens sent", n)
	}
	if msgs[1].Content != huge {
		t.Error("the caller's message was changed")
	}
}

// A request under the limit goes out unchanged.
func TestSideGuard_SmallInputUnchanged(t *testing.T) {
	msgs := []llm.Message{{Role: "user", Content: "short"}}
	if out := capSideInput(1000, msgs, nil, nil); &out[0] != &msgs[0] {
		t.Error("a small request was copied or clipped")
	}
}

// fold.go: a rejected summary's retry on a request with no messages does
// not index an empty slice.
func TestFoldRunner_EmptyRequestNoPanic(t *testing.T) {
	r := &cutRunner{mode: "ok"} // "fine" fails the summary check
	h := rephraseHarness(t, r)
	fr := foldRunner{inner: r, h: h}
	ctx := agentFoldCtx(t, h, fr)
	ch, _ := fr.ChatStream(ctx, nil, nil, nil)
	for range ch {
	}
}

// agentFoldCtx is a ctx agent.IsFold reports true for, captured from a real
// Shaper fold (shaperFold).
func agentFoldCtx(t *testing.T, _ *Harness, _ foldRunner) context.Context {
	t.Helper()
	var got context.Context
	var once sync.Once
	shaperFold(t, capRunner{f: func(ctx context.Context) { once.Do(func() { got = ctx }) }})
	if got == nil || !agent.IsFold(got) {
		t.Fatal("no fold ctx from the Shaper")
	}
	return got
}

type capRunner struct{ f func(context.Context) }

func (c capRunner) ChatStream(ctx context.Context, _ []llm.Message, _ []llm.ToolDef, _ *llm.ChatOpts) (<-chan llm.StreamChunk, error) {
	c.f(ctx)
	ch := make(chan llm.StreamChunk, 2)
	ch <- llm.StreamChunk{Content: "## Task\ns"}
	ch <- llm.StreamChunk{Done: true}
	close(ch)
	return ch, nil
}

// RecoverSide at a goroutine root keeps a panic there from ending the
// process.
func TestRecoverSide_StopsAPanic(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer RecoverSide("test")
		var s []string
		_ = s[0]
	}()
	<-done
}
