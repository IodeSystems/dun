package dun

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/agentkit/mcpmgr"
)

// What the context costs before the conversation starts, broken down.
//
// `/context` used to report one number for "prompt + schemas", derived at four
// characters per token. That hid two different things. The first is WHICH part
// is expensive — a system prompt and forty tool schemas are separately fixable,
// and one number says only that the total is large. The second is worse: the
// ratio is a guess whose error is not small. agentkit measured it at 5.54
// characters per token on prose and 1.11 on a surveyor's metes and bounds, so a
// number derived from a single ratio can be five times out and still look
// precise.
//
// So this counts EXACTLY when the endpoint can tokenize (llama.cpp, vLLM, and
// anything behind corrallm's /upstream passthrough), estimates when it cannot,
// and says which it did. A count that is sometimes measured and sometimes
// guessed, rendered identically, is worse than an honest estimate.

// ContextPart is one named contributor to the pre-conversation context.
type ContextPart struct {
	Name   string
	Tokens int
}

// SystemBreakdown is the whole pre-conversation cost, in parts.
//
// Exact is all-or-nothing ON PURPOSE. A breakdown whose system prompt was
// measured and whose MCP schemas were estimated has no honest label: reporting
// it as exact is false for half the rows, and reporting it as estimated throws
// away a real measurement. Falling back wholesale keeps one claim true of every
// number in the table.
type SystemBreakdown struct {
	Exact  bool
	Prompt int
	Parts  []ContextPart // built-ins first, then one row per MCP server, sorted
	Total  int
}

// tokenCounter is agentkit's exact-count capability, named as an interface so
// this package asserts on the CAPABILITY rather than on *llm.Client. A runner
// that cannot count is not an error and not a special case — it simply does not
// satisfy this.
type tokenCounter interface {
	CountTokens(ctx context.Context, text string) (int, bool)
}

// measureSystemTimeout bounds the whole measurement.
//
// Generous ON PURPOSE, and it is why this runs off the setup path. Behind
// corrallm, /upstream RESOLVES the model and makes the backend resident before
// forwarding, so the first count against a cold model pays for a model load —
// measured at 7.6s for one already-warm model, and a cold one is worse.
//
// The interaction that makes a short timeout actively harmful: agentkit caches
// what it discovered per baseURL+model, INCLUDING having found nothing. A probe
// that times out once therefore downgrades the whole process to estimates, not
// just this measurement. Better to wait than to poison the cache.
const measureSystemTimeout = 45 * time.Second

// measureSystem breaks the pre-conversation context into parts and counts them.
//
// mcpTools carries the tool→server attribution (mcpmgr.MCPTool.ServerID); defs
// that match no MCP tool are dun's own, and are grouped as "built-in tools".
func measureSystem(ctx context.Context, runner any, sys string, defs []llm.ToolDef, mcpTools []mcpmgr.MCPTool) SystemBreakdown {
	server := make(map[string]string, len(mcpTools))
	for _, t := range mcpTools {
		server[t.Name] = t.ServerID
	}

	// Group the schema text by owner, in the shape that actually goes on the
	// wire. The old estimate summed name+description+fmt.Sprint(parameters),
	// which is neither what is sent nor what a tokenizer would be given.
	const builtin = "built-in tools"
	grouped := map[string][]byte{}
	order := []string{}
	for _, d := range defs {
		owner := builtin
		if id := server[d.Function.Name]; id != "" {
			owner = "mcp: " + id
		}
		if _, seen := grouped[owner]; !seen {
			order = append(order, owner)
		}
		b, err := json.Marshal(d)
		if err != nil {
			// A schema that will not marshal cannot be sent either; count it
			// as nothing rather than guessing at a shape we do not have.
			continue
		}
		grouped[owner] = append(grouped[owner], b...)
	}
	sort.Slice(order, func(i, j int) bool {
		if (order[i] == builtin) != (order[j] == builtin) {
			return order[i] == builtin // built-ins first, then servers by name
		}
		return order[i] < order[j]
	})

	out := SystemBreakdown{}
	counter, canCount := runner.(tokenCounter)
	if canCount {
		cctx, cancel := context.WithTimeout(ctx, measureSystemTimeout)
		defer cancel()
		if exact, ok := countExact(cctx, counter, sys, order, grouped); ok {
			return exact
		}
	}

	out.Prompt = estimateTokens(sys)
	for _, owner := range order {
		out.Parts = append(out.Parts, ContextPart{Name: owner, Tokens: estimateTokens(string(grouped[owner]))})
	}
	out.Total = total(out)
	return out
}

// countExact returns the measured breakdown, or ok=false if ANY part could not
// be counted. See SystemBreakdown.Exact for why it is all-or-nothing.
func countExact(ctx context.Context, c tokenCounter, sys string, order []string, grouped map[string][]byte) (SystemBreakdown, bool) {
	var out SystemBreakdown
	n, ok := c.CountTokens(ctx, sys)
	if !ok {
		return out, false
	}
	out.Prompt = n
	for _, owner := range order {
		n, ok := c.CountTokens(ctx, string(grouped[owner]))
		if !ok {
			return SystemBreakdown{}, false
		}
		out.Parts = append(out.Parts, ContextPart{Name: owner, Tokens: n})
	}
	out.Exact = true
	out.Total = total(out)
	return out, true
}

func total(b SystemBreakdown) int {
	n := b.Prompt
	for _, p := range b.Parts {
		n += p.Tokens
	}
	return n
}

// estimateTokens is the four-characters-per-token fallback, kept for endpoints
// with no tokenizer. It is only ever reported as an estimate.
func estimateTokens(s string) int { return len(s) / 4 }
