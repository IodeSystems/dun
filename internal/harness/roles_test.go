package harness

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/dun/internal/dunconfig"
)

// Roles and model profiles (USER 2026-11-18): one resolver, one precedence,
// the effort clamped to the model the role runs on.

// thinkingOnlyClient is a client whose record says thinking cannot be
// turned off: no request is made.
func thinkingOnlyClient(model string) *llm.Client {
	c := llm.NewClient("http://only.test/v1", "", model)
	c.SetCapabilities(llm.Capabilities{Endpoint: "http://only.test/v1", Model: model, Facts: map[string]llm.Fact{
		llm.CapReasoningThinkingOnly: {Value: "true", Source: llm.SourceRefused, Evidence: "Reasoning is mandatory"},
	}})
	return c
}

// capsRunner runs a script, and answers for its effort as client does.
type capsRunner struct {
	*scriptRunner
	client *llm.Client
	opts   *[]*llm.ChatOpts // what each request asked, when set
}

func (r capsRunner) ChatStream(ctx context.Context, msgs []llm.Message, tools []llm.ToolDef, opts *llm.ChatOpts) (<-chan llm.StreamChunk, error) {
	if r.opts != nil {
		*r.opts = append(*r.opts, opts)
	}
	return r.scriptRunner.ChatStream(ctx, msgs, tools, opts)
}

func (r capsRunner) Capabilities() llm.Capabilities          { return r.client.Capabilities() }
func (r capsRunner) Effective(e llm.Effort) llm.EffortChoice { return r.client.Effective(e) }
func (r capsRunner) EffortSupport() []llm.EffortChoice       { return r.client.EffortSupport() }

// The lists this package keeps beside dunconfig's are the same.
func TestRoles_SideCallKindsMatchConfig(t *testing.T) {
	if !slices.Equal(SideCallKinds, dunconfig.SideCallKinds) {
		t.Errorf("SideCallKinds %v, dunconfig %v", SideCallKinds, dunconfig.SideCallKinds)
	}
	for _, k := range dunconfig.WindowSideCalls {
		if !windowSideCalls[k] {
			t.Errorf("%s is window-sized in dunconfig only", k)
		}
	}
	if len(windowSideCalls) != len(dunconfig.WindowSideCalls) {
		t.Errorf("windowSideCalls %v, dunconfig %v", windowSideCalls, dunconfig.WindowSideCalls)
	}
}

// Precedence per role, with where each field came from: call > flag/env >
// $DUN_HOME config > the built-in default.
func TestResolve_Precedence(t *testing.T) {
	cfg := Config{Tiers: []Tier{
		{Name: TierFast, Provider: "local", Model: "small", Source: "flag"},
		{Name: TierWork},
		{Name: TierThink, Model: "big-think", Effort: EffortMedium, Source: "config"},
	}, ChildProvider: "kids", ChildModel: "child-m", ChildSource: "config",
		SideCallTiers: map[string]string{"journal": TierFast, "fold": TierFast}, SideCallSources: map[string]string{"journal": "config"}}
	r := RoleResolver{Cfg: cfg, MainEffort: EffortHigh, MainSource: srcEffort, Saved: EffortHigh}
	type want struct{ model, msrc, tier, wanted, esrc string }
	for _, c := range []struct {
		name, role string
		arg        RoleArg
		cfg        func(*Config)
		want       want
	}{
		{"main: /effort", RoleMain, RoleArg{}, nil, want{"", srcSession, "", EffortHigh, srcEffort}},
		{"main: the call", RoleMain, RoleArg{Effort: EffortLow}, nil, want{"", srcSession, "", EffortLow, srcCall}},
		{"fast: built-in lowest", ChildRole(TierFast), RoleArg{}, nil, want{"small", "tier fast · flag", TierFast, EffortLowest, "tier default (fast→lowest)"}},
		{"think: tier's effort over the built-in high", ChildRole(TierThink), RoleArg{}, nil, want{"big-think", "tier think · config", TierThink, EffortMedium, "tiers.think.effort"}},
		{"think: the call's effort", ChildRole(TierThink), RoleArg{Effort: EffortOff}, nil, want{"big-think", "tier think · config", TierThink, EffortOff, srcCall}},
		{"work: no model is the child default, inheriting", ChildRole(TierWork), RoleArg{}, nil, want{"child-m", "facets.child · config", "", "", srcInherit}},
		{"a model overrides the tier, its effort too", ChildRole(TierThink), RoleArg{Model: "named"}, nil, want{"named", srcCall, "", "", srcInherit}},
		{"--child-effort over the tier's", ChildRole(TierThink), RoleArg{}, func(c *Config) { c.ChildEffort, c.ChildEffortSource = EffortLow, "--child-effort" },
			want{"big-think", "tier think · config", TierThink, EffortLow, "--child-effort"}},
		{"facets.child's effort under the tier's", ChildRole(TierThink), RoleArg{}, func(c *Config) { c.ChildEffort, c.ChildEffortSource = EffortLow, "facets.child" },
			want{"big-think", "tier think · config", TierThink, EffortMedium, "tiers.think.effort"}},
		{"facets.child's effort over the built-in", ChildRole(TierFast), RoleArg{}, func(c *Config) { c.ChildEffort, c.ChildEffortSource = EffortLow, "facets.child" },
			want{"small", "tier fast · flag", TierFast, EffortLow, "facets.child"}},
		{"the default tier", "child:", RoleArg{}, func(c *Config) { c.DefaultTier = TierFast }, want{"small", "tier fast · flag", TierFast, EffortLowest, "tier default (fast→lowest)"}},
		{"journal on its side_calls tier", "journal", RoleArg{}, nil, want{"small", "side_calls fast · config", TierFast, "", ""}},
		{"fold: lowest on its tier", "fold", RoleArg{}, nil, want{"small", "side_calls fast · default", TierFast, EffortLowest, srcDefault}},
		{"rescue: the session's", "rescue", RoleArg{}, nil, want{"", srcSession, "", EffortLowest, srcDefault}},
	} {
		rr := r
		if c.cfg != nil {
			c.cfg(&rr.Cfg)
		}
		got := rr.Resolve(c.role, c.arg)
		g := want{got.Model, got.ModelSource, got.Tier, got.Effort.Wanted, got.EffortSource}
		if g != c.want {
			t.Errorf("%s:\n got %+v\nwant %+v", c.name, g, c.want)
		}
	}
	// An unavailable tier is the child default, said.
	rr := r
	rr.Cfg.Tiers = []Tier{{Name: TierFast, Provider: "gone", Model: "small", Off: "no saved provider gone"}}
	if got := rr.Resolve(ChildRole(TierFast), RoleArg{}); got.Model != "child-m" || got.Tier != "" || got.Note != "tier fast unavailable: no saved provider gone" || got.Effort.Wanted != "" {
		t.Errorf("off tier: %+v", got)
	}
}

