package dun

import (
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/mcpmgr"
)

// The eval tool's bridged definition carries the example-first cheat sheet folded
// onto mcpshell's own description — so the essentials are inline, not gated
// behind the `prompt` tool.
func TestMcpToolDefs_EnrichesEval(t *testing.T) {
	defs := mcpToolDefs([]mcpmgr.MCPTool{
		{Name: "eval", Description: "Execute mcpshell code."},
		{Name: "search", Description: "Search docs."},
	})
	byName := map[string]string{}
	for _, d := range defs {
		byName[d.Function.Name] = d.Function.Description
	}

	eval := byName["eval"]
	if !strings.HasPrefix(eval, "Execute mcpshell code.") {
		t.Fatal("original description should be preserved")
	}
	for _, want := range []string{"export", "LAST expression", "|> unique()", "no `new`"} {
		if !strings.Contains(eval, want) {
			t.Fatalf("eval doc missing %q:\n%s", want, eval)
		}
	}
	// Tools without an entry are untouched.
	if byName["search"] != "Search docs." {
		t.Fatalf("non-enriched tool changed: %q", byName["search"])
	}
}
