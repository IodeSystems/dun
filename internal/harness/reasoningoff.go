package harness

import (
	"context"
	"log"
	"regexp"
	"strings"
	"time"

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
// agentkit sends it for its reasoning-cut repair round. Before this, the
// fold failed on every retry and the session could not continue.
//
// offGuard sends such a request once more at the lowest effort instead of
// off. agentkit learns reasoning.thinking_only from the refusal (its
// thinkingOnlyRe matches this body) and dun stores the record
// (capstore.go), so a later off request goes out at the lowest effort from
// the start. agentkit's own translation does not do that for the OpenRouter
// dialect: it rewrites reasoning_effort, not the reasoning object.

// reasoningRequiredRe is the refusal: the body of a 400 that says reasoning
// cannot be turned off. The first wording is verbatim from OpenRouter
// (above); "cannot be disabled" and "thinking-only" are the wordings
// agentkit's thinkingOnlyRe names (Fireworks GLM-5.3: "is a thinking-only
// model; disabling thinking ... is not supported"); "is required" is the
// same refusal as other providers word it.
var reasoningRequiredRe = regexp.MustCompile(`(?i)reasoning is mandatory|(?:reasoning|thinking)[\w\s]{0,40}?(?:cannot|can ?not|can't) be (?:disabled|turned off|switched off)|(?:reasoning|thinking) (?:is|must be) (?:required|enabled|always on)|thinking[- ]only`)

// reasoningRequired reports whether err is that refusal: a 400 whose body
// says so. Any other error, 400s included, is not.
func reasoningRequired(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "status 400") && reasoningRequiredRe.MatchString(s)
}

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

// offFallbackEffort is what an off request sends to a model that refuses
// off: the lowest effort on the OpenAI ladder. OpenRouter takes it (an
// effort a model lacks maps to its nearest one); a llama.cpp endpoint is
// sent no reasoning field at all.
const offFallbackEffort = "minimal"

// lowestOn is opts with off replaced by the lowest effort.
func lowestOn(opts *llm.ChatOpts) *llm.ChatOpts {
	o := *opts
	o.EnableThinking, o.ReasoningBudgetTokens, o.ReasoningBudgetMessage = nil, nil, ""
	o.ReasoningEffort = offFallbackEffort
	return &o
}

// capsOf is the capability record r's client keeps: r itself, or a tier
// side call's tier client. nil when r has none (a fake).
func capsOf(r agent.LLMRunner) interface{ Capabilities() llm.Capabilities } {
	if t, ok := r.(tierRetry); ok {
		r = t.tier
	}
	c, _ := r.(interface{ Capabilities() llm.Capabilities })
	return c
}

// thinkingOnly reports whether r's record says off is refused.
func thinkingOnly(r agent.LLMRunner) bool {
	c := capsOf(r)
	if c == nil {
		return false
	}
	only, ok := c.Capabilities().ThinkingOnly()
	return ok && only
}

// rememberThinkingOnly records the refusal on r's client when agentkit did
// not (a wording only reasoningRequiredRe knows), and stores it.
func rememberThinkingOnly(r agent.LLMRunner, err error) {
	if t, ok := r.(tierRetry); ok {
		r = t.tier
	}
	c, ok := r.(*llm.Client)
	if !ok || thinkingOnly(c) {
		return
	}
	caps := c.Capabilities()
	caps.Facts[llm.CapReasoningThinkingOnly] = llm.Fact{Value: "true", Source: llm.SourceRefused,
		Evidence: "sent thinking off → " + oneLine(err.Error(), 120), At: time.Now()}
	c.SetCapabilities(caps)
	if serr := storeSnapshot(c); serr != nil {
		log.Printf("dun: capabilities: not stored: %v", serr)
	}
}

// offGuard is a runner that does not let a refused off fail the call.
type offGuard struct {
	agent.LLMRunner
	// refused, when set, is told each time off was not sent as asked.
	refused func()
}

func (g offGuard) ChatStream(ctx context.Context, msgs []llm.Message, tools []llm.ToolDef, opts *llm.ChatOpts) (<-chan llm.StreamChunk, error) {
	if !sendsOff(opts) {
		return g.LLMRunner.ChatStream(ctx, msgs, tools, opts)
	}
	if thinkingOnly(g.LLMRunner) {
		g.tell()
		return g.LLMRunner.ChatStream(ctx, msgs, tools, lowestOn(opts))
	}
	ch, err := g.LLMRunner.ChatStream(ctx, msgs, tools, opts)
	if !reasoningRequired(err) || ctx.Err() != nil {
		return ch, err
	}
	log.Printf("dun: reasoning off refused (%s); once more at effort %s", oneLine(err.Error(), 200), offFallbackEffort)
	rememberThinkingOnly(g.LLMRunner, err)
	g.tell()
	return g.LLMRunner.ChatStream(ctx, msgs, tools, lowestOn(opts))
}

func (g offGuard) tell() {
	if g.refused != nil {
		g.refused()
	}
}

// noteOffRefused tells the person, once per session, that /effort off is
// not what their model gets. Only when they chose off: a fold's off is
// dun's, and agentkit's repair round's is agentkit's.
func (h *Harness) noteOffRefused() {
	if h.Effort() != EffortOff || h.cfg.OnNotify == nil || h.offNoted.Swap(true) {
		return
	}
	h.cfg.OnNotify("effort off: " + h.cfg.Endpoint.Model + " cannot turn reasoning off; it is sent at the lowest effort (" + offFallbackEffort + ")")
}
