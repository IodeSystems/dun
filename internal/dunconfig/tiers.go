package dunconfig

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/iodesystems/agentkit/llm"
)

// tiers.go: the three model tiers (USER 2026-11-18). A sub-agent is started
// on a tier (the agent tool's tier param) and each automatic side call has
// one (side_calls), so the parent names the kind of work, not a model id.
//
// The models live in $DUN_HOME/config.json only, beside facets:
//
//	"tiers": {"fast": {"provider": "gw", "model": "m", "effort": "low", "use": "…"}, …}
//	"default_tier": "work"
//	"side_calls": {"suggest": "session", "journal": "fast", …}
//
// A repo's dun.json may set a tier's use text and default_tier, nothing else
// (RepoTiers): a committed file does not pick what this machine pays for.
// Precedence, field by field as in facets.go: flag (--tier-<name>) > env
// (DUN_TIER_<NAME>, "[provider/]model") > config. default_tier: flag
// (--default-tier) > env (DUN_DEFAULT_TIER) > the repo's > config > work.
// A tier with no model is unconfigured: its spawns run on the child default
// (facets.child, --child-model, else the session's model), as before tiers.

// The tiers, and the side-call value for the session's own model.
const (
	TierFast    = "fast"
	TierWork    = "work"
	TierThink   = "think"
	TierSession = "session"
)

// TierNames are the tiers in the order they are shown.
var TierNames = []string{TierFast, TierWork, TierThink}

// DefaultTier is the tier a spawn that names none runs on, when neither the
// repo nor the config names another.
const DefaultTier = TierWork

// IsTier says whether name is a tier.
func IsTier(name string) bool {
	for _, t := range TierNames {
		if t == name {
			return true
		}
	}
	return false
}

// Tier is one tier in config.json. Use is the guidance the agent tool shows
// for it; empty is DefaultTierUse.
type Tier struct {
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	Effort   string `json:"effort,omitempty"`
	Use      string `json:"use,omitempty"`
}

// DefaultTierUse is each tier's use text when no file sets one.
var DefaultTierUse = map[string]string{
	TierFast:  "mechanical work (renames, formatting, file operations, summarizing given text; no judgment)",
	TierWork:  "scoped research, code reading, single-area fixes",
	TierThink: "planning, tradeoffs, architecture, review",
}

// RepoTiers is what a repo's dun.json says of tiers (mcpserver.LoadTiers):
// use texts and default_tier. Ignored names each field it set that only
// config.json may set (tiers.fast.model), for doctor to warn about.
type RepoTiers struct {
	Use     map[string]string
	Default string
	Ignored []string
}

// TierFlags are the tier flags given: a "[provider/]model" by tier, and
// --default-tier.
type TierFlags struct {
	Picks   map[string]string
	Default string
}

// TierPick is one tier as it is served.
type TierPick struct {
	Name            string
	Provider, Model string // Provider "" is the session's
	Effort          string // "" leaves the child default's
	Use             string
	Source          string // the layer Model came from; "" when none names one
	// Off is why the tier cannot be served (its provider is not saved).
	Off string
}

// On says the tier names a model: a spawn on it runs there, not on the
// child default.
func (t TierPick) On() bool { return t.Model != "" }

// What is the tier's model in words: "m on gw, effort low", or what an
// unconfigured tier runs on.
func (t TierPick) What() string {
	if !t.On() {
		return "the child default (no model set)"
	}
	w := t.Model
	if t.Provider != "" {
		w += " on " + t.Provider
	}
	if t.Effort != "" {
		w += ", effort " + t.Effort
	}
	if t.Off != "" {
		w += ": off (" + t.Off + ")"
	}
	return w
}

// Line is the tier in one line, as /model and dun doctor show it.
func (t TierPick) Line() string {
	l := fmt.Sprintf("%-6s %s", t.Name, t.What())
	if t.Source != "" {
		l += " · " + t.Source
	}
	return l + " — " + t.Use
}

// TierSet is every tier, resolved, and the default.
type TierSet struct {
	Tiers         []TierPick // TierNames order
	Default       string
	DefaultSource string
	// Warnings are what doctor and /model say about the files: a repo that
	// names a model, an unknown tier, a bad default_tier.
	Warnings []string
}

