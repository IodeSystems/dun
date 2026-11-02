package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/dun"
	"github.com/iodesystems/dun/internal/dunconfig"
)

// reasoningDialects are the values a provider's "reasoning" takes.
var reasoningDialects = []string{llm.DialectOpenAI, llm.DialectOpenRouter, llm.DialectLlamaCpp, llm.DialectAnthropic}

// reasoningDialect is how the endpoint at url is asked to think:
// DUN_REASONING, else the active provider's "reasoning" when it is that
// endpoint, else the saved provider's at url, else the config's; "" detects
// it.
func reasoningDialect(fc dunconfig.DunConfig, url string) (string, error) {
	d := os.Getenv("DUN_REASONING")
	if d == "" {
		if p, ok := fc.Active(); ok && strings.TrimRight(p.URL, "/") == strings.TrimRight(url, "/") {
			d = p.Reasoning
		} else if p, ok := fc.ProviderAt(url); ok {
			d = p.Reasoning
		}
	}
	if d == "" {
		d = fc.Reasoning
	}
	if d != "" && !slices.Contains(reasoningDialects, d) {
		return "", fmt.Errorf("reasoning %q: want %s (or leave it unset to detect)", d, strings.Join(reasoningDialects, ", "))
	}
	return d, nil
}

// noProvider stands in for the LLM when providerMissing says why: every call
// fails with that, and nothing is contacted.
type noProvider struct{ why error }

func (n noProvider) ChatStream(context.Context, []llm.Message, []llm.ToolDef, *llm.ChatOpts) (<-chan llm.StreamChunk, error) {
	return nil, n.why
}

// Providerless tells the engine there is no provider to pick models from
// (dun.hasProvider): raglit's setup line stays quiet.
func (noProvider) Providerless() bool { return true }

// providerLabel is what the ledger calls the provider at u: its saved name,
// else its host.
func providerLabel(fc dunconfig.DunConfig, u string) string {
	if p, ok := fc.ProviderAt(u); ok && p.Name != "" {
		return p.Name
	}
	return dunconfig.HostName(u)
}

// The wizard itself is a Bubble Tea program — see setup.go (runSetupTUI).

// clientCache builds LLM runners by provider and model name, one per model on
// each endpoint, for the lifetime of the process. Provider "" is the session's
// own (url, key); another is a logged-in provider by name, so a session on the
// Claude API can run its children on a smaller model elsewhere (USER
// 2026-10-30).
//
// Cached rather than constructed per call because a client owns a retry policy
// and its backpressure state: two clients on the same model would each think
// they had the endpoint to themselves and would back off independently against
// one shared rate limit. Children on the same model must contend through ONE
// client, which is also why there is no concurrency cap on sub-agents — the
// client is where the queueing actually happens.
//
// tune and usageFor are applied to each client as it is built, before anyone
// can use it — so a child is asked to think in its endpoint's dialect, with the
// configured retries, and its spend reaches the same meter as the parent's, the
// ledger line naming the provider and model it ran on (dun/spend.go,
// dun/ledger.go).
func clientCache(fc dunconfig.DunConfig, url, key string, tune func(c *llm.Client, url string), usageFor func(url, key, model string) func(llm.Usage)) func(provider, model string) (dun.ChildClient, error) {
	var mu sync.Mutex
	cache := map[string]dun.ChildClient{}
	return func(provider, model string) (dun.ChildClient, error) {
		if strings.TrimSpace(model) == "" {
			return dun.ChildClient{}, fmt.Errorf("no model named")
		}
		u, k := url, key
		if provider != "" {
			p, ok := fc.Named(provider)
			if !ok {
				var names []string
				for _, p := range fc.ProviderList() {
					names = append(names, p.Name)
				}
				return dun.ChildClient{}, fmt.Errorf("provider %q is not logged in (logged in: %s)", provider, strings.Join(names, ", "))
			}
			u, k = p.URL, p.Key
		}
		id := strings.TrimRight(u, "/") + "\x00" + model
		mu.Lock()
		defer mu.Unlock()
		if c, ok := cache[id]; ok {
			return c, nil
		}
		c := llm.NewClient(u, k, model)
		if tune != nil {
			tune(c, u)
		}
		c.OnUsage = usageFor(u, k, model)
		cc := dun.ChildClient{Runner: c, Endpoint: dun.Endpoint{URL: u, Key: k, Model: model}}
		cache[id] = cc
		return cc, nil
	}
}

// tuneClient sets what the config says about a client on url: how it is asked
// to think (/effort) and its retries.
func tuneClient(fc dunconfig.DunConfig, c *llm.Client, url string) {
	// How /effort's level reaches the model (agentkit's reasoning dialects):
	// a LiteLLM proxy in front of OpenRouter names neither in its address.
	if d, err := reasoningDialect(fc, url); err != nil {
		log.Printf("dun: %v; detecting it", err)
	} else {
		c.SetReasoningDialect(d)
	}
	if fc.Retry5xxAttempts > 0 {
		c.Retry5xxAttempts = fc.Retry5xxAttempts // /retry attempts; the env wins, in dun.Start (retry.go)
	}
	if rb, tb, reqSet, turnSet := parseRetryBudgets(fc); reqSet || turnSet {
		if reqSet {
			c.RetryBudget = rb
		}
		if turnSet {
			if tb == 0 {
				c.DisableTurnRetry = true
			} else {
				c.TurnRetryBudget = tb
			}
		}
	}
	loopWatch.install(c, fc.Loops)
	c.ClaudePrices = dunconfig.LoadPrices()
}

// dropKeyEnv takes dun's LLM keys out of this process's environment, so that
// nothing it starts inherits them: exec, MCP servers, anything the model can make
// print `env`. Run in the engine once its clients hold the keys. A variable whose
// value is one of them goes too, whatever its name (ANTHROPIC_API_KEY); every
// other variable stays, because a project's own tools need theirs.
//
// This stops a key leaking by accident. It is no boundary: exec runs as the user
// and can read config.json. Isolation is --docker.
func dropKeyEnv(fc dunconfig.DunConfig, key string) {
	keys := map[string]bool{}
	for _, k := range append([]string{key, fc.Key}, providerKeys(fc)...) {
		if k != "" {
			keys[k] = true
		}
	}
	for _, kv := range os.Environ() {
		if name, value, ok := strings.Cut(kv, "="); ok && (name == "DUN_LLM_KEY" || keys[value]) {
			os.Unsetenv(name)
		}
	}
}

func providerKeys(fc dunconfig.DunConfig) []string {
	var out []string
	for _, p := range fc.Providers {
		out = append(out, p.Key)
	}
	return out
}

// parseRetryBudgets interprets the persisted retry budgets: request scope (a
// negative value means unbounded) and turn scope ("0" disables the turn-scope
// retry, same as DUN_TURN_RETRY_BUDGET). Empty = unset. Junk is ignored with a
// warning; an env var still wins over both, because applyRetryPolicy and
// turnRetryPolicy only override when set.
func parseRetryBudgets(fc dunconfig.DunConfig) (request, turn time.Duration, requestSet, turnSet bool) {
	if v := fc.RetryBudget; v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			fmt.Fprintf(os.Stderr, "dun: ignoring invalid retry_budget %q in config\n", v)
		} else {
			request, requestSet = d, true
		}
	}
	if v := fc.TurnRetryBudget; v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			fmt.Fprintf(os.Stderr, "dun: ignoring invalid turn_retry_budget %q in config\n", v)
		} else {
			turn, turnSet = d, true
		}
	}
	return request, turn, requestSet, turnSet
}
