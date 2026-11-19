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
// dun asks for none (off) for a fold and rescue (their role's lowest), for a
// round at /effort none, and agentkit sends it for its reasoning-cut repair
// round. Once the record says thinking_only, the role resolver (roles.go)
// already sends the lowest level the model takes (agentkit's clamp, since
// v0.35.0). Before it says, agentkit handles the refusal: it learns
// reasoning.thinking_only from the body, sends the request once more at the
// lowest level, and from then on sends that level from the start. dun stores
// the record (capstore.go, OnLearn), so a new client knows it too. dun's part
// is the note below.

// sendsOff reports whether opts asks for reasoning off.
func sendsOff(opts *llm.ChatOpts) bool {
	return opts != nil && opts.Effort == llm.EffortNone
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

// noteOffRefused tells the person, once per session, that /effort none is
// not what their model gets. Only when they chose none: a fold's is dun's,
// and agentkit's repair round's is agentkit's. Called when the main role's
// clamp moves none (shaping.go), and by the tee after a round that asked for
// none on a model the record has since learned is thinking-only.
func (h *Harness) noteOffRefused() {
	if h.Effort() != EffortOff || h.cfg.OnNotify == nil || h.offNoted.Swap(true) {
		return
	}
	h.cfg.OnNotify("effort none: " + h.cfg.Endpoint.Model + " cannot turn reasoning off; it is sent at the lowest effort it takes")
}
