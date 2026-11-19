package harness

import (
	"context"
	"fmt"
	"strings"

	"github.com/iodesystems/agentkit/llm"
)

// Roles (USER 2026-11-18, docs/decisions.md "Roles and model profiles"):
// every request dun makes is for a role, and the role decides its provider,
// model and effort here, in one place. The roles are main (the session's
// rounds), child:fast, child:work and child:think (sub-agents), and one per
// side-call kind (SideCallKinds).
//
// One precedence for every role, field by field:
//
//	the call's own argument (the agent call's model and effort)
//	> a CLI flag or env var (--tier-<t>, DUN_TIER_<T>, --child-model, --child-effort)
//	> the repo's dun.json (only what the tiers rule allows: use, default_tier)
//	> $DUN_HOME (config.json's tiers, facets and side_calls; /effort's effort.json)
//	> the role's built-in default (roleDefaults)
//
// The wanted effort is then clamped to the model's profile (profile.go,
// effortclamp.go). cmd/dun resolves the layers below the call into Config
// (Tiers, Child*, Effort, the Source fields); this file picks among them.

// RoleMain is the session's own rounds.
const RoleMain = "main"

// ChildRole is the role of a sub-agent on tier t: "child:fast".
func ChildRole(t string) string { return "child:" + t }

// SideCallKinds are the side calls, by their noteSideCall kind: the same
// list as dunconfig.SideCallKinds (a test holds them equal; this package
// does not read the config).
var SideCallKinds = []string{
	"suggest", "fold", "remember", "rescue", "plan_summary", "journal",
	"worklog", "commit", "rephrase", "rules", "learn", "repaint",
}

// windowSideCalls send input sized from the session's window (the session's
// history, or up to half the window for rescue): dunconfig.WindowSideCalls.
// One runs on a tier only when the tier's model's window holds the input.
var windowSideCalls = map[string]bool{"suggest": true, "fold": true, "remember": true, "rescue": true}

// RoleNames are every role, in the order the table shows them.
func RoleNames() []string {
	out := []string{RoleMain}
	for _, t := range TierNames {
		out = append(out, ChildRole(t))
	}
	return append(out, SideCallKinds...)
}

// roleDefaults are each role's built-in effort: an intent, not a level.
// "" is unset: main sends nothing (the server decides), a child inherits the
// saved /effort level, a side call sends what it builds (nothing). fold and
// rescue think as little as the model allows (a summarizer that thinks
// spends its budget before it writes). A tier's default applies only when
// the tier picks the model: a tier with no model is the child default, its
// effort included.
var roleDefaults = map[string]string{
	ChildRole(TierFast):  EffortLowest,
	ChildRole(TierThink): EffortHigh,
	"fold":               EffortLowest,
	"rescue":             EffortLowest,
}

// Source labels a RoleChoice gives that Config's Source fields do not.
const (
	srcCall    = "call"
	srcDefault = "default"
	srcSession = "session"
	srcInherit = "inherits /effort"
	srcEffort  = "/effort"
)

// RoleArg is what the call itself names; empty fields name nothing.
type RoleArg struct {
	Tier, Provider, Model, Effort string
}

// RoleChoice is one role resolved: what it runs on, its effort, and where
// each came from.
type RoleChoice struct {
	Role string
	// Provider "" is the session's; Model "" is the session's model.
	Provider, Model string
	ModelSource     string
	// Tier is the tier that picked Model ("" none did).
	Tier         string
	Effort       EffortChoice
	EffortSource string
	// Note is why the role is not on the model its layers name (a tier
	// that cannot be served).
	Note string
}

// ModelName is the model in words.
func (c RoleChoice) ModelName(session string) string {
	m := c.Model
	if m == "" {
		m = session
	}
	if c.Provider != "" {
		m += " on " + c.Provider
	}
	return m
}

// RoleResolver resolves roles over a Config. Profile is the model profile
// of provider+model ("" model: the session's); Saved is the /effort level a
// child with no effort inherits; MainEffort and MainSource are the
// session's effort now and where it came from.
type RoleResolver struct {
	Cfg                    Config
	MainEffort, MainSource string
	Saved                  string
	Profile                func(provider, model string) ModelProfile
}

// resolver is h's: its config, its effort now, and its profiles.
func (h *Harness) resolver(ctx context.Context) RoleResolver {
	src, _ := h.effortSrc.Load().(string)
	if src == "" {
		src = srcDefault
	}
	return RoleResolver{Cfg: h.cfg, MainEffort: h.Effort(), MainSource: src, Saved: LoadEffort(),
		Profile: func(provider, model string) ModelProfile { return h.profileOf(ctx, provider, model) }}
}

