package harness

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/dun/internal/dunconfig"
)

// Model tiers (USER 2026-11-18): a spawn names the kind of work, the tier
// names the model.

func tierSet() []Tier {
	return []Tier{
		{Name: TierFast, Provider: "local", Model: "small", Effort: EffortLow, Use: "mechanical work"},
		{Name: TierWork, Use: "scoped research"}, // no model: the child default
		{Name: TierThink, Model: "big-think", Effort: EffortHigh, Use: "planning"},
	}
}

func TestSpawnTier(t *testing.T) {
	h := &Harness{cfg: Config{Tiers: tierSet(), ChildProvider: "kids", ChildModel: "child-m", ChildEffort: EffortMedium}}
	for _, c := range []struct {
		name                               string
		tier, provider, model, effort      string
		wantTier, wantP, wantM, wantEffort string
	}{
		{"configured tier", TierFast, "", "", "", TierFast, "local", "small", EffortLow},
		{"call effort beats the tier's", TierThink, "", "", EffortOff, TierThink, "", "big-think", EffortOff},
		{"default tier has no model: child default", "", "", "", "", "", "kids", "child-m", ""},
		{"a tier with no model: child default", TierWork, "", "", "", "", "kids", "child-m", ""},
		{"a model overrides the tier", TierFast, "", "named", "", "", "", "named", ""},
		{"a provider and model override the tier", TierThink, "far", "m2", "", "", "far", "m2", ""},
	} {
		tier, p, m, e, err := h.spawnTier(c.tier, c.provider, c.model, c.effort)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if tier != c.wantTier || p != c.wantP || m != c.wantM || e != c.wantEffort {
			t.Errorf("%s: got %q %q %q %q", c.name, tier, p, m, e)
		}
	}
	if _, _, _, _, err := h.spawnTier("huge", "", "", ""); err == nil || !strings.Contains(err.Error(), "fast, work, think") {
		t.Errorf("unknown tier: %v", err)
	}
	// The repo's (or config's) default_tier.
	h.cfg.DefaultTier = TierThink
	if tier, _, m, _, _ := h.spawnTier("", "", "", ""); tier != TierThink || m != "big-think" {
		t.Errorf("default tier think: %q %q", tier, m)
	}
}

// No tiers at all: everything is as before tiers.
func TestSpawnTier_NoTiersIsTheChildDefault(t *testing.T) {
	h := &Harness{cfg: Config{ChildModel: "child-m"}}
	tier, p, m, e, err := h.spawnTier("", "", "", "")
	if err != nil || tier != "" || p != "" || m != "child-m" || e != "" {
		t.Errorf("got %q %q %q %q %v", tier, p, m, e, err)
	}
}

