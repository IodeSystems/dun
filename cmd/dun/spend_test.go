package main

import (
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/dun"
	"github.com/iodesystems/dun/internal/dunconfig"
	"github.com/iodesystems/dun/internal/spendmeter"
)

var spendNow = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

// The snapshot → wire mapping keeps "not reported" absent.
func TestSpendProto(t *testing.T) {
	m := spendmeter.NewMeter()
	if p := spendProto(m.Snapshot(), spendNow); p.Session != nil || p.LastTurn != nil || p.Key != nil || p.Limits != nil {
		t.Errorf("empty meter = %+v", p)
	}
	m.Note(llm.Usage{
		Cost: llm.Cost{Amount: 0.5, Currency: "USD", Reported: true},
		Limits: llm.RateLimits{Reported: true,
			Requests: llm.RateWindow{Remaining: 0, HasRemaining: true, Reported: true}},
		KeyBudget: llm.KeyBudget{Spend: 3, Currency: "USD", Source: llm.BudgetSourceLiteLLMHeaders, Reported: true},
	})
	m.EndTurn()
	p := spendProto(m.Snapshot(), spendNow)
	if p.Session == nil || *p.Session != 0.5 || p.LastTurn == nil || *p.LastTurn != 0.5 {
		t.Errorf("costs = %+v", p)
	}
	if p.Key == nil || p.Key.Limit != nil || p.Key.Remaining != nil || p.Key.ResetAt != "" {
		t.Errorf("key = %+v", p.Key)
	}
	if len(p.Limits) != 1 || p.Limits[0].Limit != nil || p.Limits[0].Remaining == nil || *p.Limits[0].Remaining != 0 {
		t.Errorf("limits = %+v", p.Limits)
	}
}

// Every client the per-model cache builds reports to the meter.
func TestClientCacheWiresOnUsage(t *testing.T) {
	var got int
	var model string
	get := clientCache(dunconfig.DunConfig{}, "http://127.0.0.1:1", "k", nil, func(_, _, m string) func(llm.Usage) { model = m; return func(llm.Usage) { got++ } })
	r, err := get("", "child-model")
	if err != nil {
		t.Fatal(err)
	}
	c, ok := r.Runner.(*llm.Client)
	if !ok || c.OnUsage == nil {
		t.Fatal("cached client has no OnUsage")
	}
	c.OnUsage(llm.Usage{})
	if got != 1 || model != "child-model" {
		t.Errorf("OnUsage is not the meter's (calls %d, model %q)", got, model)
	}
}

// A child on another logged-in provider gets that provider's endpoint and
// key, is billed to them, and shares one client with any other child on the
// same endpoint and model; a provider not logged in is an error naming the
// ones that are.
func TestClientCache_AnotherProvider(t *testing.T) {
	fc := dunconfig.DunConfig{Provider: "claude", Providers: []dunconfig.ProviderConfig{
		{Name: "claude", URL: "https://api.anthropic.com/v1", Key: "ck"},
		{Name: "local", URL: "http://127.0.0.1:2/v1", Key: "lk", Reasoning: llm.DialectLlamaCpp},
	}}
	var billed []string
	var tuned []string
	get := clientCache(fc, "https://api.anthropic.com/v1", "ck",
		func(_ *llm.Client, u string) { tuned = append(tuned, u) },
		func(u, k, m string) func(llm.Usage) {
			billed = append(billed, u+" "+k+" "+m)
			return func(llm.Usage) {}
		})
	cc, err := get("local", "qwen3-8b")
	if err != nil {
		t.Fatal(err)
	}
	want := dun.Endpoint{URL: "http://127.0.0.1:2/v1", Key: "lk", Model: "qwen3-8b"}
	if cc.Endpoint != want {
		t.Errorf("endpoint %+v, want %+v", cc.Endpoint, want)
	}
	if len(billed) != 1 || billed[0] != "http://127.0.0.1:2/v1 lk qwen3-8b" {
		t.Errorf("billed %q", billed)
	}
	if len(tuned) != 1 || tuned[0] != "http://127.0.0.1:2/v1" {
		t.Errorf("tuned %q", tuned)
	}
	again, _ := get("local", "qwen3-8b")
	if again.Runner != cc.Runner {
		t.Error("a second child on the same endpoint and model got its own client")
	}
	own, _ := get("", "qwen3-8b")
	if own.Runner == cc.Runner || own.Endpoint.URL != "https://api.anthropic.com/v1" {
		t.Errorf("the session's provider shares the other's client: %+v", own.Endpoint)
	}
	if _, err := get("openrouter", "x"); err == nil || !strings.Contains(err.Error(), "claude, local") {
		t.Errorf("unknown provider: %v", err)
	}
}

// A provider's "reasoning" reaches a client on its address even when it is
// not the active one: a child there is asked to think in its dialect.
func TestReasoningDialect_SavedProviderAtURL(t *testing.T) {
	t.Setenv("DUN_REASONING", "")
	fc := dunconfig.DunConfig{Provider: "claude", URL: "https://api.anthropic.com/v1", Providers: []dunconfig.ProviderConfig{
		{Name: "claude", URL: "https://api.anthropic.com/v1", Reasoning: llm.DialectAnthropic},
		{Name: "local", URL: "http://127.0.0.1:2/v1", Reasoning: llm.DialectLlamaCpp},
	}}
	for u, want := range map[string]string{
		"https://api.anthropic.com/v1": llm.DialectAnthropic,
		"http://127.0.0.1:2/v1/":       llm.DialectLlamaCpp,
		"http://127.0.0.1:3/v1":        "",
	} {
		if d, err := reasoningDialect(fc, u); err != nil || d != want {
			t.Errorf("%s: %q, %v; want %q", u, d, err, want)
		}
	}
}
