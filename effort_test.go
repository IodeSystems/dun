package dun

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/iodesystems/agentkit/llm"
)

// Each level's knobs on a request, and the level saved for the next
// session (USER 2026-10-26: a server with thinking off by default showed
// none; dun sent nothing to ask for it).
func TestEffort(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	for level, want := range map[string]struct {
		think  *bool
		budget int // -1: none sent
	}{
		EffortDefault: {nil, -1},
		EffortOff:     {ptr(false), -1},
		EffortLow:     {ptr(true), 1024},
		EffortMedium:  {ptr(true), 4096},
		EffortHigh:    {ptr(true), -1},
	} {
		o := &llm.ChatOpts{}
		applyEffort(o, EffortHigh) // a level before, cleared by the next
		applyEffort(o, level)
		if (o.EnableThinking == nil) != (want.think == nil) || (o.EnableThinking != nil && *o.EnableThinking != *want.think) {
			t.Errorf("%s: thinking %v", level, o.EnableThinking)
		}
		if (o.ReasoningBudgetTokens == nil) != (want.budget < 0) || (o.ReasoningBudgetTokens != nil && *o.ReasoningBudgetTokens != want.budget) {
			t.Errorf("%s: budget %v", level, o.ReasoningBudgetTokens)
		}
	}
	h := &Harness{}
	if h.Effort() != EffortDefault || LoadEffort() != EffortDefault {
		t.Fatal("not default before one is set")
	}
	if err := h.SetEffort("HIGH"); err != nil || h.Effort() != EffortHigh || LoadEffort() != EffortHigh {
		t.Errorf("set: %v %s %s", err, h.Effort(), LoadEffort())
	}
	if err := h.SetEffort("max"); err == nil {
		t.Error("an unknown level was taken")
	}
}

func ptr[T any](v T) *T { return &v }

// corrallm states what a request that sends no thinking settings gets
// (thinking_* beside context_length); /effort default says it, and says
// "the server decides" when the list does not (USER 2026-10-30).
func TestServerThinking_DefaultInWords(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"data":[{"id":"chat","context_length":86016,"thinking_default":"on","thinking_modes":["off","on"],
			"thinking_effort":"medium","thinking_budget":{"fraction_of_remaining":0.2,"min":2048,"max":8192}},
			{"id":"quiet","thinking_default":"off"},{"id":"fw","context_length":1048576}]}`))
	}))
	defer srv.Close()
	for model, want := range map[string]string{
		"chat":  "nothing sent: the server thinks at medium, up to 20% of what is left (2k–8k tokens)",
		"quiet": "nothing sent: the server does not think",
		"fw":    EffortAbout[EffortDefault],
	} {
		h := &Harness{cfg: Config{Endpoint: Endpoint{URL: srv.URL + "/v1", Model: model}}}
		h.LearnServerThinking(context.Background())
		if got := h.EffortWords(EffortDefault); got != want {
			t.Errorf("%s: %q, want %q", model, got, want)
		}
		if got := h.EffortWords(EffortHigh); got != EffortAbout[EffortHigh] {
			t.Errorf("%s: high is %q", model, got)
		}
	}
}
