package dun

import (
	"context"
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/agentkit/mcpmgr"
)

// fakeCounter models the shape that made leave-one-out necessary: a fixed
// preamble the moment ANY tool is declared, plus a per-tool cost. Numbers chosen
// to match what Anthropic actually charged on 2026-08-10 (baseline 9, preamble
// ~497, ~45 per small tool) so the arithmetic under test is the real one.
const (
	fakeBase     = 9
	fakePreamble = 497
	fakePerTool  = 45
)

type fakeCounter struct {
	calls  int
	failOn func(system string, tools []llm.ToolDef) bool
}

func (f *fakeCounter) CountPrompt(_ context.Context, system string, tools []llm.ToolDef) (int, bool) {
	f.calls++
	if f.failOn != nil && f.failOn(system, tools) {
		return 0, false
	}
	n := fakeBase + len(strings.Fields(system))
	if len(tools) > 0 {
		n += fakePreamble + fakePerTool*len(tools)
	}
	return n, true
}

// nonCounter is a runner with no counter at all — the reason CountPrompt returns
// ok rather than an error.
type nonCounter struct{}

func fixture() (string, []llm.ToolDef, []mcpmgr.MCPTool) {
	sys := "you are a helpful agent working in a repository"
	def := func(name, desc string) llm.ToolDef {
		var d llm.ToolDef
		d.Type = "function"
		d.Function.Name = name
		d.Function.Description = desc
		d.Function.Parameters = map[string]any{"type": "object"}
		return d
	}
	defs := []llm.ToolDef{
		def("search", "search the corpus"),
		def("fetch", "fetch a page"),
		def("exec", "run a shell command"),
	}
	mcp := []mcpmgr.MCPTool{
		{Name: "search", ServerID: "raglit"},
		{Name: "fetch", ServerID: "chrome"},
	}
	return sys, defs, mcp
}

// TestBreakdownAttributesEveryToolToItsServer is the feature: one number said
// the total was large, these rows say which part to go and fix.
func TestBreakdownAttributesEveryToolToItsServer(t *testing.T) {
	sys, defs, mcp := fixture()
	bd := measureSystem(context.Background(), &fakeCounter{}, sys, defs, mcp)

	if !bd.Exact {
		t.Fatal("a runner that can count must produce an exact breakdown")
	}
	got := map[string]int{}
	for _, p := range bd.Parts {
		got[p.Name] = p.Tokens
	}
	for _, want := range []string{"built-in tools", "mcp: raglit", "mcp: chrome"} {
		if got[want] == 0 {
			t.Errorf("no row for %q; got %v", want, got)
		}
	}
	if len(bd.Parts) != 3 {
		t.Errorf("want 3 rows (built-ins + 2 servers), got %d: %v", len(bd.Parts), bd.Parts)
	}
	// exec matches no MCP tool, so it must land in built-ins rather than being
	// silently attributed to whichever server was seen last.
	if bd.Parts[0].Name != "built-in tools" {
		t.Errorf("built-ins must sort first, got %q", bd.Parts[0].Name)
	}
	if sum := bd.Prompt + bd.Shared + got["built-in tools"] + got["mcp: raglit"] + got["mcp: chrome"]; sum != bd.Total {
		t.Errorf("Total %d != sum of its parts %d", bd.Total, sum)
	}
}

// TestPerServerCostsExcludeTheSharedPreamble is the whole reason costs are
// marginal. Each group here holds exactly one tool, so an honest per-server cost
// is fakePerTool — NOT fakePerTool+fakePreamble, which is what counting each
// group on its own would report, once per server.
func TestPerServerCostsExcludeTheSharedPreamble(t *testing.T) {
	sys, defs, mcp := fixture()
	bd := measureSystem(context.Background(), &fakeCounter{}, sys, defs, mcp)

	for _, p := range bd.Parts {
		if p.Tokens != fakePerTool {
			t.Errorf("%s costs %d, want %d — the shared preamble is being charged to it",
				p.Name, p.Tokens, fakePerTool)
		}
	}
	if bd.Shared != fakePreamble {
		t.Errorf("Shared = %d, want the preamble %d", bd.Shared, fakePreamble)
	}
	// And the envelope every count carries must not appear anywhere: this is the
	// cost of the system context, not of a minimal request.
	if bd.Total != fakePreamble+3*fakePerTool+len(strings.Fields(sys)) {
		t.Errorf("Total %d still carries the request envelope", bd.Total)
	}
}

