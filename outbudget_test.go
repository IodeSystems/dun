package dun

// outbudget_test.go — the pure arithmetic of the window division.
//
// The window, the prompt's share, the response's cap, and the margin between
// them are the one decision this codebase makes about context. These tests pin
// down the arithmetic without touching a server: generationBudget,
// reasoningBudget, applyGeneration, and the WindowBudget accessors.

import (
	"testing"

	"github.com/iodesystems/agentkit/llm"
)

// generationBudget is the core: the room left for one response, capped by the
// hard limit, and ok=false when there is no honest budget to send.
func TestGenerationBudget_KnownWindow(t *testing.T) {
	t.Setenv("DUN_MAX_OUTPUT_TOKENS", "")
	cap := defaultMaxOutputTokens

	// Plenty of room: room is the cap.
	if got, ok := generationBudget(220_000, 100_000); !ok || got != cap {
		t.Errorf("room to spare: got %d ok=%v, want %d true", got, ok, cap)
	}

	// Room is less than the cap: room wins.
	// room = window - promptTokens - outputMargin
	// promptTokens = 220_000 - cap - 1
	// room = 220_000 - (220_000 - cap - 1) - outputMargin = cap + 1 - outputMargin
	if got, ok := generationBudget(220_000, 220_000-cap-1); !ok || got != cap+1-outputMargin {
		t.Errorf("tight room: got %d ok=%v, want %d true", got, ok, cap+1-outputMargin)
	}

	// Room is below minOutputTokens: no honest budget.
	if got, ok := generationBudget(220_000, 220_000-outputMargin-minOutputTokens+1); ok {
		t.Errorf("below the floor should not be ok: got %d", got)
	}

	// Unknown window: no budget.
	if _, ok := generationBudget(0, 1000); ok {
		t.Error("zero window must not be ok")
	}
}

func TestGenerationBudget_EnvCap(t *testing.T) {
	t.Setenv("DUN_MAX_OUTPUT_TOKENS", "8192")
	// Room is large; cap is 8192.
	if got, ok := generationBudget(1_000_000, 1000); !ok || got != 8192 {
		t.Errorf("env cap should win: got %d ok=%v, want 8192 true", got, ok)
	}
	// Room is less than the cap: room wins.
	if got, ok := generationBudget(10_000, 1000); !ok || got != 10_000-1000-outputMargin {
		want := 10_000 - 1000 - outputMargin
		t.Errorf("tight room with env cap: got %d ok=%v, want %d", got, ok, want)
	}
}

// reasoningBudget is the 2/3 split for thinking vs. answer.
func TestReasoningBudget(t *testing.T) {
	if got, want := reasoningBudget(3000), 2000; got != want {
		t.Errorf("reasoningBudget(3000) = %d, want %d", got, want)
	}
	if got, want := reasoningBudget(3), 2; got != want {
		t.Errorf("reasoningBudget(3) = %d, want %d", got, want)
	}
	if got, want := reasoningBudget(0), 0; got != want {
		t.Errorf("reasoningBudget(0) = %d, want %d", got, want)
	}
}

// applyGeneration writes the caps onto ChatOpts and reports what it set.
func TestApplyGeneration(t *testing.T) {
	t.Setenv("DUN_MAX_OUTPUT_TOKENS", "")

	// nil opts: nothing to write.
	if _, ok := applyGeneration(nil, 220_000, 100_000); ok {
		t.Error("nil opts must not be ok")
	}

	// No honest budget: previous caps are left in place.
	prev := llm.ChatOpts{MaxTokens: 9999, ReasoningBudgetTokens: func() *int { v := 5555; return &v }()}
	gen, ok := applyGeneration(&prev, 220_000, 220_000-outputMargin-minOutputTokens+1)
	if ok {
		t.Errorf("below the floor must not be ok: got %d", gen)
	}
	if prev.MaxTokens != 9999 {
		t.Errorf("previous MaxTokens should be untouched: got %d", prev.MaxTokens)
	}

	// Happy path: caps are written.
	opts := &llm.ChatOpts{}
	gen, ok = applyGeneration(opts, 220_000, 100_000)
	if !ok {
		t.Fatalf("should be ok: got %d", gen)
	}
	if opts.MaxTokens != gen {
		t.Errorf("MaxTokens = %d, want %d", opts.MaxTokens, gen)
	}
	if opts.ReasoningBudgetTokens == nil {
		t.Error("ReasoningBudgetTokens should be set")
	} else if *opts.ReasoningBudgetTokens != reasoningBudget(gen) {
		t.Errorf("ReasoningBudgetTokens = %d, want %d", *opts.ReasoningBudgetTokens, reasoningBudget(gen))
	}
}

// WindowBudget accessors: Free and Known are pure.
func TestWindowBudget_Accessors(t *testing.T) {
	// Unknown window.
	var b WindowBudget
	if b.Free() != 0 {
		t.Errorf("Free() with unknown window = %d, want 0", b.Free())
	}
	if b.Known() {
		t.Error("Known() should be false with zero window")
	}

	// Known window, prompt has used some of it.
	b = WindowBudget{Window: 220_000, Prompt: 100_000}
	if got := b.Free(); got != 120_000 {
		t.Errorf("Free() = %d, want 120000", got)
	}
	if !b.Known() {
		t.Error("Known() should be true with a stated window")
	}

	// Prompt has overrun the window: Free is negative and not clamped.
	b = WindowBudget{Window: 100_000, Prompt: 150_000}
	if got := b.Free(); got != -50_000 {
		t.Errorf("Free() overrun = %d, want -50000", got)
	}
}