// profileOf is provider+model's profile: the session's own, or the record
// of the client Config.ClientFor builds for it (built, not called).
func (h *Harness) profileOf(ctx context.Context, provider, model string) ModelProfile {
	if model == "" || (model == h.cfg.Endpoint.Model && (provider == "" || provider == h.cfg.Provider)) {
		return h.sessionProfile()
	}
	if h.cfg.ClientFor == nil {
		return ModelProfile{Provider: provider, Model: model}
	}
	c, err := h.cfg.ClientFor(provider, model)
	if err != nil || c.Runner == nil {
		return ModelProfile{Provider: provider, Model: model}
	}
	// No model-list GET here: the table is built on the settings path. A
	// window is asked only by a side call that needs it (tierRetry).
	p := ProfileOf(provider, model, c.Runner, 0)
	if v, ok := h.windows.Load(provider + "\x00" + model); ok && p.Window == 0 {
		p.Window = v.(int)
	}
	return p
}

// sessionOpts writes role's effort on opts, clamped to the session's own
// model: the main role's every round (shaping.go), and fold's and rescue's
// options (foldOpts), which a tier's runner clamps again to its model
// (tierRetry). An unset effort leaves opts as built. It returns the choice.
func (h *Harness) sessionOpts(opts *llm.ChatOpts, role string) EffortChoice {
	rr := h.resolver(context.Background())
	var wanted string
	if role == RoleMain {
		wanted = rr.main(RoleArg{}).Effort.Wanted
	} else {
		wanted = rr.side(role, RoleArg{}).Effort.Wanted
	}
	if wanted == "" {
		return EffortChoice{}
	}
	c := efforts.Clamp(h.sessionProfile(), wanted)
	efforts.Apply(opts, c.Effective)
	if role == RoleMain && toLLM(wanted) == llm.EffortNone && c.Effective != EffortOff {
		h.noteOffRefused()
	}
	return c
}

// Role is role resolved for this harness now.
func (h *Harness) Role(role string, arg RoleArg) RoleChoice {
	return h.resolver(context.Background()).Resolve(role, arg)
}

// Roles is every role, resolved: the table /model, dun doctor and the
// settings event show.
func (h *Harness) Roles() []RoleChoice { return h.resolver(context.Background()).All() }

// All is every role, in RoleNames order.
func (r RoleResolver) All() []RoleChoice {
	names := RoleNames()
	out := make([]RoleChoice, 0, len(names))
	for _, n := range names {
		out = append(out, r.Resolve(n, RoleArg{}))
	}
	return out
}

// Resolve is role with the call's arg, clamped to its model's profile.
func (r RoleResolver) Resolve(role string, arg RoleArg) RoleChoice {
	var c RoleChoice
	switch {
	case role == RoleMain:
		c = r.main(arg)
	case strings.HasPrefix(role, "child:"):
		t := strings.TrimPrefix(role, "child:")
		if t == "" {
			t = orStr(r.Cfg.DefaultTier, TierWork)
			role = ChildRole(t)
		}
		c = r.child(t, arg, false)
	default:
		c = r.side(role, arg)
	}
	c.Role = role
	r.clamp(&c)
	return c
}

// ChildDefault is role child:t on the child default, not on its tier: what
// a spawn runs on when its tier cannot (unavailable, no tool support).
func (r RoleResolver) ChildDefault(t string, arg RoleArg) RoleChoice {
	c := r.child(t, arg, true)
	c.Role = ChildRole(t)
	r.clamp(&c)
	return c
}

// main is the session's rounds: the session's model; the call's effort,
// else /effort's (or facets.main's), else default.
func (r RoleResolver) main(arg RoleArg) RoleChoice {
	c := RoleChoice{ModelSource: orStr(r.Cfg.MainSource, srcSession)}
	switch {
	case arg.Effort != "":
		c.Effort.Wanted, c.EffortSource = arg.Effort, srcCall
	case r.MainEffort != "":
		c.Effort.Wanted, c.EffortSource = r.MainEffort, orStr(r.MainSource, srcDefault)
	default:
		c.Effort.Wanted, c.EffortSource = EffortDefault, srcDefault
	}
	return c
}

// child is a sub-agent on tier t ("" the default tier). A model in the call
// overrides the tier, its effort included; a tier with no model, or one that
// cannot be served, is the child default (facets.child, --child-model, else
// the session's model).
//
// noTier is the child default whatever the tier names: the spawn's
// fallback when the tier cannot run it (ChildDefault).
func (r RoleResolver) child(t string, arg RoleArg, noTier bool) RoleChoice {
	var c RoleChoice
	var tier Tier
	onTier := false
	switch {
	case arg.Provider != "" || arg.Model != "":
		c.Provider, c.Model, c.ModelSource = arg.Provider, arg.Model, srcCall
	default:
		for _, x := range r.Cfg.Tiers {
			if x.Name == t && !noTier {
				tier = x
			}
		}
		switch {
		case tier.on() && tier.Off != "":
			c.Note = "tier " + t + " unavailable: " + tier.Off
		case tier.on():
			onTier = true
			c.Tier, c.Provider, c.Model = t, tier.Provider, tier.Model
			c.ModelSource = "tier " + t + " · " + orStr(tier.Source, "config")
		}
		if !onTier {
			c.Provider, c.Model = r.Cfg.ChildProvider, r.Cfg.ChildModel
			c.ModelSource = srcSession
			if c.Model != "" {
				c.ModelSource = "facets.child · " + orStr(r.Cfg.ChildSource, "config")
			}
		}
	}
	flagEffort := r.Cfg.ChildEffort != "" && r.Cfg.ChildEffortSource == "--child-effort"
	switch {
	case arg.Effort != "":
		c.Effort.Wanted, c.EffortSource = arg.Effort, srcCall
	case flagEffort:
		c.Effort.Wanted, c.EffortSource = r.Cfg.ChildEffort, r.Cfg.ChildEffortSource
	case onTier && tier.Effort != "":
		c.Effort.Wanted, c.EffortSource = tier.Effort, "tiers."+t+".effort"
	case r.Cfg.ChildEffort != "":
		c.Effort.Wanted, c.EffortSource = r.Cfg.ChildEffort, orStr(r.Cfg.ChildEffortSource, "facets.child")
	case onTier && roleDefaults[ChildRole(t)] != "":
		c.Effort.Wanted, c.EffortSource = roleDefaults[ChildRole(t)], tierDefaultSource(t)
	default:
		c.EffortSource = srcInherit
	}
	return c
}