// lowest is none on a model that can turn thinking off, and the lowest level
// it takes, with agentkit's reason, on one that cannot.
func TestClamp_Lowest(t *testing.T) {
	normal := ProfileOf("", "m", llm.NewClient("http://x.test/v1", "", "m"), 0)
	if eff, why := clampEffort(normal, EffortLowest); eff != EffortOff || why != "" {
		t.Errorf("normal model: %q %q", eff, why)
	}
	only := ProfileOf("", "g", thinkingOnlyClient("g"), 0)
	eff, why := clampEffort(only, EffortLowest)
	if eff != EffortMinimal || !strings.HasPrefix(why, "minimal (none not supported") {
		t.Errorf("thinking-only: %q %q", eff, why)
	}
	if !only.ThinkingOnly || !strings.Contains(only.ThinkingOnlyWhy, "thinking_only=true") {
		t.Errorf("profile %+v", only)
	}
	// The fold's request on each.
	o := &llm.ChatOpts{}
	efforts.Apply(o, eff)
	if o.Effort != llm.EffortMinimal {
		t.Errorf("fold on a thinking-only model sends %q", o.Effort)
	}
}

// The picker marks each level the session's model does not take as asked,
// with the reason.
func TestEffortPicker_MarksUnavailable(t *testing.T) {
	h := &Harness{client: thinkingOnlyClient("g")}
	rows := h.EffortPicker()
	if rows[0].Level != EffortDefault || len(rows) != len(EffortLevels) {
		t.Fatalf("rows %+v", rows)
	}
	for _, r := range rows {
		switch r.Level {
		case EffortOff:
			if !strings.Contains(r.Unavailable, "none not supported") {
				t.Errorf("none: %+v", r)
			}
		case EffortMinimal, EffortLow, EffortHigh:
			if r.Unavailable != "" {
				t.Errorf("%s: %+v", r.Level, r)
			}
		}
	}
	// A fake keeps no record: every level is as asked.
	for _, r := range (&Harness{client: &scriptRunner{}}).EffortPicker() {
		if r.Unavailable != "" {
			t.Errorf("fake: %+v", r)
		}
	}
}

