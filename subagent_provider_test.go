package dun

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/llm"
)

// A child may run on another logged-in provider than its parent (USER
// 2026-10-30: the session on the Claude API, its children on smaller models).

type askedFor struct{ provider, model string }

// startParentOn starts a root harness whose ClientFor records what it was
// asked for and builds kid on an endpoint named after the provider.
func startParentOn(t *testing.T, kid *scriptRunner, asked *[]askedFor) *Harness {
	t.Helper()
	t.Setenv("DUN_HOME", t.TempDir())
	dir := t.TempDir()
	h, err := Start(context.Background(), Config{
		Workspace: dir, Servers: []Server{}, Client: &scriptRunner{},
		Endpoint: Endpoint{URL: "http://claude.test/v1", Model: "big"},
		ClientFor: func(provider, model string) (ChildClient, error) {
			*asked = append(*asked, askedFor{provider, model})
			if provider == "gone" {
				return ChildClient{}, errors.New(`provider "gone" is not logged in`)
			}
			return ChildClient{Runner: kid, Endpoint: Endpoint{URL: "http://" + provider + ".test/v1", Model: model}}, nil
		},
		Providers:   []string{"claude", "local"},
		SessionFile: filepath.Join(dir, "s.jsonl"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return h
}

func TestSubAgent_OnAnotherProvider(t *testing.T) {
	var asked []askedFor
	h := startParentOn(t, &scriptRunner{rounds: []roundFn{textRound("done")}}, &asked)
	sa, err := h.spawnAgent(context.Background(), "say done", "local", "qwen3-8b", "")
	if err != nil {
		t.Fatal(err)
	}
	waitIdle(t, sa)
	if len(asked) != 1 || asked[0] != (askedFor{"local", "qwen3-8b"}) {
		t.Errorf("ClientFor was asked %v", asked)
	}
	// The child's own endpoint: its window is its model's, not its parent's.
	if e := sa.h.cfg.Endpoint; e.URL != "http://local.test/v1" || e.Model != "qwen3-8b" {
		t.Errorf("child endpoint %+v", e)
	}
	if sa.h.cfg.Provider != "local" || sa.h.cfg.ChildProvider != "local" {
		t.Errorf("child provider %q, its children's %q", sa.h.cfg.Provider, sa.h.cfg.ChildProvider)
	}
	if r := sa.report(); !strings.Contains(r, "on qwen3-8b (local)") {
		t.Errorf("report does not say where it ran: %q", r)
	}
	if got := h.Agents()[0].Model; got != "qwen3-8b (local)" {
		t.Errorf("agents pane shows %q", got)
	}
	// Kept for a resume.
	if rec := loadAgentState(sa.h.cfg.SessionFile); rec.Provider != "local" || rec.Model != "qwen3-8b" {
		t.Errorf("record %+v", rec)
	}
}

// Another provider has no fallback to the parent's client: a mistake there
// would run, and bill, somewhere the parent did not ask for.
func TestSubAgent_AnotherProviderFailsInsteadOfFallingBack(t *testing.T) {
	var asked []askedFor
	h := startParentOn(t, &scriptRunner{}, &asked)
	if _, err := h.spawnAgent(context.Background(), "x", "gone", "m", ""); err == nil || !strings.Contains(err.Error(), "not logged in") {
		t.Errorf("unknown provider: %v", err)
	}
	if _, err := h.spawnAgent(context.Background(), "x", "local", "", ""); err == nil || !strings.Contains(err.Error(), "needs a model") {
		t.Errorf("provider with no model: %v", err)
	}
	if out := spawnTool(context.Background(), h, "x", "gone", "m", "", false); !strings.HasPrefix(out, "ERROR:") {
		t.Errorf("the tool must report the failure: %q", out)
	}
	if n := len(h.Agents()); n != 0 {
		t.Errorf("%d children registered for failed spawns", n)
	}
}

// The defaults (--child-model, --child-provider) apply only to a spawn that
// names neither: a model named alone is on the session's provider.
func TestSubAgent_ChildProviderDefault(t *testing.T) {
	var asked []askedFor
	h := startParentOn(t, &scriptRunner{rounds: []roundFn{textRound("a"), textRound("b")}}, &asked)
	h.cfg.ChildProvider, h.cfg.ChildModel = "local", "qwen3-8b"
	a, err := h.spawnAgent(context.Background(), "a", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	waitIdle(t, a)
	b, err := h.spawnAgent(context.Background(), "b", "", "haiku", "")
	if err != nil {
		t.Fatal(err)
	}
	waitIdle(t, b)
	want := []askedFor{{"local", "qwen3-8b"}, {"", "haiku"}}
	if len(asked) != 2 || asked[0] != want[0] || asked[1] != want[1] {
		t.Errorf("ClientFor was asked %v, want %v", asked, want)
	}
}

// The provider argument is offered only when there is another provider to
// name, and lists them.
func TestAgentToolDef_ProviderOnlyWithSeveral(t *testing.T) {
	props := func(td llm.ToolDef) map[string]any {
		return td.Function.Parameters.(map[string]any)["properties"].(map[string]any)
	}
	if _, ok := props(agentToolDef("", "", []string{"claude"}))["provider"]; ok {
		t.Error("one provider: nothing to choose")
	}
	td := agentToolDef("qwen3-8b", "local", []string{"claude", "local"})
	p, ok := props(td)["provider"].(map[string]any)
	if !ok || strings.Join(p["enum"].([]string), ",") != "claude,local" {
		t.Errorf("provider argument %v", props(td)["provider"])
	}
	if !strings.Contains(td.Function.Description, "Default model: qwen3-8b on provider local.") {
		t.Errorf("description %q", td.Function.Description)
	}
}

// A child on another key counts toward the session's spend, but that key's
// budget is not the session key's.
func TestSpendMeter_OtherKeysBudgetIsNotTheSessions(t *testing.T) {
	m := NewSpendMeter()
	m.SetLedger(NewSpendLedger(t.TempDir()), KeyFingerprint("ck"))
	other := m.NoteAs(SpendSource{Provider: "local", Key: KeyFingerprint("lk"), Model: "small"})
	other(llm.Usage{PromptTokens: 10, KeyBudget: llm.KeyBudget{Reported: true, Currency: "USD"}})
	if m.budget.Reported {
		t.Error("another key's budget became the session's")
	}
	if m.prompt != 10 {
		t.Errorf("its tokens were not counted: %d", m.prompt)
	}
	own := m.NoteAs(SpendSource{Provider: "claude", Key: KeyFingerprint("ck"), Model: "big"})
	own(llm.Usage{KeyBudget: llm.KeyBudget{Reported: true, Currency: "USD"}})
	if !m.budget.Reported {
		t.Error("the session key's own budget was dropped")
	}
}