// tierDefaultSource is the source label of tier t's built-in effort:
// "tier default (fast→lowest)".
func tierDefaultSource(t string) string {
	return "tier default (" + t + "→" + roleDefaults[ChildRole(t)] + ")"
}

// TierDefaultNotes are the start notes for tiers whose spawns now take the
// tier's built-in effort: a configured tier with a model and no effort, which
// before roles inherited the saved /effort level. One per tier, said once
// at engine start.
func (r RoleResolver) TierDefaultNotes() []string {
	var out []string
	for _, t := range TierNames {
		c := r.child(t, RoleArg{}, false)
		if c.EffortSource == tierDefaultSource(t) {
			out = append(out, fmt.Sprintf("tier %s (%s) has no effort set: its spawns run at %s, the tier's default (before, the saved /effort level); set tiers.%s.effort to choose", t, c.Model, c.Effort.Wanted, t))
		}
	}
	return out
}

// side is side call kind: its side_calls tier's model, else the session's;
// its built-in effort (side calls take a tier's model, not its effort).
func (r RoleResolver) side(kind string, arg RoleArg) RoleChoice {
	name := r.Cfg.SideCallTiers[kind]
	src := orStr(r.Cfg.SideCallSources[kind], srcDefault)
	c := RoleChoice{ModelSource: srcSession}
	if name != "" && name != TierSession {
		for _, t := range r.Cfg.Tiers {
			if t.Name != name || !t.on() {
				continue
			}
			if t.Off != "" {
				c.Note = "tier " + name + " unavailable: " + t.Off + "; on the session's model"
				break
			}
			c.Tier, c.Provider, c.Model = name, t.Provider, t.Model
			c.ModelSource = "side_calls " + name + " · " + src
		}
	}
	switch {
	case arg.Effort != "":
		c.Effort.Wanted, c.EffortSource = arg.Effort, srcCall
	case roleDefaults[kind] != "":
		c.Effort.Wanted, c.EffortSource = roleDefaults[kind], srcDefault
	}
	return c
}

// clamp sets c's effective effort on its model's profile. Unset: a child
// shows the /effort level it inherits, clamped; a side call sends nothing.
func (r RoleResolver) clamp(c *RoleChoice) {
	var p ModelProfile
	if r.Profile != nil {
		p = r.Profile(c.Provider, c.Model)
	}
	switch {
	case c.Effort.Wanted != "":
		c.Effort = efforts.Clamp(p, c.Effort.Wanted)
	case strings.HasPrefix(c.Role, "child:"):
		in := efforts.Clamp(p, orStr(r.Saved, EffortDefault))
		c.Effort = EffortChoice{Effective: in.Effective, Reason: in.Reason}
	default:
		c.Effort = EffortChoice{Effective: EffortDefault, Reason: "nothing sent"}
	}
}

// Line is the role as one row of the table: role, model, wanted, effective,
// reason, source.
func (c RoleChoice) Line(session string) string {
	wanted := c.Effort.Wanted
	if wanted == "" {
		wanted = "unset"
	}
	return fmt.Sprintf("%-12s %-24s %-7s %-7s %s · %s / %s", c.Role, c.ModelName(session), wanted,
		c.Effort.Effective, orStr(c.Why(), "-"), c.ModelSource, orStr(c.EffortSource, "-"))
}

// RoleHeader is the table's header, aligned with Line.
func RoleHeader() string {
	return fmt.Sprintf("%-12s %-24s %-7s %-7s %s", "role", "model", "wanted", "sent", "reason · source of model / effort")
}

// Why is the reason column: why the effort sent is not the one wanted, and
// why the model is not the one the role's layers name.
func (c RoleChoice) Why() string {
	if c.Note == "" {
		return c.Effort.Reason
	}
	return strings.TrimPrefix(c.Effort.Reason+"; "+c.Note, "; ")
}

func orStr(s, or string) string {
	if s == "" {
		return or
	}
	return s
}
