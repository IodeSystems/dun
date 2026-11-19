package main

import (
	"os"

	"github.com/iodesystems/dun/internal/dunconfig"
	"github.com/iodesystems/dun/internal/harness"
	"github.com/iodesystems/dun/internal/mcpserver"
)

// The model tiers (dunconfig/tiers.go): resolved once at start, from
// $DUN_HOME/config.json, the workspace's dun.json (use and default_tier
// only), env and the flags, and handed to the harness.

// tierFlags are this engine's --tier-<name> and --default-tier, for the
// TUI's /model to resolve the tiers as this process does.
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

// harnessTiers is set as the harness takes it.
func harnessTiers(set dunconfig.TierSet) []harness.Tier {
	out := make([]harness.Tier, 0, len(set.Tiers))
	for _, t := range set.Tiers {
		out = append(out, harness.Tier{Name: t.Name, Provider: t.Provider, Model: t.Model, Effort: t.Effort, Use: t.Use})
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
	for _, t := range set.Tiers {
		level, fix := "ok", ""
		if t.Off != "" {
			level, fix = "warn", "/login "+t.Provider+", or change tiers."+t.Name+" in "+dunconfig.ConfigPath()
		}
		d.add(level, "tier", t.Line(), fix)
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
		d.add("warn", "tier", w, "remove it, or set it in "+dunconfig.ConfigPath())
	}
}
