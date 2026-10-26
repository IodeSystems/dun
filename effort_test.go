package dun

import (
	"testing"

	"github.com/iodesystems/agentkit/llm"
)

// Each level's knobs on a request, and the level saved for the next
// session (USER 2026-10-26: a server with thinking off by default showed
// none; dun sent nothing to ask for it).
func TestEffort(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	for level, want := range map[string]struct {
		think  *bool
		budget int // -1: none sent
	}{
		EffortDefault: {nil, -1},
		EffortOff:     {ptr(false), -1},
		EffortLow:     {ptr(true), 1024},
		EffortMedium:  {ptr(true), 4096},
		EffortHigh:    {ptr(true), -1},
	} {
		o := &llm.ChatOpts{}
		applyEffort(o, EffortHigh) // a level before, cleared by the next
		applyEffort(o, level)
		if (o.EnableThinking == nil) != (want.think == nil) || (o.EnableThinking != nil && *o.EnableThinking != *want.think) {
			t.Errorf("%s: thinking %v", level, o.EnableThinking)
		}
		if (o.ReasoningBudgetTokens == nil) != (want.budget < 0) || (o.ReasoningBudgetTokens != nil && *o.ReasoningBudgetTokens != want.budget) {
			t.Errorf("%s: budget %v", level, o.ReasoningBudgetTokens)
		}
	}
	h := &Harness{}
	if h.Effort() != EffortDefault || LoadEffort() != EffortDefault {
		t.Fatal("not default before one is set")
	}
	if err := h.SetEffort("HIGH"); err != nil || h.Effort() != EffortHigh || LoadEffort() != EffortHigh {
		t.Errorf("set: %v %s %s", err, h.Effort(), LoadEffort())
	}
	if err := h.SetEffort("max"); err == nil {
		t.Error("an unknown level was taken")
	}
}

func ptr[T any](v T) *T { return &v }
