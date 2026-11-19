package harness

import (
	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
)

// A model whose reasoning cannot be turned off refuses every request that
// sends off. OpenRouter marks such a model reasoning.mandatory in its model
// list, and answers off (agentkit sends reasoning: {enabled: false}) with
// (USER 2026-11-18, google/gemini-4-hydrogen):
//
//	400 {"error":{"message":"Reasoning is mandatory for this endpoint and
//	cannot be disabled.","code":400,"metadata":{"provider_name":null}}}
//
// dun sends off for a fold (foldOpts), for a round at /effort off, and
// agentkit sends it for its reasoning-cut repair round. agentkit (v0.34.0)
// handles the refusal: it learns reasoning.thinking_only from the body, sends
// the request once more with off rewritten to the lowest effort (every
// dialect's off form), and from then on sends that effort from the start.
// dun stores the record (capstore.go, OnLearn), so a new client knows it too.
// dun had its own resend here (offGuard) until agentkit did it in every
// dialect; dun's part now is the note below.

// sendsOff reports whether opts asks for reasoning off, in any of the forms
// applyEffort and agentkit's reasoningOff write.
func sendsOff(opts *llm.ChatOpts) bool {
	if opts == nil {
		return false
	}
	return opts.ReasoningEffort == "none" ||
		(opts.EnableThinking != nil && !*opts.EnableThinking) ||
		(opts.ReasoningBudgetTokens != nil && *opts.ReasoningBudgetTokens == 0)
}

// thinkingOnly reports whether r's capability record says off is refused.
// False when r keeps none (a fake).
func thinkingOnly(r agent.LLMRunner) bool {
	c, ok := r.(interface{ Capabilities() llm.Capabilities })
	if !ok {
		return false
	}
	only, ok := c.Capabilities().ThinkingOnly()
	return ok && only
}

// noteOffRefused tells the person, once per session, that /effort off is
// not what their model gets. Only when they chose off: a fold's off is
// dun's, and agentkit's repair round's is agentkit's. The tee calls it after
// a round that asked for off on a model the record says is thinking-only.
func (h *Harness) noteOffRefused() {
	if h.Effort() != EffortOff || h.cfg.OnNotify == nil || h.offNoted.Swap(true) {
		return
	}
	h.cfg.OnNotify("effort off: " + h.cfg.Endpoint.Model + " cannot turn reasoning off; it is sent at the lowest effort it takes")
}
