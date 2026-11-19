package dunconfig

import (
	"strings"
	"testing"
)

func tierCfg() DunConfig {
	c := facetCfg()
	c.Tiers = map[string]Tier{
		TierFast:  {Provider: "far", Model: "small", Effort: "low", Use: "config fast"},
		TierThink: {Model: "big"},
	}
	return c
}

// flag > env > config for the model, field by field; use: repo > config >
// the shipped text.
func TestResolveTiers(t *testing.T) {
	c := tierCfg()
	set := c.ResolveTiers(TierFlags{}, nil, RepoTiers{})
	fast, _ := set.Get(TierFast)
	if fast.Model != "small" || fast.Provider != "far" || fast.Effort != "low" || fast.Source != SourceConfig || fast.Use != "config fast" || !fast.On() {
		t.Fatalf("config: %+v", fast)
	}
	work, _ := set.Get(TierWork)
	if work.On() || work.Use != DefaultTierUse[TierWork] || work.What() != "the child default (no model set)" {
		t.Fatalf("unconfigured work: %+v", work)
	}
	if set.Default != TierWork || set.DefaultSource != SourceDefault || len(set.Warnings) != 0 {
		t.Fatalf("default: %+v", set)
	}

	env := envOf(map[string]string{"DUN_TIER_FAST": "env-small", "DUN_TIER_WORK": "gw/env-work", "DUN_DEFAULT_TIER": "fast"})
	set = c.ResolveTiers(TierFlags{}, env, RepoTiers{Use: map[string]string{TierFast: "repo fast"}, Default: TierThink})
	fast, _ = set.Get(TierFast)
	if fast.Model != "env-small" || fast.Source != SourceEnv || fast.Provider != "far" || fast.Use != "repo fast" {
		t.Fatalf("env model keeps the config's provider, the repo's use wins: %+v", fast)
	}
	work, _ = set.Get(TierWork)
	if work.Model != "env-work" || work.Provider != "gw" {
		t.Fatalf("env provider/model: %+v", work)
	}
	if set.Default != TierFast || set.DefaultSource != SourceEnv {
		t.Fatalf("env default beats the repo's: %+v", set)
	}

	set = c.ResolveTiers(TierFlags{Picks: map[string]string{TierThink: "flag-big"}, Default: TierWork}, env, RepoTiers{Default: TierThink})
	think, _ := set.Get(TierThink)
	if think.Model != "flag-big" || think.Source != SourceFlag || set.Default != TierWork || set.DefaultSource != SourceFlag {
		t.Fatalf("flag: %+v %+v", think, set)
	}
	set = c.ResolveTiers(TierFlags{}, nil, RepoTiers{Default: TierThink})
	if set.Default != TierThink || set.DefaultSource != "repo" {
		t.Fatalf("repo default beats config's: %+v", set)
	}
}

// What a repo may not set is ignored and said; so is a bad name or a
// default that is no tier, and a provider not logged in is off.
func TestResolveTiers_Warnings(t *testing.T) {
	c := tierCfg()
	c.Tiers["huge"] = Tier{Model: "x"}
	c.Tiers[TierWork] = Tier{Effort: "high"} // effort with no model
	c.Tiers[TierThink] = Tier{Provider: "nowhere", Model: "big"}
	c.DefaultTier = "medium"
	set := c.ResolveTiers(TierFlags{}, nil, RepoTiers{
		Use:     map[string]string{"tiny": "x"},
		Ignored: []string{"tiers.fast.model (/r/dun.json)"},
	})
	got := strings.Join(set.Warnings, "\n")
	for _, want := range []string{
		"tiers.huge in", "the repo's tiers.tiny is not a tier",
		"the repo's tiers.fast.model (/r/dun.json) is ignored",
		"tiers.work sets no model", `default_tier in`, `"medium" is not a tier`,
		"tier think is unavailable (no saved provider nowhere): its spawns run on the child default",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("warnings lack %q:\n%s", want, got)
		}
	}
	if set.Default != TierWork {
		t.Errorf("a bad default_tier falls back to work: %q", set.Default)
	}
	fast, _ := set.Get(TierFast)
	if fast.Model != "small" {
		t.Errorf("the repo's model field must not reach the tier: %+v", fast)
	}
	think, _ := set.Get(TierThink)
	if think.Off == "" || !strings.Contains(think.Line(), "off (no saved provider nowhere)") {
		t.Errorf("unsaved provider: %+v", think)
	}
}

func TestSideCallTiers(t *testing.T) {
	var c DunConfig
	got, warn := c.SideCallTiers()
	if len(warn) != 0 || len(got) != len(SideCallKinds) {
		t.Fatalf("defaults: %v %v", got, warn)
	}
	// The ones on the session's prompt cache stay on the session's model.
	for _, k := range WindowSideCalls {
		if got[k] != TierSession {
			t.Errorf("%s defaults to %q, want session", k, got[k])
		}
	}
	if got["plan_summary"] != TierFast || got["journal"] != TierFast {
		t.Errorf("summaries: %v", got)
	}
	c.SideCalls = map[string]string{"fold": "work", "suggest": "fast", "nope": "fast", "journal": "huge"}
	got, warn = c.SideCallTiers()
	if got["fold"] != TierWork || got["suggest"] != TierFast || got["journal"] != TierFast {
		t.Errorf("overrides: %v", got)
	}
	w := strings.Join(warn, "\n")
	if !strings.Contains(w, "side_calls.nope is not a side call") || !strings.Contains(w, `side_calls.journal "huge" is not a tier`) {
		t.Errorf("warnings: %v", warn)
	}
	// A window-sized call put on a tier is warned about, each one.
	for _, k := range []string{"fold", "suggest"} {
		if !strings.Contains(w, "side_calls."+k+` "`) || !strings.Contains(w, k+" sends up to the session's window") {
			t.Errorf("no window warning for %s: %v", k, warn)
		}
	}
	c.SideCalls = map[string]string{"rescue": TierSession, "journal": TierFast}
	if _, warn = c.SideCallTiers(); len(warn) != 0 {
		t.Errorf("session and a small call on fast are fine: %v", warn)
	}
	// Each kind lists in SideCallKinds once.
	for _, k := range SideCallKinds {
		if !IsSideCall(k) {
			t.Errorf("%s has no default", k)
		}
	}
	set := tierCfg().ResolveTiers(TierFlags{}, nil, RepoTiers{})
	if l := SideCallLine("journal", TierFast, set); !strings.Contains(l, "small on far") {
		t.Errorf("line %q", l)
	}
	if l := SideCallLine("commit", TierWork, set); !strings.Contains(l, "the session's model (work has no model)") {
		t.Errorf("line %q", l)
	}
}

// The effort values the config and the flags take: agentkit's levels,
// default, off (the old name of none) and lowest; a tier's other value is
// ignored with a warning.
func TestValidEffort(t *testing.T) {
	for _, ok := range []string{"default", "off", "none", "minimal", "low", "Medium", "high", "xhigh", "max", "lowest"} {
		if !ValidEffort(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"", "huge", "on"} {
		if ValidEffort(bad) {
			t.Errorf("%q taken", bad)
		}
	}
	c := tierCfg()
	c.Tiers[TierThink] = Tier{Model: "big", Effort: "huge"}
	set := c.ResolveTiers(TierFlags{}, nil, RepoTiers{})
	if th, _ := set.Get(TierThink); th.Effort != "" || !strings.Contains(strings.Join(set.Warnings, "\n"), `tiers.think.effort "huge" is not a level`) {
		t.Errorf("think %+v, warnings %q", th, set.Warnings)
	}
}
