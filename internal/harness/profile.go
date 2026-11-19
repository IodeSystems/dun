package harness

import (
	"context"
	"strings"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
)

// Model profiles (USER 2026-11-18, docs/decisions.md "Roles and model
// profiles"): what dun knows of one provider+model, in one place, for the
// role resolver (roles.go) to decide with. Nothing here asks the network on
// its own but a tier's window, once; each field is read from what dun holds:
//
//   - the model's client: agentkit's Client.Effective and EffortSupport,
//     which clamp a level by the dialect and the capability record
//     (capstore.go; learned from traffic, refusals and the model list);
//   - the record's reasoning.thinking_only and limits.context;
//   - the model list's thinking fields (listedThinking in effort.go), for
//     the session's own model: thinking_modes with no "off" is recorded as
//     thinking_only (LearnServerThinking);
//   - the window the harness learned (windowTokens), for the session's model;
//   - tools.supported, a fact no source writes yet (agentkit learns none);
//     when one does, a tier on a model with no tool support stops taking
//     spawns.

// CapToolsSupported is the record's fact that the model takes tool calls at
// all: "true" or "false". dun's key, not agentkit's: nothing learns it yet.
const CapToolsSupported = "tools.supported"

// effortClient is what a profile asks of a model's client: agentkit's clamp.
type effortClient interface {
	Capabilities() llm.Capabilities
	Effective(llm.Effort) llm.EffortChoice
	EffortSupport() []llm.EffortChoice
}

// ModelProfile is one provider+model as far as dun knows it. A zero field is
// unknown, not "none".
type ModelProfile struct {
	Provider, Model string
	// ThinkingOnly: the model cannot turn thinking off. Why says which fact.
	ThinkingOnly    bool
	ThinkingOnlyWhy string
	// Window is the whole context in tokens; 0 unknown.
	Window int
	// Tools is "true" or "false" when known, "" unknown.
	Tools string
	// client is the model's client, for the effort clamp; nil when dun has
	// none that clamps (a fake): every level is as asked.
	client effortClient
}

// NoTools says the profile knows the model takes no tool calls.
func (p ModelProfile) NoTools() bool { return p.Tools == "false" }

// effective is what the model's client sends for e.
func (p ModelProfile) effective(e llm.Effort) llm.EffortChoice {
	if p.client == nil {
		return llm.EffortChoice{Wanted: e, Effective: e, Native: true}
	}
	return p.client.Effective(e)
}

// Support is one choice per level (llm.Efforts), lowest first: a level whose
// Effective is not its Wanted is unavailable, and Reason says why.
func (p ModelProfile) Support() []llm.EffortChoice {
	if p.client == nil {
		out := make([]llm.EffortChoice, len(llm.Efforts))
		for i, e := range llm.Efforts {
			out[i] = p.effective(e)
		}
		return out
	}
	return p.client.EffortSupport()
}

// ProfileOf is provider+model's profile from its client r, with window the
// window learned elsewhere (0: the record's limits.context).
func ProfileOf(provider, model string, r agent.LLMRunner, window int) ModelProfile {
	p := ModelProfile{Provider: provider, Model: model, Window: window}
	caps := capsOf(r)
	if c, ok := r.(effortClient); ok {
		p.client = c
	}
	if only, ok := caps.ThinkingOnly(); ok && only {
		p.ThinkingOnly, p.ThinkingOnlyWhy = true, "reasoning.thinking_only="+caps.Facts[llm.CapReasoningThinkingOnly].Value
		if ev := caps.Facts[llm.CapReasoningThinkingOnly].Evidence; ev != "" {
			p.ThinkingOnlyWhy += " (" + ev + ")"
		}
	}
	if p.Window == 0 {
		p.Window, _ = caps.ContextWindow()
	}
	if f, ok := caps.Facts[CapToolsSupported]; ok {
		p.Tools = strings.TrimSpace(f.Value)
	}
	return p
}

// capsOf is r's live capability record; empty for a runner that keeps none
// (a fake, a tee: untee first).
func capsOf(r agent.LLMRunner) llm.Capabilities {
	if c, ok := r.(interface{ Capabilities() llm.Capabilities }); ok {
		return c.Capabilities()
	}
	return llm.Capabilities{}
}

// sessionProfile is this harness's own model: its client and the window it
// learned.
func (h *Harness) sessionProfile() ModelProfile {
	if h == nil {
		return ModelProfile{}
	}
	return ProfileOf(h.cfg.Provider, h.cfg.Endpoint.Model, untee(h.client), h.windowTokens())
}

// runnerProfile is a model another client talks to (a tier's, a child's):
// its record, and its window from the record or, once, from its model list
// (ContextWindow; a short GET, cached for this harness's life).
func (h *Harness) runnerProfile(ctx context.Context, provider, model string, r agent.LLMRunner) ModelProfile {
	p := ProfileOf(provider, model, r, 0)
	if p.Window > 0 || h == nil {
		return p
	}
	key := provider + "\x00" + model
	if v, ok := h.windows.Load(key); ok {
		p.Window = v.(int)
		return p
	}
	if w, ok := r.(contextWindower); ok {
		cctx, cancel := context.WithTimeout(ctx, contextWindowTimeout)
		n, _ := w.ContextWindow(cctx)
		cancel()
		p.Window = n
	}
	h.windows.Store(key, p.Window)
	return p
}

// runProfile is the profile of the model a child runs on: the session's when
// it runs on the parent's own client.
func (h *Harness) runProfile(ctx context.Context, run childRun) ModelProfile {
	if run.model == "" || run.client == h.cfg.Client {
		return h.sessionProfile()
	}
	return h.runnerProfile(ctx, run.provider, run.model, run.client)
}

// declareThinkingOnly records, on the session's client, that its model list
// lists no off among the thinking modes: agentkit then clamps none to the
// lowest level the model takes, before the first refusal. A record that
// already says either way is left alone.
func (h *Harness) declareThinkingOnly(t ServerThinking) {
	if len(t.Modes) == 0 {
		return
	}
	for _, m := range t.Modes {
		if m == "off" {
			return
		}
	}
	c, ok := untee(h.client).(interface {
		Capabilities() llm.Capabilities
		SetCapabilities(llm.Capabilities)
	})
	if !ok {
		return
	}
	caps := c.Capabilities()
	if _, known := caps.ThinkingOnly(); known {
		return
	}
	if caps.Facts == nil {
		caps.Facts = map[string]llm.Fact{}
	}
	caps.Facts[llm.CapReasoningThinkingOnly] = llm.Fact{Value: "true", Source: llm.SourceDeclared,
		Evidence: "the model list's thinking_modes " + strings.Join(t.Modes, ",") + " has no off"}
	c.SetCapabilities(caps)
}
