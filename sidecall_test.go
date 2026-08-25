package dun

import (
	"testing"
	"time"

	"github.com/iodesystems/agentkit/llm"
)

// noteSideCall folds one finished call into the per-kind totals: repeated calls
// sum their tokens and latency, and the average is the sum over the count —
// not a mean of means, which the TUI must be able to reproduce from the event.
func TestSideCalls_AccumulatePerKind(t *testing.T) {
	h := newNoteHarness(t)
	h.sideCalls = &sideCallStats{}

	u := &llm.Usage{PromptTokens: 100, CompletionTokens: 40,
		PromptTokensDetails: &llm.PromptTokensDetails{CachedTokens: 60}, LatencyMS: 300}
	h.noteSideCall("suggest", time.Now().Add(-310*time.Millisecond), u)
	h.noteSideCall("suggest", time.Now().Add(-110*time.Millisecond), u)
	h.noteSideCall("commit", time.Now().Add(-50*time.Millisecond), &llm.Usage{PromptTokens: 50, CompletionTokens: 10})

	got := h.SideCalls()
	if len(got) != 2 {
		t.Fatalf("want 2 kinds, got %d: %v", len(got), got)
	}
	sg := got["suggest"]
	if n := sg["calls"].(int); n != 2 {
		t.Errorf("suggest calls = %v, want 2", sg["calls"])
	}
	if n := sg["latency_ms"].(int64); n != 600 {
		t.Errorf("suggest latency = %v, want 600 (the client's per-call figures summed)", sg["latency_ms"])
	}
	if n := sg["avg_ms"].(int64); n != 300 {
		t.Errorf("suggest avg = %v, want 300", sg["avg_ms"])
	}
	// 40 prompt tokens actually re-evaluated per call (100 − 60 cached), 40
	// generated — the token columns are running totals.
	p, c, g := sg["processed"].(int), sg["cached"].(int), sg["generated"].(int)
	if p != 80 || c != 120 || g != 80 {
		t.Errorf("suggest tokens = proc %v / cached %v / gen %v, want 80/120/80",
			sg["processed"], sg["cached"], sg["generated"])
	}
	// 80 tokens in 0.6s.
	if v, _ := sg["tok_per_s"].(float64); v < 100 || v > 150 {
		t.Errorf("suggest tok/s = %v, want ~133", v)
	}
	cm := got["commit"]
	cc, cp, cg := cm["calls"].(int), cm["processed"].(int), cm["generated"].(int)
	if cc != 1 || cp != 50 || cg != 10 {
		t.Errorf("commit row = %v", cm)
	}
}

// A call that fails before the provider answers (or a provider that reports no
// usage) is still counted, with its wall-clock latency and zero tokens: a
// session full of failed suggestions should show as such, not as nothing.
func TestSideCall_FailedCallStillCounted(t *testing.T) {
	h := newNoteHarness(t)
	h.sideCalls = &sideCallStats{}
	h.noteSideCall("suggest", time.Now().Add(-900*time.Millisecond), nil)
	got := h.SideCalls()["suggest"]
	if n := got["calls"].(int); n != 1 {
		t.Fatalf("a failed call is still a call: %v", got)
	}
	if v := got["latency_ms"].(int64); v < 800 {
		t.Errorf("the wall clock must cover the call, got %v ms", v)
	}
	gp, gg := got["processed"].(int), got["generated"].(int)
	if gp != 0 || gg != 0 {
		t.Errorf("no usage report means no tokens, got %v", got)
	}
	if _, ok := got["tok_per_s"]; ok {
		t.Errorf("zero generated tokens means no speed to report: %v", got)
	}
}

// An untouched harness has no side calls: SideCalls is nil, and the usage
// event's reader has to treat "no map" as "the section stays empty", not as
// "zero of every kind".
func TestSideCalls_EmptyIsNil(t *testing.T) {
	h := newNoteHarness(t)
	if h.SideCalls() != nil {
		t.Fatalf("want nil before any side call, got %v", h.SideCalls())
	}
}