// Get is tier name; false when name is no tier.
func (s TierSet) Get(name string) (TierPick, bool) {
	for _, t := range s.Tiers {
		if t.Name == name {
			return t, true
		}
	}
	return TierPick{}, false
}

// Any says some tier names a model. With none, the agent tool is as it
// was before tiers.
func (s TierSet) Any() bool {
	for _, t := range s.Tiers {
		if t.On() {
			return true
		}
	}
	return false
}

// tierEnv is the env var of tier name: DUN_TIER_FAST.
func tierEnv(name string) string { return "DUN_TIER_" + strings.ToUpper(name) }

// ResolveTiers is every tier as flags, env, the repo and the config give it.
func (c DunConfig) ResolveTiers(flags TierFlags, env func(string) string, repo RepoTiers) TierSet {
	if env == nil {
		env = func(string) string { return "" }
	}
	var set TierSet
	for _, name := range TierNames {
		f := c.Tiers[name]
		t := TierPick{Name: name, Use: DefaultTierUse[name]}
		if u := strings.TrimSpace(f.Use); u != "" {
			t.Use = u
		}
		if u := strings.TrimSpace(repo.Use[name]); u != "" {
			t.Use = u
		}
		layers := []struct {
			src string
			p   Pick
		}{
			{SourceFlag, c.pickOf(flags.Picks[name])},
			{SourceEnv, c.pickOf(env(tierEnv(name)))},
			{SourceConfig, Pick{Provider: strings.TrimSpace(f.Provider), Model: strings.TrimSpace(f.Model)}},
		}
		for _, l := range layers {
			if t.Model == "" && l.p.Model != "" {
				t.Model, t.Source = l.p.Model, l.src
			}
			if t.Provider == "" && l.p.Provider != "" {
				t.Provider = l.p.Provider
			}
		}
		if t.Model != "" {
			t.Effort = strings.TrimSpace(f.Effort)
			if t.Effort != "" && !ValidEffort(t.Effort) {
				set.Warnings = append(set.Warnings, fmt.Sprintf("tiers.%s.effort %q is not a level (%s); ignored", name, t.Effort, EffortWords))
				t.Effort = ""
			}
			if t.Provider != "" {
				if _, ok := c.Named(t.Provider); !ok {
					t.Off = "no saved provider " + t.Provider
					set.Warnings = append(set.Warnings, "tier "+name+" is unavailable ("+t.Off+"): its spawns run on the child default, its side calls on the session's model")
				}
			}
		} else if f.Provider != "" || f.Effort != "" {
			set.Warnings = append(set.Warnings, "tiers."+name+" sets no model; its provider and effort are ignored")
		}
		set.Tiers = append(set.Tiers, t)
	}
	for _, n := range sortedKeys(c.Tiers) {
		if !IsTier(n) {
			set.Warnings = append(set.Warnings, "tiers."+n+" in "+ConfigPath()+" is not a tier ("+strings.Join(TierNames, ", ")+"); ignored")
		}
	}
	for _, n := range sortedKeys(repo.Use) {
		if !IsTier(n) {
			set.Warnings = append(set.Warnings, "the repo's tiers."+n+" is not a tier ("+strings.Join(TierNames, ", ")+"); ignored")
		}
	}
	for _, f := range repo.Ignored {
		set.Warnings = append(set.Warnings, "the repo's "+f+" is ignored: a repo sets only use and default_tier; models live in "+ConfigPath())
	}
	set.Default, set.DefaultSource = DefaultTier, SourceDefault
	for _, l := range []struct{ src, v, what string }{
		{SourceFlag, flags.Default, "--default-tier"},
		{SourceEnv, env("DUN_DEFAULT_TIER"), "DUN_DEFAULT_TIER"},
		{"repo", repo.Default, "the repo's default_tier"},
		{SourceConfig, c.DefaultTier, "default_tier in " + ConfigPath()},
	} {
		v := strings.TrimSpace(l.v)
		if v == "" {
			continue
		}
		if !IsTier(v) {
			set.Warnings = append(set.Warnings, fmt.Sprintf("%s %q is not a tier (%s); ignored", l.what, v, strings.Join(TierNames, ", ")))
			continue
		}
		set.Default, set.DefaultSource = v, l.src
		break
	}
	return set
}

