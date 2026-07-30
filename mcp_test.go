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

// The node_query cheat sheet exists to pre-empt two specific habits that
// dominate real failures — a bare file path, and a grep pattern quoted twice.
// If a future edit drops them, the docs stop earning their tokens.
func TestToolDocs_NodeQueryPreemptsTheCommonMistakes(t *testing.T) {
	doc := toolDocs["node_query"]
	if doc == "" {
		t.Fatal("node_query has no cheat sheet")
	}
	for _, want := range []string{
		"PATH IS AN ID",      // 25 of 39 recorded failures
		"#'cmd/dun/tui.go'",  // the corrected form, copyable
		"ONE quoted arg",     // 8 of 39
		"~= is the regex op", // [name^=[A-Z]] in the corpus
		"fixed set",          // invented types
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("cheat sheet no longer mentions %q", want)
		}
	}
	// It rides in EVERY request, so it has to stay small.
	if len(doc) > 1200 {
		t.Errorf("cheat sheet is %d bytes — it is charged on every turn", len(doc))
	}
}

// And it must actually reach the model, appended to the server's description.
func TestToolDocs_ReachTheToolDefinition(t *testing.T) {
	defs := mcpToolDefs([]mcpmgr.MCPTool{{Name: "node_query", Description: "query nodes"}})
	if len(defs) != 1 {
		t.Fatal("expected one def")
	}
	if !strings.Contains(defs[0].Function.Description, "PATH IS AN ID") {
		t.Error("the cheat sheet is not attached to the tool definition")
	}
}
