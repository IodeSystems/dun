package harness

import (
	"github.com/iodesystems/agentkit/llm"
)

// effortclamp.go: the one place a wanted effort becomes what a request
// sends, and the one adapter between dun's level names and agentkit's
// (v0.35.0: llm.Effort, Client.Effective, EffortSupport). The role resolver
// (roles.go) asks efforts.Clamp what a role's wanted level is on its model's
// profile (profile.go), and every request path writes the result with
// efforts.Apply. Nothing else in dun writes ChatOpts' thinking fields.
//
// dun's names are agentkit's, plus default (llm.EffortUnset: nothing sent),
// off (read as none) and the role intent lowest.

// EffortLowest is a role's intent, not a level: the lowest level the model
// takes (agentkit's clamp of none: the first available row of EffortSupport,
// from minimal up, when none is not). None on a model that
// can turn thinking off; minimal or above on one that cannot. Never in the
// picker.
const EffortLowest = "lowest"

// EffortChoice is what a role asked for, what its request sends, and why
// the two differ ("" when they do not), in dun's names. Wanted "" is unset:
// a child inherits the saved /effort level, a side call sends nothing.
type EffortChoice struct {
	Wanted, Effective, Reason string
}

// effortPolicy clamps a wanted level to a profile, and writes a level on a
// request's options.
type effortPolicy interface {
	Clamp(p ModelProfile, wanted string) EffortChoice
	Apply(opts *llm.ChatOpts, effective string)
}

// efforts is the policy in force.
var efforts effortPolicy = agentkitEfforts{}

// agentkitEfforts clamps with agentkit's EffortFor (through the model's
// client) and sends ChatOpts.Effort.
type agentkitEfforts struct{}

func (agentkitEfforts) Clamp(p ModelProfile, wanted string) EffortChoice {
	eff, why := clampEffort(p, wanted)
	return EffortChoice{Wanted: wanted, Effective: eff, Reason: why}
}

func (agentkitEfforts) Apply(opts *llm.ChatOpts, effective string) { setEffort(opts, effective) }

// toLLM is dun's level as agentkit's: default is unset, off is none.
func toLLM(level string) llm.Effort {
	l, _ := NormalizeEffort(level)
	if l == EffortDefault {
		return llm.EffortUnset
	}
	return llm.Effort(l)
}

// fromLLM is agentkit's level in dun's names: unset is default.
func fromLLM(e llm.Effort) string {
	if e == llm.EffortUnset {
		return EffortDefault
	}
	return string(e)
}

// clampEffort is the level sent for wanted on a model with profile p, and
// why it differs (agentkit's Reason, "minimal (none not supported: …)").
func clampEffort(p ModelProfile, wanted string) (effective, reason string) {
	switch wanted {
	case "":
		return "", ""
	case EffortDefault:
		return EffortDefault, ""
	case EffortLowest:
		// agentkit's none is already "the lowest the record allows, from
		// minimal up" when none is refused: the same row EffortSupport's
		// first available one is, with the reason none is not.
		c := p.effective(llm.EffortNone)
		return fromLLM(c.Effective), c.Reason
	}
	c := p.effective(toLLM(wanted))
	return fromLLM(c.Effective), c.Reason
}

// setEffort writes level on opts: ChatOpts.Effort, and no token budget of
// its own (as before: the level decides, and a window budget a round set is
// not sent), so default sends nothing. Only efforts.Apply calls it.
func setEffort(opts *llm.ChatOpts, level string) {
	if opts == nil {
		return
	}
	opts.Effort, opts.ReasoningBudgetTokens = toLLM(level), nil
}