// TestOneServerOwnsThePreamble pins the edge the marginal model implies: with a
// single group, turning it off really does save the preamble too, so its row
// carries it and Shared is zero. Correct, not a rounding artifact.
func TestOneServerOwnsThePreamble(t *testing.T) {
	var d llm.ToolDef
	d.Function.Name = "only"
	bd := measureSystem(context.Background(), &fakeCounter{}, "sys",
		[]llm.ToolDef{d}, []mcpmgr.MCPTool{{Name: "only", ServerID: "solo"}})

	if len(bd.Parts) != 1 {
		t.Fatalf("want 1 part, got %v", bd.Parts)
	}
	if bd.Parts[0].Tokens != fakePreamble+fakePerTool {
		t.Errorf("sole group costs %d, want %d — it is the only thing switching the preamble on",
			bd.Parts[0].Tokens, fakePreamble+fakePerTool)
	}
	if bd.Shared != 0 {
		t.Errorf("Shared = %d, want 0 when one group owns everything", bd.Shared)
	}
}

// TestOnePartThatCannotBeCountedDowngradesTheWholeTable pins the all-or-nothing
// rule. A table whose prompt was measured and whose schemas were estimated has
// no honest label, and the failure is silent: every number still looks precise.
func TestOnePartThatCannotBeCountedDowngradesTheWholeTable(t *testing.T) {
	sys, defs, mcp := fixture()
	// The tokenizer answers for everything except the chrome server's schemas.
	c := &fakeCounter{failOn: func(_ string, tools []llm.ToolDef) bool {
		for _, t := range tools {
			if t.Function.Name == "fetch" {
				return true
			}
		}
		return false
	}}
	bd := measureSystem(context.Background(), c, sys, defs, mcp)

	if bd.Exact {
		t.Fatal("one uncountable part must downgrade the whole breakdown to estimated")
	}
	if bd.Prompt != estimateTokens(sys) {
		t.Errorf("prompt = %d, want the estimate %d — a measured row must not survive the downgrade",
			bd.Prompt, estimateTokens(sys))
	}
	if len(bd.Parts) != 3 {
		t.Errorf("the downgraded table still needs every row, got %d", len(bd.Parts))
	}
}

// TestNoTokenizerIsNotAnError — an endpoint without one estimates and says so,
// and must not cost a probe per part.
func TestNoTokenizerIsNotAnError(t *testing.T) {
	sys, defs, mcp := fixture()
	bd := measureSystem(context.Background(), nonCounter{}, sys, defs, mcp)

	if bd.Exact {
		t.Fatal("a runner with no tokenizer cannot produce exact counts")
	}
	if bd.Total == 0 || len(bd.Parts) != 3 {
		t.Fatalf("the estimated table must still be complete: %+v", bd)
	}
	if bd.Prompt != estimateTokens(sys) {
		t.Errorf("prompt = %d, want %d", bd.Prompt, estimateTokens(sys))
	}
}

// TestTheTokenizerIsAskedOncePerPart guards the cost of doing this at setup:
// a fixed few calls plus one per GROUP, never one per tool definition.
func TestTheTokenizerIsAskedOncePerPart(t *testing.T) {
	sys, defs, mcp := fixture()
	c := &fakeCounter{}
	measureSystem(context.Background(), c, sys, defs, mcp)
	// base + system + whole + one leave-one-out per group. Marginal costs are
	// why it is N+3 rather than N+1; the guard is that it stays per PART, never
	// per tool definition.
	if want := 6; c.calls != want {
		t.Errorf("CountPrompt called %d times, want %d (base+system+whole+3 groups)", c.calls, want)
	}
}
