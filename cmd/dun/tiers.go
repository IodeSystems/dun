package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/dun/internal/dunconfig"
	"github.com/iodesystems/dun/internal/harness"
	"github.com/iodesystems/dun/internal/mcpserver"
	"github.com/iodesystems/dun/proto"
)

// The model tiers (dunconfig/tiers.go): resolved once at start, from
// $DUN_HOME/config.json, the workspace's dun.json (use and default_tier
// only), env and the flags, and handed to the harness.

// tierFlags are this engine's --tier-<name> and --default-tier.
var tierFlags dunconfig.TierFlags

// resolveTiers is every tier for workspace ws. A --no-dir session has no
// project, so no repo layer.
func resolveTiers(fc dunconfig.DunConfig, ws string, noDir bool) dunconfig.TierSet {
	var repo dunconfig.RepoTiers
	if !noDir {
		repo = mcpserver.LoadTiers(ws)
	}
	return fc.ResolveTiers(tierFlags, os.Getenv, repo)
}

// startTiers is the engine's tiers and side-call tiers, with every warning
// (a tier unavailable, a name that is no tier, a repo naming a model, a
// window-sized side call on a tier) written to w, one "dun: " line each.
func startTiers(fc dunconfig.DunConfig, ws string, noDir bool, w io.Writer) (dunconfig.TierSet, map[string]string) {
	set := resolveTiers(fc, ws, noDir)
	sides, warn := fc.SideCallTiers()
	for _, l := range append(append([]string(nil), set.Warnings...), warn...) {
		fmt.Fprintln(w, "dun: "+l)
	}
	return set, sides
}

// engineTiers are this engine's tiers, resolved once at start, for the
// settings event: /model shows them rather than re-reading the config.
var engineTiers dunconfig.TierSet

// tiersProto is set for the settings event, then the role table: every
// role as roles resolves it (harness.Harness.Roles; none before the
// harness is up).
func tiersProto(set dunconfig.TierSet, roles []harness.RoleChoice) []proto.TierInfo {
	out := make([]proto.TierInfo, 0, len(set.Tiers)+len(roles))
	for _, t := range set.Tiers {
		out = append(out, proto.TierInfo{Name: t.Name, Provider: t.Provider, Model: t.Model, Effort: t.Effort, Source: t.Source, Off: t.Off})
	}
	for _, r := range roles {
		out = append(out, proto.TierInfo{Name: r.Role, Role: r.Role, Provider: r.Provider, Model: r.Model, Source: r.ModelSource,
			Wanted: r.Effort.Wanted, Effective: r.Effort.Effective, Reason: r.Why(), EffortSource: r.EffortSource})
	}
	return out
}

// effortLayers are the effort layers cmd/dun hands the role resolver.
type effortLayers struct {
	main, child, childSrc string
}

// resolveEfforts reads facets.main's and the child default's effort (flag
// --child-effort, else facets.child), off read as none. A value that is no
// level is said on w: main's is ignored, as before; the child's is kept, so
// a spawn that would take it refuses it (harness spawnAgentFor) rather than
// quietly running on the saved level.
func resolveEfforts(main, child dunconfig.Resolved, childFlag string, w io.Writer) effortLayers {
	var e effortLayers
	if main.Effort != "" {
		if l, ok := harness.NormalizeEffort(main.Effort); ok {
			e.main = l
		} else {
			fmt.Fprintf(w, "dun: facets.main.effort %q is not a level (%s); ignored\n", main.Effort, dunconfig.EffortWords)
		}
	}
	if child.Effort == "" {
		return e
	}
	e.childSrc = "facets.child"
	if childFlag != "" {
		e.childSrc = "--child-effort"
	}
	l, ok := harness.NormalizeEffort(child.Effort)
	if !ok {
		fmt.Fprintf(w, "dun: %s %q is not a level (%s); a spawn that takes it is refused\n", e.childSrc, child.Effort, dunconfig.EffortWords)
		l = strings.TrimSpace(child.Effort)
	}
	e.child = l
	return e
}

// rolesResend is harness.OnCapsChanged's func for the engine: the settings
// event and the command list (the /effort picker's marks) again when the
// role table changed since the last it sent. An effort translation can come
// on every request; the table changes once.
func rolesResend(emit func(any), commands func() any) func() {
	var mu sync.Mutex
	last := ""
	// Built, compared and sent under one lock: two changes in quick
	// succession (each run on its own goroutine) cannot send the older table
	// after the newer one.
	return func() {
		mu.Lock()
		defer mu.Unlock()
		ev := settingsEvent()
		b, _ := json.Marshal(ev.Tiers)
		if string(b) == last {
			return
		}
		last = string(b)
		emit(ev)
		emit(commands())
	}
}

