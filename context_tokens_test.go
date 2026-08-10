package dun

import (
	"context"
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/agentkit/mcpmgr"
)

// fakeCounter counts words, so a "measured" number is distinguishable from the
// chars/4 estimate at a glance. failOn makes one specific part unmeasurable.
type fakeCounter struct {
	calls  int
	failOn func(text string) bool
}

func (f *fakeCounter) CountTokens(_ context.Context, text string) (int, bool) {
	f.calls++
	if f.failOn != nil && f.failOn(text) {
		return 0, false
	}
	return len(strings.Fields(text)), true
}

// nonCounter is a runner with no tokenizer — the Anthropic/OpenAI case, and the
// reason CountTokens returns ok rather than an error.
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
	if sum := bd.Prompt + got["built-in tools"] + got["mcp: raglit"] + got["mcp: chrome"]; sum != bd.Total {
		t.Errorf("Total %d != sum of its parts %d", bd.Total, sum)
	}
}

// TestOnePartThatCannotBeCountedDowngradesTheWholeTable pins the all-or-nothing
// rule. A table whose prompt was measured and whose schemas were estimated has
// no honest label, and the failure is silent: every number still looks precise.
func TestOnePartThatCannotBeCountedDowngradesTheWholeTable(t *testing.T) {
	sys, defs, mcp := fixture()
	// The tokenizer answers for everything except the chrome server's schemas.
	c := &fakeCounter{failOn: func(text string) bool { return strings.Contains(text, "fetch a page") }}
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
// one call for the prompt and one per group, never one per tool definition.
func TestTheTokenizerIsAskedOncePerPart(t *testing.T) {
	sys, defs, mcp := fixture()
	c := &fakeCounter{}
	measureSystem(context.Background(), c, sys, defs, mcp)
	if want := 4; c.calls != want { // prompt + built-ins + raglit + chrome
		t.Errorf("CountTokens called %d times, want %d (one per part, not per tool)", c.calls, want)
	}
}