// EffortWords names the effort values config.json and the flags take.
const EffortWords = "default, none (or off), minimal, low, medium, high, xhigh, max, lowest"

// ValidEffort says whether s is an effort a tier, a facet or a flag may set:
// default, agentkit's levels (llm.ParseEffort), off (the old name of none),
// or lowest (the lowest level the model takes).
func ValidEffort(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "default", "off", "lowest":
		return true
	case "":
		return false
	}
	_, err := llm.ParseEffort(s)
	return err == nil
}

// pickOf reads a flag or env value, "[provider/]model".
func (c DunConfig) pickOf(s string) Pick {
	p, m := c.ParsePick(s)
	return Pick{Provider: p, Model: m}
}

// The side calls: dun's own automatic calls outside the conversation, by
// their noteSideCall kind. vision is not one of them: it has its facet.
var SideCallKinds = []string{
	"suggest", "fold", "remember", "rescue", "plan_summary", "journal",
	"commit", "rephrase", "rules", "learn", "repaint", "until",
}

// WindowSideCalls are the side calls whose input is sized from the
// session's window: suggest, fold and remember send the session's history
// (and its prompt-cache prefix, which hits only on the model that wrote
// it); rescue sends up to half the window to recover a session that is
// over it. A smaller tier model may refuse them, so they default to the
// session's model, and a side_calls entry that puts one on a tier is
// warned about.
var WindowSideCalls = []string{"suggest", "fold", "remember", "rescue"}

// DefaultSideCalls is the tier of each side call no side_calls entry
// names: the WindowSideCalls on the session's model, the ones that send a
// prompt of their own on fast, but learn and until, which judge, and repaint on
// work. A tier with no model is the session's model, as before tiers; a
// tier call that fails is asked once more on the session's model.
var DefaultSideCalls = map[string]string{
	"suggest":      TierSession,
	"fold":         TierSession,
	"remember":     TierSession,
	"rescue":       TierSession,
	"plan_summary": TierFast,
	"journal":      TierFast,
	"commit":       TierFast,
	"rephrase":     TierFast,
	"rules":        TierFast,
	"learn":        TierWork,
	"repaint":      TierWork,
	"until":        TierWork,
}

// IsSideCall says whether kind is a side call.
func IsSideCall(kind string) bool {
	_, ok := DefaultSideCalls[kind]
	return ok
}

// SideCallTiers is the tier (or TierSession) of every side call: side_calls
// over DefaultSideCalls. The warnings name an entry that is ignored.
func (c DunConfig) SideCallTiers() (map[string]string, []string) {
	out := make(map[string]string, len(DefaultSideCalls))
	for k, v := range DefaultSideCalls {
		out[k] = v
	}
	var warn []string
	for _, k := range sortedKeys(c.SideCalls) {
		v := strings.TrimSpace(c.SideCalls[k])
		switch {
		case !IsSideCall(k):
			warn = append(warn, fmt.Sprintf("side_calls.%s is not a side call (%s); ignored", k, strings.Join(SideCallKinds, ", ")))
		case v != TierSession && !IsTier(v):
			warn = append(warn, fmt.Sprintf("side_calls.%s %q is not a tier (%s, or %s); ignored", k, v, strings.Join(TierNames, ", "), TierSession))
		default:
			out[k] = v
			if v != TierSession && slices.Contains(WindowSideCalls, k) {
				warn = append(warn, fmt.Sprintf("side_calls.%s %q: %s sends up to the session's window, and runs on %s only when its model's window holds the input (else on the session's model, logged); it loses the session's prompt cache, and a refusal is asked again on the session's model", k, v, k, v))
			}
		}
	}
	return out, warn
}

// SideCallLine is one side call as doctor shows it: its tier and the model
// that runs on (a side call takes the tier's model, not its effort).
func SideCallLine(kind, tier string, set TierSet) string {
	what := "the session's model"
	if t, ok := set.Get(tier); ok && t.On() {
		what = t.Model
		if t.Provider != "" {
			what += " on " + t.Provider
		}
	} else if tier != TierSession {
		what += " (" + tier + " has no model)"
	}
	return fmt.Sprintf("%-12s %-7s %s", kind, tier, what)
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
