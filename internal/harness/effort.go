package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/dun/internal/fsutil"
)

// Effort: how much the model thinks before it answers, set with /effort
// (USER 2026-10-26). dun used to send nothing, so the server's default
// decided; a server launched with thinking off showed no thinking at all.
// A level asks for it on every request: agentkit's ChatOpts.Effort (since
// v0.35.0), which writes each dialect's fields and clamps the level to the
// capability record (effortclamp.go):
//
//	default        nothing sent: the server's default (what dun did before;
//	               an OpenRouter model may then think and send none of it back)
//	none           thinking off; off, the old name, is read as it
//	minimal … max  agentkit's levels, lowest first (llm.Efforts)
//
// The level is saved in $DUN_HOME/effort.json and is every session's.
const (
	EffortDefault = "default"
	EffortOff     = "none" // thinking off; "off" is read as it (NormalizeEffort)
	EffortMinimal = "minimal"
	EffortLow     = "low"
	EffortMedium  = "medium"
	EffortHigh    = "high"
	EffortXHigh   = "xhigh"
	EffortMax     = "max"
)

// effortAlias is off, the name dun saved before agentkit's scale.
const effortAlias = "off"

// EffortLevels are the levels, in the order a picker shows them: default,
// then agentkit's.
var EffortLevels = func() []string {
	out := []string{EffortDefault}
	for _, e := range llm.Efforts {
		out = append(out, string(e))
	}
	return out
}()

// EffortAbout is each level in words, for a picker.
var EffortAbout = func() map[string]string {
	out := map[string]string{EffortDefault: "the server decides (nothing sent)"}
	for _, e := range llm.Efforts {
		out[string(e)] = llm.EffortAbout[e]
	}
	out[EffortOff] = "no thinking: fastest"
	return out
}()

// NormalizeEffort is l as a level: trimmed, lower case, off read as none.
// False when it is no level.
func NormalizeEffort(l string) (string, bool) {
	l = strings.ToLower(strings.TrimSpace(l))
	if l == effortAlias {
		l = EffortOff
	}
	return l, validEffort(l)
}

// effortFile is where the level is kept.
func effortFile() string { return filepath.Join(DunHome(), "effort.json") }

// LoadEffort is the saved level; default when none is saved.
func LoadEffort() string {
	l, _ := SavedEffort()
	return l
}

// SavedEffort is the saved level, and whether one is saved (else default):
// facets.main's effort applies only when /effort has saved none. An older
// file's off reads as none.
func SavedEffort() (string, bool) {
	var f struct {
		Level string `json:"level"`
	}
	b, err := os.ReadFile(effortFile())
	if err != nil || json.Unmarshal(b, &f) != nil {
		return EffortDefault, false
	}
	l, ok := NormalizeEffort(f.Level)
	if !ok {
		return EffortDefault, false
	}
	return l, true
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
	level, ok := NormalizeEffort(level)
	if !ok {
		return fmt.Errorf("effort %q: want %s (off is none)", level, strings.Join(EffortLevels, ", "))
	}
	b, _ := json.Marshal(map[string]string{"level": level})
	if err := os.MkdirAll(DunHome(), 0o700); err != nil {
		return err
	}
	if err := fsutil.WriteAtomic(effortFile(), append(b, '\n'), 0o600); err != nil {
		return err
	}
	h.effort.Store(level)
	h.effortSrc.Store(srcEffort)
	h.writeHarnessInfo()
	return nil
}

// EffortPick is one row of the /effort picker: a level, what it does, and,
// when the session's model does not take it as asked, why (agentkit's
// EffortSupport Reason, "minimal (none not supported: …)").
type EffortPick struct {
	Level, About, Unavailable string
}

// EffortPicker is the /effort picker's rows for the session's model now:
// default, then every level lowest first, each marked unavailable when the
// model's clamp sends another.
func (h *Harness) EffortPicker() []EffortPick {
	out := []EffortPick{{Level: EffortDefault, About: h.EffortWords(EffortDefault)}}
	for _, c := range h.sessionProfile().Support() {
		p := EffortPick{Level: string(c.Wanted), About: EffortAbout[string(c.Wanted)]}
		if c.Effective != c.Wanted {
			p.Unavailable = c.Reason
		}
		out = append(out, p)
	}
	return out
}

// Effort is the level in force.
func (h *Harness) Effort() string {
	if l, ok := h.effort.Load().(string); ok && l != "" {
		return l
	}
	return EffortDefault
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
	if !getModels(ctx, e, &models) {
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
		h.declareThinkingOnly(t)
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
