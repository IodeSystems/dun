package dun

import (
	"context"
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
	h.writeHarnessInfo()
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

// ServerThinking is what the model list says the session's model does when
// a request sends no thinking settings: corrallm's thinking_* fields beside
// context_length (USER 2026-10-30: report the default effort). Each is
// absent when the server does not know; Default "" is "not stated".
type ServerThinking struct {
	Default string   `json:"thinking_default"` // on | off
	Modes   []string `json:"thinking_modes"`
	Effort  string   `json:"thinking_effort"`
	// Budget applies to a thinking request that sends no budget.
	Budget *struct {
		Fraction float64 `json:"fraction_of_remaining"`
		Min      int     `json:"min"`
		Max      int     `json:"max"`
	} `json:"thinking_budget"`
}

// listedThinking is e's model's entry's thinking fields in /models.
func listedThinking(ctx context.Context, e Endpoint) (ServerThinking, bool) {
	if e.URL == "" || e.Model == "" {
		return ServerThinking{}, false
	}
	var models struct {
		Data []struct {
			ID string `json:"id"`
			ServerThinking
		} `json:"data"`
	}
	if !getListed(ctx, e, strings.TrimRight(e.URL, "/")+"/models", &models) {
		return ServerThinking{}, false
	}
	for _, m := range models.Data {
		if m.ID == e.Model {
			t := m.ServerThinking
			return t, t.Default != "" || t.Effort != "" || t.Budget != nil
		}
	}
	return ServerThinking{}, false
}

// LearnServerThinking asks the model list what the default effort does;
// it reports whether the server said. Off the turn path: one GET.
func (h *Harness) LearnServerThinking(ctx context.Context) bool {
	cctx, cancel := context.WithTimeout(ctx, contextWindowTimeout)
	defer cancel()
	t, ok := listedThinking(cctx, h.cfg.Endpoint)
	if ok {
		h.thinking.Store(&t)
	}
	return ok
}

// EffortWords is level in words; for default, what the server does when
// its model list says (else EffortAbout's "the server decides").
func (h *Harness) EffortWords(level string) string {
	if t := h.thinking.Load(); level == EffortDefault && t != nil {
		return t.words()
	}
	return EffortAbout[level]
}

// words is the server's default, e.g. "nothing sent: the server thinks at
// medium, up to 20% of what is left (2k–8k tokens)".
func (t ServerThinking) words() string {
	switch t.Default {
	case "off":
		return "nothing sent: the server does not think"
	case "":
		return EffortAbout[EffortDefault]
	}
	w := "nothing sent: the server thinks"
	if t.Effort != "" {
		w += " at " + t.Effort
	}
	if b := t.Budget; b != nil && b.Fraction > 0 {
		w += fmt.Sprintf(", up to %d%% of what is left", int(b.Fraction*100+0.5))
		if b.Min > 0 && b.Max > 0 {
			w += fmt.Sprintf(" (%s–%s tokens)", tokK(b.Min), tokK(b.Max))
		}
	}
	return w
}

// tokK is a token count the way the levels say it: 2048 is "2k".
func tokK(n int) string {
	if n >= 1024 && n%1024 == 0 {
		return fmt.Sprintf("%dk", n/1024)
	}
	return fmt.Sprint(n)
}