// A spawn on a tier runs on its model and is shown with it: the report, the
// agents pane and the record a resume reads.
func TestSubAgent_OnATier(t *testing.T) {
	var asked []askedFor
	h := startParentOn(t, &scriptRunner{rounds: []roundFn{textRound("done")}}, &asked)
	h.cfg.Tiers = tierSet()
	sa, err := h.spawnAgentFor(context.Background(), "say done", TierFast, "", "", "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	waitIdle(t, sa)
	if len(asked) != 1 || asked[0] != (askedFor{"local", "small"}) {
		t.Errorf("ClientFor was asked %v", asked)
	}
	if got := h.Agents()[0]; got.Tier != TierFast || got.Model != "fast: small (local), effort low" {
		t.Errorf("agents pane: %+v", got)
	}
	if rec := loadAgentState(sa.h.cfg.SessionFile); rec.Tier != TierFast || rec.Model != "small" {
		t.Errorf("record %+v", rec)
	}
}

func toolJSON(t *testing.T, td llm.ToolDef) []byte {
	t.Helper()
	b, err := json.Marshal(td)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// With no tier naming a model the tool is byte for byte what it was; with
// tiers it gains a line per tier and the tier param. The sizes are logged:
// the tool is sent on every round.
func TestAgentToolDef_Tiers(t *testing.T) {
	before := toolJSON(t, agentToolDef("child-m", "", "", nil, nil, ""))
	unset := []Tier{{Name: TierFast, Use: "a"}, {Name: TierWork, Use: "b"}, {Name: TierThink, Use: "c"}}
	if got := toolJSON(t, agentToolDef("child-m", "", "", nil, unset, TierWork)); string(got) != string(before) {
		t.Errorf("tiers with no model changed the tool:\n%s\n%s", before, got)
	}
	td := agentToolDef("child-m", "", "", nil, tierSet(), TierWork)
	with := toolJSON(t, td)
	t.Logf("agent tool: %d bytes without tiers, %d with three (+%d)", len(before), len(with), len(with)-len(before))
	shipped := tierSet()
	for i := range shipped {
		shipped[i].Use = dunconfig.DefaultTierUse[shipped[i].Name]
	}
	full := toolJSON(t, agentToolDef("child-m", "", "", nil, shipped, TierWork))
	t.Logf("with the shipped use texts: %d bytes (+%d)", len(full), len(full)-len(before))
	desc := td.Function.Description
	for _, want := range []string{
		"fast: mechanical work (small on local, effort low)",
		"work: scoped research (the default model)",
		"think: planning (big-think, effort high)",
		"Default tier: work.", "A model given overrides the tier.",
	} {
		if !strings.Contains(desc, want) {
			t.Errorf("description lacks %q:\n%s", want, desc)
		}
	}
	ps := td.Function.Parameters.(map[string]any)["properties"].(map[string]any)
	p, ok := ps["tier"].(map[string]any)
	if !ok {
		t.Fatalf("no tier param: %v", ps)
	}
	if enum, _ := p["enum"].([]string); strings.Join(enum, ",") != "fast,work,think" {
		t.Errorf("tier enum %v", p["enum"])
	}
}

type namedRunner struct {
	agent.LLMRunner
	name string
}

// A side call runs on its tier's model; session, a tier with no model, no
// entry, or a client that cannot be built is the runner it had before.
func TestSideRunner(t *testing.T) {
	fallback := &namedRunner{name: "session"}
	var asked []askedFor
	h := &Harness{cfg: Config{
		Tiers:         tierSet(),
		SideCallTiers: map[string]string{"journal": TierFast, "fold": TierSession, "commit": TierWork, "rules": TierThink},
		ClientFor: func(provider, model string) (ChildClient, error) {
			asked = append(asked, askedFor{provider, model})
			if model == "big-think" {
				return ChildClient{}, errors.New("no")
			}
			return ChildClient{Runner: &namedRunner{name: provider + "/" + model}}, nil
		},
	}}
	name := func(r agent.LLMRunner) string { return r.(*namedRunner).name }
	for kind, want := range map[string]string{
		"journal": "local/small", // fast
		"fold":    "session",     // session: the prompt cache
		"commit":  "session",     // work has no model
		"rules":   "session",     // think's client fails
		"suggest": "session",     // no entry
	} {
		if got := name(h.sideRunner(kind, fallback)); got != want {
			t.Errorf("%s: on %s, want %s", kind, got, want)
		}
	}
	if len(asked) != 2 {
		t.Errorf("ClientFor asked %v; only fast and think have models", asked)
	}
}

// fold on its default, session, calls the Shaper's own runner: the one the
// session's prompt cache is on.
func TestFoldRunner_DefaultStaysOnTheSessionModel(t *testing.T) {
	inner := &scriptRunner{rounds: []roundFn{textRound("## Ask\nx\n")}}
	other := &scriptRunner{}
	h := &Harness{cfg: Config{
		Tiers:         tierSet(),
		SideCallTiers: map[string]string{"fold": TierSession},
		ClientFor: func(string, string) (ChildClient, error) {
			return ChildClient{Runner: other}, nil
		},
	}}
	r := foldRunner{inner: inner, h: h}
	if _, _, err := r.call(context.Background(), []llm.Message{{Role: "user", Content: "fold"}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(inner.sent) != 1 || len(other.sent) != 0 {
		t.Errorf("fold went to the session runner %d times, a tier's %d", len(inner.sent), len(other.sent))
	}
}
