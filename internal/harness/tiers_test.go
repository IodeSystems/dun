package harness

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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

// With no tier naming a model, the agent tool is main's from before tiers,
// byte for byte: testdata/agent_tool_notier.golden.json is main's
// agentToolDef output (87e2564), written by that tree's code, not this one.
// Changed on purpose once since (2026-11-18, roles): the effort enum is
// agentkit's full scale plus lowest, +75 bytes (3473 to 3548 plain).
func TestAgentToolDef_NoTiersMatchesMain(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "agent_tool_notier.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	var golden map[string]json.RawMessage
	if err := json.Unmarshal(b, &golden); err != nil {
		t.Fatal(err)
	}
	unset := []Tier{{Name: TierFast, Use: "a"}, {Name: TierWork, Use: "b"}, {Name: TierThink, Use: "c", Off: "x"}}
	for name, td := range map[string]llm.ToolDef{
		"plain": agentToolDef("", "", "", nil, unset, TierThink),
		"child": agentToolDef("qwen3-8b", "local", "low", []string{"claude", "local"}, nil, ""),
	} {
		var want, got any
		_ = json.Unmarshal(golden[name], &want)
		_ = json.Unmarshal(toolJSON(t, td), &got)
		w, _ := json.Marshal(want)
		g, _ := json.Marshal(got)
		if string(w) != string(g) {
			t.Errorf("%s differs from main:\nmain %s\nnow  %s", name, w, g)
		}
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
	name := func(r agent.LLMRunner) string {
		if tr, ok := r.(tierRetry); ok {
			r = tr.tier
		}
		return r.(*namedRunner).name
	}
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

// errRunner refuses every call: as an error from ChatStream, or as the
// first chunk's error (a provider's 400, context too long).
type errRunner struct {
	inChunk bool
	calls   int
}

func (r *errRunner) ChatStream(context.Context, []llm.Message, []llm.ToolDef, *llm.ChatOpts) (<-chan llm.StreamChunk, error) {
	r.calls++
	if !r.inChunk {
		return nil, errors.New("401 bad key")
	}
	ch := make(chan llm.StreamChunk, 1)
	ch <- llm.StreamChunk{Error: "400 context length exceeded", Done: true}
	close(ch)
	return ch, nil
}

// A side call the tier model refuses is asked once more on the session's
// runner; a reply that started on the tier stays the tier's.
func TestSideRunner_RetriesOnTheSession(t *testing.T) {
	for _, inChunk := range []bool{false, true} {
		tierR := &errRunner{inChunk: inChunk}
		session := &scriptRunner{rounds: []roundFn{textRound("from the session")}}
		h := &Harness{cfg: Config{
			Tiers:         tierSet(),
			SideCallTiers: map[string]string{"journal": TierFast},
			ClientFor:     func(string, string) (ChildClient, error) { return ChildClient{Runner: tierR}, nil },
		}}
		text, errMsg, _, err := collectText(context.Background(), h.sideRunner("journal", session), []llm.Message{{Role: "user", Content: "x"}}, nil, nil)
		if err != nil || errMsg != "" || text != "from the session" || tierR.calls != 1 || len(session.sent) != 1 {
			t.Errorf("inChunk %v: %q %q %v, tier %d calls, session %d", inChunk, text, errMsg, err, tierR.calls, len(session.sent))
		}
	}
	// It worked on the tier: the session is not asked.
	tierR := &scriptRunner{rounds: []roundFn{textRound("from the tier")}}
	session := &scriptRunner{}
	h := &Harness{cfg: Config{
		Tiers:         tierSet(),
		SideCallTiers: map[string]string{"journal": TierFast},
		ClientFor:     func(string, string) (ChildClient, error) { return ChildClient{Runner: tierR}, nil },
	}}
	text, _, _, _ := collectText(context.Background(), h.sideRunner("journal", session), []llm.Message{{Role: "user", Content: "x"}}, nil, nil)
	if text != "from the tier" || len(session.sent) != 0 {
		t.Errorf("%q, session asked %d times", text, len(session.sent))
	}
	// A tier whose provider is not logged in is the session's at once.
	h.cfg.Tiers = []Tier{{Name: TierFast, Provider: "gone", Model: "small", Off: "no saved provider gone"}}
	if got := h.sideRunner("journal", session); got != agent.LLMRunner(session) {
		t.Errorf("an off tier got %T", got)
	}
}

// A spawn on a tier that cannot be served runs on the child default, and
// the result says so; it is not an error, so one bad tier does not stop
// every spawn.
func TestSubAgent_UnavailableTierFallsBack(t *testing.T) {
	var asked []askedFor
	h := startParentOn(t, &scriptRunner{rounds: []roundFn{textRound("done"), textRound("done")}}, &asked)
	h.cfg.ChildModel = "child-m"
	h.cfg.Tiers = []Tier{
		{Name: TierFast, Provider: "nowhere", Model: "small", Off: "no saved provider nowhere"},
		{Name: TierWork, Provider: "gone", Model: "w"}, // ClientFor refuses "gone"
		{Name: TierThink},
	}
	sa, err := h.spawnAgentFor(context.Background(), "x", TierFast, "", "", "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	waitIdle(t, sa)
	if sa.note != "tier fast unavailable: no saved provider nowhere; ran on child-m" || sa.tierName() != "" || sa.model != "child-m" {
		t.Errorf("off tier: note %q tier %q model %q", sa.note, sa.tierName(), sa.model)
	}
	if out := sa.noted("Started."); !strings.HasPrefix(out, "tier fast unavailable") {
		t.Errorf("result %q", out)
	}
	sa, err = h.spawnAgentFor(context.Background(), "x", TierWork, "", "", "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	waitIdle(t, sa)
	if !strings.HasPrefix(sa.note, `tier work unavailable: provider "gone" is not logged in; ran on child-m`) {
		t.Errorf("unbuildable tier: note %q", sa.note)
	}
}

// The fallback clientFor always had, a model on this session's provider
// with no client, is said too.
func TestSubAgent_SilentFallbackIsSaid(t *testing.T) {
	var asked []askedFor
	h := startParentOn(t, &scriptRunner{}, &asked)
	h.cfg.ClientFor = func(string, string) (ChildClient, error) { return ChildClient{}, errors.New("no such model") }
	for _, c := range []struct{ tier, model, want string }{
		{"", "missing", "missing unavailable: no such model; ran on this session's model"},
		{TierFast, "", "tier fast unavailable: no such model; ran on this session's model"},
	} {
		h.cfg.Tiers = []Tier{{Name: TierFast, Model: "missing"}}
		sa, err := h.spawnAgentFor(context.Background(), "x", c.tier, "", c.model, "", "", "", "")
		if err != nil {
			t.Fatal(err)
		}
		waitIdle(t, sa)
		if sa.note != c.want || sa.tierName() != "" {
			t.Errorf("%+v: note %q, tier %q", c, sa.note, sa.tierName())
		}
	}
}
