package main

import (
	"fmt"
	"io"
	"os"

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

// tiersProto is set for the settings event.
func tiersProto(set dunconfig.TierSet) []proto.TierInfo {
	out := make([]proto.TierInfo, 0, len(set.Tiers))
	for _, t := range set.Tiers {
		out = append(out, proto.TierInfo{Name: t.Name, Provider: t.Provider, Model: t.Model, Effort: t.Effort, Source: t.Source, Off: t.Off})
	}
	return out
}

// harnessTiers is set as the harness takes it.
func harnessTiers(set dunconfig.TierSet) []harness.Tier {
	out := make([]harness.Tier, 0, len(set.Tiers))
	for _, t := range set.Tiers {
		out = append(out, harness.Tier{Name: t.Name, Provider: t.Provider, Model: t.Model, Effort: t.Effort, Use: t.Use, Off: t.Off})
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
}