// A window-sized side call goes to its tier only when the tier's window
// holds the input; otherwise, and when no window is known, the session's.
func TestSideRunner_WindowFallsBack(t *testing.T) {
	for _, c := range []struct {
		name   string
		window int
		onTier bool
	}{{"fits", 100_000, true}, {"too small", 50, false}, {"unknown", 0, false}} {
		tierC := llm.NewClient("http://tier.test/v1", "", "small")
		if c.window > 0 {
			tierC.SetCapabilities(llm.Capabilities{Endpoint: "http://tier.test/v1", Model: "small", Facts: map[string]llm.Fact{
				llm.CapLimitsContext: {Value: strconv.Itoa(c.window), Source: llm.SourceDeclared}}})
		}
		tierR := capsRunner{scriptRunner: &scriptRunner{rounds: []roundFn{textRound("from the tier")}}, client: tierC}
		session := &scriptRunner{rounds: []roundFn{textRound("from the session")}}
		h := &Harness{cfg: Config{
			Tiers:         tierSet(),
			SideCallTiers: map[string]string{"suggest": TierFast},
			ClientFor:     func(string, string) (ChildClient, error) { return ChildClient{Runner: tierR}, nil },
		}}
		h.windows.Store("local\x00small", c.window) // no model-list GET
		text, _, _, err := collectText(context.Background(), h.sideRunner("suggest", session), []llm.Message{{Role: "user", Content: strings.Repeat("x", 4000)}}, nil, &llm.ChatOpts{})
		want := "from the session"
		if c.onTier {
			want = "from the tier"
		}
		if err != nil || text != want {
			t.Errorf("%s: %q %v, want %q", c.name, text, err, want)
		}
	}
}

// A tier's effort is clamped to the tier's own model: fold's lowest is
// minimal on a thinking-only tier model, though the session's model sends
// none.
func TestSideRunner_EffortClampedToTheTier(t *testing.T) {
	var sent []*llm.ChatOpts
	tierR := capsRunner{scriptRunner: &scriptRunner{rounds: []roundFn{textRound("ok")}}, client: thinkingOnlyClient("small"), opts: &sent}
	h := &Harness{client: &scriptRunner{}, cfg: Config{
		Tiers:         tierSet(),
		SideCallTiers: map[string]string{"journal": TierFast},
		ClientFor:     func(string, string) (ChildClient, error) { return ChildClient{Runner: tierR}, nil },
	}}
	r := h.sideRunnerRaw("journal", &scriptRunner{}).(tierRetry)
	r.effort = EffortLowest // as fold's role has it
	opts := h.foldOpts()
	if opts.Effort != llm.EffortNone {
		t.Fatalf("on the session: %q", opts.Effort)
	}
	if _, _, _, err := collectText(context.Background(), r, []llm.Message{{Role: "user", Content: "x"}}, nil, opts); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 1 || sent[0] == nil || sent[0].Effort != llm.EffortMinimal {
		t.Errorf("the tier got %+v", sent)
	}
	if opts.Effort != llm.EffortNone {
		t.Errorf("the caller's options changed: %q", opts.Effort)
	}
}

