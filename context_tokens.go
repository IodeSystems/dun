package dun

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/agentkit/mcpmgr"
)

// What the context costs before the conversation starts, broken down.
//
// `/context` used to report one number for "prompt + schemas", derived at four
// characters per token. That hid two things. Which part is expensive — a system
// prompt and forty MCP schemas are separately fixable, and one total says only
// that it is large. And that the number was a guess: agentkit measured
// chars-per-token at 5.54 on prose and 1.11 on a surveyor's metes and bounds, so
// a single ratio can be five times out and still look precise.
//
// So this counts EXACTLY when the endpoint can, estimates when it cannot, and
// says which it did. A count that is sometimes measured and sometimes guessed,
// rendered identically, is worse than an honest estimate.
//
// COSTS ARE MARGINAL, measured by leaving each part out. That is not a
// refinement — on Anthropic it is the only way to get an honest per-part number.
// Declaring any tool at all adds the provider's tool-use preamble, measured at
// ~497 tokens against a preamble-free baseline of 9:
//
//	no tools 9    1 tool 551    2 tools 596
//
// Counting each server's schemas on their own therefore charges that preamble
// once per server — four servers overstate by ~1.5k, and the parts stop adding
// up to the total. A leave-one-out delta answers the question a reader of
// `/context` actually has ("what would I save by turning this off"), and what no
// single part owns lands in one shared row instead of being smeared across them.

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
	Shared int           // cost no single part owns (the provider's tool-use preamble)
	Total  int
	// Rules is the project rules block's share of Prompt (rules.go), and
	// RulesMode the mode it was rendered in; "" when there are no rules.
	Rules     int
	RulesMode string
	// Files are the context files in Rules (the rule files and what they
	// import, rules.go), each with its share — always an estimate: the
	// marginal count of one file is not worth N more requests.
	Files []ContextPart
}

// promptCounter is agentkit's structured-count capability, named as an interface
// so this package asserts on the CAPABILITY rather than on *llm.Client. An
// endpoint that cannot count simply does not satisfy it.
type promptCounter interface {
	CountPrompt(ctx context.Context, system string, tools []llm.ToolDef) (int, bool)
}

// measureSystemTimeout bounds the whole measurement.
//
// Generous ON PURPOSE, and it is why this runs off the setup path. Behind
// corrallm, /upstream RESOLVES the model and makes the backend resident before
// forwarding, so the first count against a cold model pays for a model load —
// measured at 7.6s for one already-warm model, and a cold one is worse. This now
// makes N+3 calls rather than N+1, which widens the window it has to fit in.
//
// The interaction that makes a short timeout actively harmful: agentkit caches
// what it discovered per baseURL+model, INCLUDING having found nothing. A probe
// that times out once therefore downgrades the whole process to estimates, not
// just this measurement. Better to wait than to poison the cache.
const measureSystemTimeout = 60 * time.Second

// toolGroup is one owner's tool definitions.
type toolGroup struct {
	name  string
	defs  []llm.ToolDef
	bytes []byte // the same schemas as wire JSON, for the estimate path
}

// SkillsPart is the part name of the skill tool, whose description carries
// the skill list (skills.go): skills are their own row, not built-in tools.
const SkillsPart = "skills"

// groupTools splits tool definitions by owner: dun's own, the skill list,
// then one group per MCP server via mcpmgr.MCPTool.ServerID.
func groupTools(defs []llm.ToolDef, mcpTools []mcpmgr.MCPTool) []toolGroup {
	const builtin = "built-in tools"
	server := make(map[string]string, len(mcpTools))
	for _, t := range mcpTools {
		server[modelToolName(t)] = t.ServerID // the name the model sees: an added server's carries its id
	}
	byName := map[string]*toolGroup{}
	var order []string
	for _, d := range defs {
		owner := builtin
		if id := server[d.Function.Name]; id != "" {
			owner = "mcp: " + id
		} else if d.Function.Name == "skill" {
			owner = SkillsPart
		}
		g, seen := byName[owner]
		if !seen {
			g = &toolGroup{name: owner}
			byName[owner] = g
			order = append(order, owner)
		}
		g.defs = append(g.defs, d)
		if raw, err := json.Marshal(d); err == nil {
			g.bytes = append(g.bytes, raw...)
		}
	}
	rank := func(n string) int {
		switch n {
		case builtin:
			return 0
		case SkillsPart:
			return 1
		}
		return 2
	}
	sort.Slice(order, func(i, j int) bool {
		if ri, rj := rank(order[i]), rank(order[j]); ri != rj {
			return ri < rj // built-ins first, then the skills, then servers by name
		}
		return order[i] < order[j]
	})
	out := make([]toolGroup, 0, len(order))
	for _, name := range order {
		out = append(out, *byName[name])
	}
	return out
}

