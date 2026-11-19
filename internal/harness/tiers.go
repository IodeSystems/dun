package harness

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
)

// Model tiers (USER 2026-11-18, dunconfig/tiers.go): a spawn names the kind
// of work (fast, work, think) and the tier names the model; an explicit
// model overrides it. Each automatic side call has a tier too. cmd/dun
// resolves the tiers from the config and hands them here as Config.Tiers;
// this package does not read the config.

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

// tier is the named tier as configured; false when no tier has that name.
func (h *Harness) tier(name string) (Tier, bool) {
	for _, t := range h.cfg.Tiers {
		if t.Name == name {
			return t, true
		}
	}
	return Tier{}, false
}

// defaultTier is the tier a spawn that names none runs on.
func (h *Harness) defaultTier() string {
	if h.cfg.DefaultTier != "" {
		return h.cfg.DefaultTier
	}
	return TierWork
}

// spawnTier is what a spawn runs on: the provider, model and effort it
// gives, or its tier's. A model given overrides the tier (and the tier's
// effort with it); a tier with no model is the child default, as before
// tiers. The tier returned is the one the child is shown with, "" when no
// tier picked its model.
func (h *Harness) spawnTier(tierArg, provider, model, effort string) (tier, p, m, e string, err error) {
	tierArg = strings.ToLower(strings.TrimSpace(tierArg))
	if tierArg != "" && !isTierName(tierArg) {
		return "", "", "", "", fmt.Errorf("tier %q: want %s", tierArg, strings.Join(TierNames, ", "))
	}
	if provider != "" || model != "" {
		return "", provider, model, effort, nil
	}
	name := tierArg
	if name == "" {
		name = h.defaultTier()
	}
	if t, ok := h.tier(name); ok && t.on() {
		if effort == "" {
			effort = t.Effort
		}
		return name, t.Provider, t.Model, effort, nil
	}
	return "", h.cfg.ChildProvider, h.cfg.ChildModel, effort, nil
}

func isTierName(s string) bool {
	for _, n := range TierNames {
		if n == s {
			return true
		}
	}
	return false
}

// sideRunner is the runner side call kind runs on: its tier's model
// (Config.SideCallTiers), else fallback, the runner it used before tiers.
// A tier with no model, TierSession, or a client that cannot be built is
// fallback: a side call is best-effort and never fails over its tier.
func (h *Harness) sideRunner(kind string, fallback agent.LLMRunner) agent.LLMRunner {
	name := h.cfg.SideCallTiers[kind]
	if name == "" || name == TierSession || h.cfg.ClientFor == nil {
		return fallback
	}
	t, ok := h.tier(name)
	if !ok || !t.on() {
		return fallback
	}
	if t.Off != "" {
		log.Printf("dun: side call %s: tier %s unavailable: %s; on the session's model", kind, name, t.Off)
		return fallback
	}
	c, err := h.cfg.ClientFor(t.Provider, t.Model)
	if err != nil || c.Runner == nil {
		log.Printf("dun: side call %s: tier %s (%s): %v; on the session's model", kind, name, t.Model, err)
		return fallback
	}
	return tierRetry{tier: c.Runner, fallback: fallback, kind: kind, name: name, model: t.Model}
}

// tierRetry is a side call on a tier model that is asked once more on the
// session's runner when the tier refuses it: an error before any reply (a
// context too long for the smaller model, a bad key, a model not found).
// A failure after the reply has started is the call's, as before.
type tierRetry struct {
	tier, fallback    agent.LLMRunner
	kind, name, model string
}

func (r tierRetry) ChatStream(ctx context.Context, msgs []llm.Message, tools []llm.ToolDef, opts *llm.ChatOpts) (<-chan llm.StreamChunk, error) {
	ch, err := r.tier.ChatStream(ctx, msgs, tools, opts)
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