// harnessTiers is set as the harness takes it.
func harnessTiers(set dunconfig.TierSet) []harness.Tier {
	out := make([]harness.Tier, 0, len(set.Tiers))
	for _, t := range set.Tiers {
		out = append(out, harness.Tier{Name: t.Name, Provider: t.Provider, Model: t.Model, Effort: t.Effort, Use: t.Use, Off: t.Off, Source: t.Source})
	}
	return out
}

// sideCallSources says which side calls a side_calls entry in config.json
// places (the role table's source); the others are on their default.
func sideCallSources(fc dunconfig.DunConfig) map[string]string {
	sides, _ := fc.SideCallTiers()
	out := map[string]string{}
	for k := range fc.SideCalls {
		if v, ok := sides[k]; ok && v == strings.TrimSpace(fc.SideCalls[k]) {
			out[k] = dunconfig.SourceConfig
		}
	}
	return out
}

// tiers is dun doctor's tier check: each tier and what it resolves to in
// this directory, each side call's tier, and what the files set that is
// ignored (a repo naming a model, an unknown tier or side call).
func (d *doctor) tiers() {
	cfg := dunconfig.LoadConfig()
	ws, _ := os.Getwd()
	set := resolveTiers(cfg, ws, false)
	// An unavailable tier is in the warnings, with the fix.
	for _, t := range set.Tiers {
		d.add("ok", "tier", t.Line(), "")
	}
	d.add("ok", "tier", "default tier "+set.Default+" · "+set.DefaultSource, "")
	sides, warn := cfg.SideCallTiers()
	if !set.Any() {
		d.add("ok", "side call", "all on the session's model: no tier has a model", "")
	} else {
		for _, k := range dunconfig.SideCallKinds {
			d.add("ok", "side call", dunconfig.SideCallLine(k, sides[k], set), "")
		}
	}
	for _, w := range append(set.Warnings, warn...) {
		d.add("warn", "tier", w, "/login the provider, or change or remove the entry in "+dunconfig.ConfigPath())
	}
	d.add("ok", "role", harness.RoleHeader(), "")
	main := cfg.Resolve(dunconfig.FacetMain, modelFlags, os.Getenv)
	var bad strings.Builder
	rr := doctorRoles(cfg, set, sides, &bad)
	for _, r := range rr.All() {
		d.add("ok", "role", r.Line(main.Model), "")
	}
	for _, n := range rr.TierDefaultNotes() {
		d.add("ok", "role", n, "")
	}
	for _, l := range strings.Split(strings.TrimSpace(bad.String()), "\n") {
		if l != "" {
			d.add("warn", "role", strings.TrimPrefix(l, "dun: "), "set a level in "+dunconfig.ConfigPath()+" or the flag")
		}
	}
}

// doctorRoles is the role resolver over the config as an engine started
// here would have it, its profiles from the stored capability records (a
// client is built for each model, no request is made). Effort values that
// are no level are said on warn.
func doctorRoles(fc dunconfig.DunConfig, set dunconfig.TierSet, sides map[string]string, warn io.Writer) harness.RoleResolver {
	main := fc.Resolve(dunconfig.FacetMain, modelFlags, os.Getenv)
	child := fc.Resolve(dunconfig.FacetChild, modelFlags, os.Getenv)
	cfg := harness.Config{Endpoint: harness.Endpoint{URL: main.URL, Model: main.Model}, Provider: main.Provider, MainSource: main.Source,
		ChildModel: child.Model, ChildProvider: child.Provider, ChildSource: child.Source,
		Tiers: harnessTiers(set), DefaultTier: set.Default, SideCallTiers: sides, SideCallSources: sideCallSources(fc)}
	el := resolveEfforts(main, child, modelFlags[dunconfig.FacetChild].Effort, warn)
	cfg.ChildEffort, cfg.ChildEffortSource = el.child, el.childSrc
	saved, isSaved := harness.SavedEffort()
	mainEffort, mainSrc := saved, "/effort"
	if !isSaved {
		mainEffort, mainSrc = harness.EffortDefault, "default"
		if el.main != "" {
			mainEffort, mainSrc = el.main, "facets.main"
		}
	}
	profile := func(provider, model string) harness.ModelProfile {
		url, m := main.URL, model
		if m == "" {
			m = main.Model
		}
		if provider != "" {
			if p, ok := fc.Named(provider); ok {
				url = p.URL
			}
		}
		if url == "" || m == "" {
			return harness.ModelProfile{Provider: provider, Model: m}
		}
		c := llm.NewClient(url, "", m)
		harness.TrackCaps(c, effortOff(fc, url))
		if d, err := reasoningDialect(fc, url); err == nil {
			c.SetReasoningDialect(d)
		}
		return harness.ProfileOf(provider, m, c, 0)
	}
	return harness.RoleResolver{Cfg: cfg, MainEffort: mainEffort, MainSource: mainSrc, Saved: saved, Profile: profile}
}