// measureSystem breaks the pre-conversation context into parts and counts them.
// rules is the part of sys that is the project rules block, rendered in
// rulesMode.
func measureSystem(ctx context.Context, runner any, sys, rules, rulesMode string, defs []llm.ToolDef, mcpTools []mcpmgr.MCPTool) SystemBreakdown {
	groups := groupTools(defs, mcpTools)

	if counter, ok := runner.(promptCounter); ok {
		cctx, cancel := context.WithTimeout(ctx, measureSystemTimeout)
		defer cancel()
		if exact, ok := countExact(cctx, counter, sys, groups); ok {
			if rules == "" {
				return exact
			}
			// The rules' share is marginal like every row: the system prompt
			// with them, less the system prompt without them. All-or-nothing
			// like the rest: a failed count falls back to estimates.
			without, ok1 := counter.CountPrompt(cctx, strings.Replace(sys, rules, "", 1), nil)
			base, ok2 := counter.CountPrompt(cctx, "", nil)
			if ok1 && ok2 {
				exact.Rules, exact.RulesMode = exact.Prompt-(without-base), rulesMode
				return exact
			}
		}
	}

	out := SystemBreakdown{Prompt: estimateTokens(sys), Rules: estimateTokens(rules)}
	if rules != "" {
		out.RulesMode = rulesMode
	}
	for _, g := range groups {
		out.Parts = append(out.Parts, ContextPart{Name: g.name, Tokens: estimateTokens(string(g.bytes))})
	}
	out.Total = total(out)
	return out
}

// countExact measures each part as a MARGINAL cost — the difference between the
// whole prompt and the whole prompt without that part — and returns ok=false if
// ANY count fails. See SystemBreakdown.Exact for why it is all-or-nothing.
//
// With a single tool group its own row absorbs the provider's preamble and
// Shared is zero. That is correct rather than a rounding artifact: when one
// server is the only server, turning it off really does save the preamble too.
func countExact(ctx context.Context, c promptCounter, sys string, groups []toolGroup) (SystemBreakdown, bool) {
	var all []llm.ToolDef
	for _, g := range groups {
		all = append(all, g.defs...)
	}
	// The envelope every count carries. Subtracting it is what makes these
	// numbers the cost of the SYSTEM CONTEXT rather than of a minimal request.
	base, ok := c.CountPrompt(ctx, "", nil)
	if !ok {
		return SystemBreakdown{}, false
	}
	withSystem, ok := c.CountPrompt(ctx, sys, nil)
	if !ok {
		return SystemBreakdown{}, false
	}
	whole, ok := c.CountPrompt(ctx, sys, all)
	if !ok {
		return SystemBreakdown{}, false
	}

	out := SystemBreakdown{Exact: true, Prompt: withSystem - base, Total: whole - base}
	owned := 0
	for i, g := range groups {
		without := make([]llm.ToolDef, 0, len(all)-len(g.defs))
		for j, other := range groups {
			if j != i {
				without = append(without, other.defs...)
			}
		}
		n, ok := c.CountPrompt(ctx, sys, without)
		if !ok {
			return SystemBreakdown{}, false
		}
		cost := whole - n
		owned += cost
		out.Parts = append(out.Parts, ContextPart{Name: g.name, Tokens: cost})
	}
	// Whatever no single part owns: the provider's tool-use preamble, plus any
	// non-additivity in the tokenizer. Derived rather than measured so the rows
	// add up to Total exactly, which is the property /context is read for.
	out.Shared = out.Total - out.Prompt - owned
	return out, true
}

func total(b SystemBreakdown) int {
	n := b.Prompt + b.Shared
	for _, p := range b.Parts {
		n += p.Tokens
	}
	return n
}

// estimateTokens is the four-characters-per-token fallback, kept for endpoints
// with no counter. It is only ever reported as an estimate.
func estimateTokens(s string) int { return len(s) / 4 }
