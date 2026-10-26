package dun

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/iodesystems/agentkit/llm"
)

// Effort: how much the model thinks before it answers, set with /effort
// (USER 2026-10-26). dun used to send nothing, so the server's default
// decided; a server launched with thinking off showed no thinking at all.
// A level asks for it on every request (agentkit's EnableThinking, sent as
// chat_template_kwargs.enable_thinking, and a reasoning budget):
//
//	default  nothing sent: the server's default (what dun did before; an
//	         OpenRouter model may then think and send none of it back)
//	off      thinking off
//	low      on, at most 1,024 tokens of it
//	medium   on, at most 4,096
//	high     on, no budget
//
// The level is saved in $DUN_HOME/effort.json and is every session's.
const (
	EffortDefault = "default"
	EffortOff     = "off"
	EffortLow     = "low"
	EffortMedium  = "medium"
	EffortHigh    = "high"
)

// EffortLevels are the levels, in the order a picker shows them.
var EffortLevels = []string{EffortDefault, EffortOff, EffortLow, EffortMedium, EffortHigh}

// EffortAbout is each level in words, for a picker.
var EffortAbout = map[string]string{
	EffortDefault: "the server decides (nothing sent)",
	EffortOff:     "no thinking: fastest",
	EffortLow:     "thinks, up to ~1k tokens",
	EffortMedium:  "thinks, up to ~4k tokens",
	EffortHigh:    "thinks as long as it needs",
}

var effortBudget = map[string]int{EffortLow: 1024, EffortMedium: 4096}

// effortFile is where the level is kept.
func effortFile() string { return filepath.Join(DunHome(), "effort.json") }

// LoadEffort is the saved level; default when none is saved.
func LoadEffort() string {
	var f struct {
		Level string `json:"level"`
	}
	b, err := os.ReadFile(effortFile())
	if err != nil || json.Unmarshal(b, &f) != nil || !validEffort(f.Level) {
		return EffortDefault
	}
	return f.Level
}

func validEffort(l string) bool {
	for _, x := range EffortLevels {
		if x == l {
			return true
		}
	}
	return false
}

// SetEffort sets the level for this harness's next request and saves it.
func (h *Harness) SetEffort(level string) error {
	level = strings.ToLower(strings.TrimSpace(level))
	if !validEffort(level) {
		return fmt.Errorf("effort %q: want %s", level, strings.Join(EffortLevels, ", "))
	}
	b, _ := json.Marshal(map[string]string{"level": level})
	if err := os.MkdirAll(DunHome(), 0o700); err != nil {
		return err
	}
	if err := writeFileAtomic(effortFile(), append(b, '\n'), 0o600); err != nil {
		return err
	}
	h.effort.Store(level)
	return nil
}

// Effort is the level in force.
func (h *Harness) Effort() string {
	if l, ok := h.effort.Load().(string); ok && l != "" {
		return l
	}
	return EffortDefault
}

// applyEffort sets level's thinking knobs on opts; default clears them.
// agentkit sends them in the endpoint's dialect: chat_template_kwargs and a
// budget to llama.cpp, a `reasoning` object to OpenRouter (the effort, or
// the budget as max_tokens), a thinking budget to Anthropic.
func applyEffort(opts *llm.ChatOpts, level string) {
	if opts == nil {
		return
	}
	opts.EnableThinking, opts.ReasoningBudgetTokens, opts.ReasoningEffort = nil, nil, ""
	switch level {
	case EffortOff:
		f := false
		opts.EnableThinking, opts.ReasoningEffort = &f, "none"
	case EffortLow, EffortMedium, EffortHigh:
		t := true
		opts.EnableThinking, opts.ReasoningEffort = &t, level
		if n, ok := effortBudget[level]; ok {
			opts.ReasoningBudgetTokens = &n
		}
	}
}
