package harness

import (
	"context"
	"encoding/json"
	"log"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
)

// Model tiers (USER 2026-11-18, dunconfig/tiers.go): a spawn names the kind
// of work (fast, work, think) and the tier names the model; an explicit
// model overrides it. Each automatic side call has a tier too. cmd/dun
// resolves the tiers from the config and hands them here as Config.Tiers;
// this package does not read the config. Which tier, model and effort a
// request gets is the role resolver's (roles.go).

// The tier names, as dunconfig has them.
const (
	TierFast    = "fast"
	TierWork    = "work"
	TierThink   = "think"
	TierSession = "session" // a side call on the session's own model
)

// TierNames are the tiers in order: the agent tool's tier enum.
var TierNames = []string{TierFast, TierWork, TierThink}

// Tier is one resolved tier. No Model: the child default.
type Tier struct {
	Name, Provider, Model, Effort string
	// Use is what the tier is for, as the agent tool describes it.
	Use string
	// Off is why the tier cannot be served (its provider is not logged
	// in); a spawn on it runs on the child default, and says so.
	Off string
	// Source is the layer Model came from (flag, env, config).
	Source string
}

// on says the tier names a model.
func (t Tier) on() bool { return t.Model != "" }

// tiered says some tier names a model: only then does the agent tool offer
// the tier param, so a config with no tiers sends the tool as before.
func tiered(tiers []Tier) bool {
	for _, t := range tiers {
		if t.on() {
			return true
		}
	}
	return false
}

func isTierName(s string) bool {
	for _, n := range TierNames {
		if n == s {
			return true
		}
	}
	return false
}

// sideRunner is the runner side call kind runs on: its role's model
// (roles.go: its side_calls tier's), else fallback, the runner it used
// before tiers. A tier with no model, TierSession, or a client that cannot
// be built is fallback: a side call is best-effort and never fails over its
// tier.
func (h *Harness) sideRunner(kind string, fallback agent.LLMRunner) agent.LLMRunner {
	// The guard (sideguard.go) wraps the trace, so a capped or refused call is
	// still traced; the tier underneath is sideRunnerRaw.
	return sideGuard{inner: h.traced(kind, h.sideRunnerRaw(kind, fallback)), kind: kind, h: h}
}

// sideRunnerRaw is sideRunner without the guard (sideguard.go) or the trace
// (trace.go): the tier's runner, or fallback.
func (h *Harness) sideRunnerRaw(kind string, fallback agent.LLMRunner) agent.LLMRunner {
	if h.cfg.ClientFor == nil {
		return fallback
	}
	c := h.resolver(context.Background()).side(kind, RoleArg{})
	if c.Note != "" {
		log.Printf("dun: side call %s: %s", kind, c.Note)
		return fallback
	}
	if c.Tier == "" {
		return fallback
	}
	cl, err := h.cfg.ClientFor(c.Provider, c.Model)
	if err != nil || cl.Runner == nil {
		log.Printf("dun: side call %s: tier %s (%s): %v; on the session's model", kind, c.Tier, c.Model, err)
		return fallback
	}
	return tierRetry{tier: cl.Runner, fallback: fallback, kind: kind, name: c.Tier, provider: c.Provider, model: c.Model,
		effort: c.Effort.Wanted, window: windowSideCalls[kind], h: h}
}

// tierRetry is a side call on a tier model that is asked once more on the
// session's runner when the tier refuses it: an error before any reply (a
// context too long for the smaller model, a bad key, a model not found).
// A failure after the reply has started is the call's, as before.
//
// A window-sized side call (windowSideCalls) goes to the tier only when the
// tier model's window, as its profile knows it, holds the input and the
// reply's cap; otherwise, or when the window is unknown, it runs on the
// session's model, logged. The role's effort (fold's lowest) is clamped to
// the tier model, not the session's: opts as the caller built them were
// clamped to the session's, and the retry sends them so.
type tierRetry struct {
	tier, fallback              agent.LLMRunner
	kind, name, provider, model string
	effort                      string // the role's wanted effort; "" leaves opts as built
	window                      bool
	h                           *Harness
}

// inputTokens estimates what a request sends and may get back: the
// messages, the tool schemas and the reply's cap.
func inputTokens(msgs []llm.Message, tools []llm.ToolDef, opts *llm.ChatOpts) int {
	n := 0
	for _, m := range msgs {
		n += estimateTokens(m.Content)
	}
	if len(tools) > 0 {
		b, _ := json.Marshal(tools)
		n += estimateTokens(string(b))
	}
	if opts != nil {
		n += opts.MaxTokens
	}
	return n
}

func (r tierRetry) ChatStream(ctx context.Context, msgs []llm.Message, tools []llm.ToolDef, opts *llm.ChatOpts) (<-chan llm.StreamChunk, error) {
	p := r.h.runnerProfile(ctx, r.provider, r.model, r.tier)
	if r.window {
		need := inputTokens(msgs, tools, opts)
		switch {
		case p.Window == 0:
			log.Printf("dun: side call %s: tier %s (%s) has no known window for ~%d tokens; on the session's model", r.kind, r.name, r.model, need)
			return r.fallback.ChatStream(ctx, msgs, tools, opts)
		case need > p.Window:
			log.Printf("dun: side call %s: ~%d tokens do not fit tier %s's window (%s, %d); on the session's model", r.kind, need, r.name, r.model, p.Window)
			return r.fallback.ChatStream(ctx, msgs, tools, opts)
		}
	}
	sent := opts
	if r.effort != "" {
		o := llm.ChatOpts{}
		if opts != nil {
			o = *opts
		}
		efforts.Apply(&o, efforts.Clamp(p, r.effort).Effective)
		sent = &o
	}
	ch, err := r.tier.ChatStream(ctx, msgs, tools, sent)
	if err != nil {
		return r.retry(ctx, err.Error(), msgs, tools, opts)
	}
	first, ok := <-ch
	if !ok {
		return r.retry(ctx, "no reply", msgs, tools, opts)
	}
	if first.Error != "" && first.Content == "" && first.ToolCall == nil {
		go func() {
			for range ch {
			}
		}()
		return r.retry(ctx, first.Error, msgs, tools, opts)
	}
	out := make(chan llm.StreamChunk, cap(ch)+1)
	go func() {
		defer close(out)
		out <- first
		for c := range ch {
			select {
			case out <- c:
			case <-ctx.Done():
				for range ch {
				}
				return
			}
		}
	}()
	return out, nil
}

func (r tierRetry) retry(ctx context.Context, why string, msgs []llm.Message, tools []llm.ToolDef, opts *llm.ChatOpts) (<-chan llm.StreamChunk, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	log.Printf("dun: side call %s on tier %s (%s) failed: %s; once more on the session's model", r.kind, r.name, r.model, oneLine(why, 200))
	return r.fallback.ChatStream(ctx, msgs, tools, opts)
}
