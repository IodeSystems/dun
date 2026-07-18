package main

import (
	"strings"
	"testing"
)

func TestRenderers_Registry(t *testing.T) {
	// Unknown tool → generic (raw body).
	pv, full := renderToolResult(renderCtx{tool: "mystery", result: "just text"})
	if !strings.Contains(pv, "just text") || !strings.Contains(full, "just text") {
		t.Fatalf("generic renderer lost the text: %q / %q", pv, full)
	}

	// node_edit → diff stat preview + colorized body.
	diff := "--- a\n+++ b\n+added line\n-removed line\n"
	pv, full = renderToolResult(renderCtx{tool: "node_edit", result: diff})
	if !strings.Contains(pv, "+1 -1") {
		t.Fatalf("node_edit preview should be a diff stat, got %q", pv)
	}
	if !strings.Contains(full, "added line") {
		t.Fatalf("node_edit body should include the diff, got %q", full)
	}

	// search → JSON summary preview + pretty body.
	pv, full = renderToolResult(renderCtx{tool: "search", result: `[{"id":1},{"id":2},{"id":3}]`})
	if !strings.Contains(pv, "3 item(s)") {
		t.Fatalf("search preview should summarize the JSON array, got %q", pv)
	}
	if !strings.Contains(full, "\"id\"") {
		t.Fatalf("search body should be pretty JSON, got %q", full)
	}

	// search with non-JSON → falls back to generic.
	pv, _ = renderToolResult(renderCtx{tool: "search", result: "no results found"})
	if !strings.Contains(pv, "no results found") {
		t.Fatalf("non-JSON search should fall back to generic, got %q", pv)
	}
}