// A child on a tier is clamped to the tier's model, and a tier whose model
// takes no tool calls is the child default, with the note.
func TestSubAgent_TierProfile(t *testing.T) {
	var asked []askedFor
	h := startParentOn(t, &scriptRunner{rounds: []roundFn{textRound("done"), textRound("done")}}, &asked)
	only := capsRunner{scriptRunner: &scriptRunner{rounds: []roundFn{textRound("done")}}, client: thinkingOnlyClient("small")}
	h.cfg.ClientFor = func(provider, model string) (ChildClient, error) { return ChildClient{Runner: only}, nil }
	h.cfg.Tiers = []Tier{{Name: TierFast, Provider: "local", Model: "small"}}
	sa, err := h.spawnAgentFor(context.Background(), "x", TierFast, "", "", "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	waitIdle(t, sa)
	if sa.effective != EffortMinimal || sa.effort != EffortMinimal || !strings.HasPrefix(sa.effortWhy, "minimal (none not supported") {
		t.Errorf("fast's lowest on a thinking-only model: %q %q %q", sa.effective, sa.effort, sa.effortWhy)
	}
	if got := h.Agents()[0]; got.Effort != EffortMinimal {
		t.Errorf("agents event effort %q", got.Effort)
	}
	// No tool support: the child default (here the session's model).
	c := llm.NewClient("http://notools.test/v1", "", "small")
	c.SetCapabilities(llm.Capabilities{Endpoint: "http://notools.test/v1", Model: "small", Facts: map[string]llm.Fact{
		CapToolsSupported: {Value: "false", Source: llm.SourceHost}}})
	h.cfg.ClientFor = func(provider, model string) (ChildClient, error) {
		return ChildClient{Runner: capsRunner{scriptRunner: &scriptRunner{}, client: c}}, nil
	}
	sa, err = h.spawnAgentFor(context.Background(), "x", TierFast, "", "", "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	waitIdle(t, sa)
	if sa.note != "tier fast unavailable: small takes no tool calls (tools.supported=false); ran on the session's model" || sa.tierName() != "" {
		t.Errorf("no-tools tier: note %q tier %q", sa.note, sa.tierName())
	}
}

// With no config, every role sends what it sent before roles: main nothing,
// fold and rescue none, the other side calls their options untouched, a
// child the saved level it loads itself.
func TestRoles_NoConfigSendsAsBefore(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	h := &Harness{client: &scriptRunner{}}
	h.effort.Store(LoadEffort())
	type row struct{ wanted, effective string }
	want := map[string]row{
		RoleMain:             {EffortDefault, EffortDefault},
		ChildRole(TierFast):  {"", EffortDefault},
		ChildRole(TierWork):  {"", EffortDefault},
		ChildRole(TierThink): {"", EffortDefault},
		"fold":               {EffortLowest, EffortOff},
		"rescue":             {EffortLowest, EffortOff},
	}
	for _, k := range SideCallKinds {
		if _, ok := want[k]; !ok {
			want[k] = row{"", EffortDefault}
		}
	}
	got := h.Roles()
	if len(got) != len(want) {
		t.Fatalf("%d roles, want %d", len(got), len(want))
	}
	for _, c := range got {
		if w := want[c.Role]; (row{c.Effort.Wanted, c.Effort.Effective}) != w || c.Model != "" || c.Tier != "" {
			t.Errorf("%s: %+v, want %+v on the session's model", c.Role, c, w)
		}
	}
	// What each path writes on its request.
	main := &llm.ChatOpts{}
	h.sessionOpts(main, RoleMain)
	if main.Effort != llm.EffortUnset || main.ReasoningBudgetTokens != nil {
		t.Errorf("main sends %+v", main)
	}
	for _, k := range SideCallKinds {
		o := h.roleOpts(k)
		w := llm.EffortUnset
		if k == "fold" || k == "rescue" {
			w = llm.EffortNone
		}
		if o.Effort != w || o.ReasoningBudgetTokens != nil {
			t.Errorf("%s sends %+v", k, o)
		}
	}
}

// lowest is a level wherever one is taken; a typo from the child default
// is refused at spawn, not dropped; a resumed child's off is none.
func TestEffort_LowestTypoAndResume(t *testing.T) {
	if l, ok := NormalizeEffort(" Lowest "); !ok || l != EffortLowest {
		t.Errorf("lowest: %q %v", l, ok)
	}
	if _, ok := NormalizeEffort("hgih"); ok {
		t.Error("a typo was taken")
	}
	if loadedEffort("off") != EffortOff || loadedEffort("hgih") != "" || loadedEffort("") != "" {
		t.Errorf("loaded: %q %q", loadedEffort("off"), loadedEffort("hgih"))
	}
	var asked []askedFor
	h := startParentOn(t, &scriptRunner{rounds: []roundFn{textRound("done")}}, &asked)
	h.cfg.ChildEffort, h.cfg.ChildEffortSource = "hgih", "--child-effort"
	if _, err := h.spawnAgentFor(context.Background(), "x", "", "", "", "", "", "", ""); err == nil || !strings.Contains(err.Error(), `effort "hgih" (--child-effort)`) {
		t.Errorf("typo spawn: %v", err)
	}
	h.cfg.ChildEffort = "lowest"
	sa, err := h.spawnAgentFor(context.Background(), "x", "", "", "", "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	waitIdle(t, sa)
	if sa.effort != EffortOff {
		t.Errorf("lowest on a fake child: %q", sa.effort)
	}
}

// A configured fast or think tier with no effort now takes the tier's
// default: the source says so, and one start note per such tier.
func TestTierDefaultNotes(t *testing.T) {
	r := RoleResolver{Cfg: Config{Tiers: []Tier{
		{Name: TierFast, Model: "small"},
		{Name: TierWork, Model: "w"},
		{Name: TierThink, Model: "big", Effort: EffortMedium},
	}}}
	notes := r.TierDefaultNotes()
	if len(notes) != 1 || !strings.HasPrefix(notes[0], "tier fast (small) has no effort set: its spawns run at lowest") {
		t.Errorf("notes %q", notes)
	}
	if c := r.Resolve(ChildRole(TierFast), RoleArg{}); c.EffortSource != "tier default (fast→lowest)" {
		t.Errorf("source %q", c.EffortSource)
	}
	if n := (RoleResolver{}).TierDefaultNotes(); len(n) != 0 {
		t.Errorf("no tiers: %q", n)
	}
}
