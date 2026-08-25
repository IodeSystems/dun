package dun

import (
	"context"
	"testing"
)

// flakyWindower states a window only from the nth ask onward, which is the shape
// of the failure this retry exists for: an endpoint that is briefly unable to
// answer and then perfectly able to.
type flakyWindower struct {
	answerFrom int // 0 = never answers
	asks       int
}

func (f *flakyWindower) ContextWindow(context.Context) (int, bool) {
	f.asks++
	if f.answerFrom > 0 && f.asks >= f.answerFrom {
		return 188160, true
	}
	return 0, false
}

// newWindowHarness is a Harness with the window unresolved and the runner still
// on the hook, i.e. exactly what Start leaves behind when the ask misses.
func newWindowHarness(t *testing.T, w *flakyWindower) *Harness {
	t.Helper()
	t.Setenv("DUN_CONTEXT_TOKENS", "") // the ambient env must not decide this
	return &Harness{windowRunner: w}
}

// The whole point: a session that started without a window adopts one as soon as
// the endpoint can state it, instead of running unshaped and uncapped forever.
func TestEnsureWindowAdoptsMidSession(t *testing.T) {
	w := &flakyWindower{answerFrom: 2}
	h := newWindowHarness(t, w)
	ctx := context.Background()

	h.ensureWindow(ctx) // ask 1: still nothing
	if got := h.windowTokens(); got != 0 {
		t.Fatalf("window = %d after a failed ask, want 0", got)
	}
	// The second ask is one build later (backoff of 1), so the build in between
	// must not ask at all.
	h.ensureWindow(ctx)
	if w.asks != 1 {
		t.Fatalf("asks = %d during the backoff, want 1", w.asks)
	}
	h.ensureWindow(ctx)
	if got := h.windowTokens(); got != 188160 {
		t.Fatalf("window = %d after the endpoint answered, want 188160", got)
	}
	// Answered once is answered for good: no further asks, no further cost.
	h.ensureWindow(ctx)
	h.ensureWindow(ctx)
	if w.asks != 2 {
		t.Fatalf("asks = %d after the window was adopted, want 2", w.asks)
	}
}

// An endpoint that will never state one must not be asked forever: the retry is
// bounded, and the cost of a permanently silent server is windowRetries
// timeouts over the session, not one per build.
func TestEnsureWindowGivesUp(t *testing.T) {
	w := &flakyWindower{} // never answers
	h := newWindowHarness(t, w)
	ctx := context.Background()

	// Enough builds to exhaust the doubling backoff several times over.
	for i := 0; i < 500; i++ {
		h.ensureWindow(context.Background())
	}
	if w.asks != windowRetries {
		t.Fatalf("asks = %d, want %d (the bound)", w.asks, windowRetries)
	}
	if h.windowRunner != nil {
		t.Fatal("a given-up runner must be dropped, so nothing asks again")
	}
	if got := h.windowTokens(); got != 0 {
		t.Fatalf("window = %d with no answer, want 0", got)
	}
	h.ensureWindow(ctx)
	if w.asks != windowRetries {
		t.Fatalf("asks = %d after giving up, want %d", w.asks, windowRetries)
	}
}

// The backoff doubles rather than asking on every build — otherwise a dead
// endpoint costs every single round a 10s timeout.
func TestEnsureWindowBacksOff(t *testing.T) {
	w := &flakyWindower{}
	h := newWindowHarness(t, w)
	// Asks land at builds 1, 3, 6, 11, 20, 37 — the gaps doubling 1, 2, 4, 8, 16.
	want := map[int]int{1: 1, 2: 1, 3: 2, 5: 2, 6: 3, 10: 3, 11: 4, 19: 4, 20: 5, 36: 5, 37: 6}
	for build := 1; build <= 40; build++ {
		h.ensureWindow(context.Background())
		if n, ok := want[build]; ok && w.asks != n {
			t.Fatalf("build %d: asks = %d, want %d", build, w.asks, n)
		}
	}
}

// A session that already knows its window never asks again — the retry is for
// the miss, not a poll.
func TestEnsureWindowNoopWhenKnown(t *testing.T) {
	w := &flakyWindower{answerFrom: 1}
	h := &Harness{} // Start leaves windowRunner nil once it has an answer
	h.window.Store(188160)
	h.ensureWindow(context.Background())
	if w.asks != 0 {
		t.Fatalf("asks = %d with a known window, want 0", w.asks)
	}
	if got := h.windowTokens(); got != 188160 {
		t.Fatalf("window = %d, want it left alone", got)
	}
}

// DUN_CONTEXT_TOKENS answers without touching the endpoint at all: a retry must
// still prefer the operator's number over the server's.
func TestEnsureWindowPrefersEnv(t *testing.T) {
	w := &flakyWindower{answerFrom: 1}
	h := newWindowHarness(t, w)
	t.Setenv("DUN_CONTEXT_TOKENS", "40000")
	h.ensureWindow(context.Background())
	if got := h.windowTokens(); got != 40000 {
		t.Fatalf("window = %d, want the environment's 40000", got)
	}
	if w.asks != 0 {
		t.Fatalf("asks = %d, want the endpoint left alone", w.asks)
	}
}
