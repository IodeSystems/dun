package dun

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/agent"
)

// The exact poison from a real session: node_edit arguments cut off mid-string.
const poisonedArgs = `{"node":"app/src/test/java/com/termux/app/ScrollKeysTest.java","newText":"package com.termux.app;\n\npublic class ScrollKeysTest {\n\n    @Test\n   `

func TestSanitizeRescuesPoisonedToolCall(t *testing.T) {
	in := agent.Entry{
		ID: "abc", Kind: agent.KindToolCall, Content: poisonedArgs,
		ToolCallID: "call_1", ToolName: "node_edit", CreatedAt: 42,
	}
	got := sanitizeOnLoad(in)

	if got.Kind == agent.KindToolCall {
		t.Fatal("still a tool call — the session would stay unresumable")
	}
	if got.Kind != agent.KindNotification {
		t.Fatalf("kind = %q, want notification", got.Kind)
	}
	// The work must SURVIVE: this is the model's own partial output.
	if !strings.Contains(got.Content, "public class ScrollKeysTest") {
		t.Error("partial work was discarded")
	}
	if !strings.Contains(got.Content, "node_edit") {
		t.Error("should name the tool that failed")
	}
	// Must not correlate as half of a tool exchange whose result never exists.
	if got.ToolCallID != "" || got.ToolName != "" {
		t.Error("tool correlation should be cleared")
	}
	if got.CreatedAt != in.CreatedAt || got.ID != in.ID {
		t.Error("ordering identity should be preserved")
	}
}

// Valid entries must pass through untouched — the sanitizer must not rewrite
// history it has no business touching.
func TestSanitizeLeavesValidEntriesAlone(t *testing.T) {
	for _, e := range []agent.Entry{
		{Kind: agent.KindToolCall, Content: `{"node":"a.java","newText":"ok"}`, ToolName: "node_edit", ToolCallID: "c1"},
		{Kind: agent.KindAssistant, Content: "not json at all, but not a tool call"},
		{Kind: agent.KindToolResult, Content: "plain text result"},
		{Kind: agent.KindUser, Content: "{malformed but a user message"},
	} {
		got := sanitizeOnLoad(e)
		if got.Kind != e.Kind || got.Content != e.Content {
			t.Errorf("rewrote a healthy %s entry", e.Kind)
		}
	}
}

// The rescued entry must not itself be deserializable as tool arguments —
// that is the property that stops it re-poisoning the request.
func TestSanitizedEntryIsNotValidArguments(t *testing.T) {
	got := sanitizeOnLoad(agent.Entry{Kind: agent.KindToolCall, Content: poisonedArgs, ToolName: "node_edit"})
	if json.Valid([]byte(got.Content)) {
		t.Error("rescued content still parses as JSON; it must be plain text")
	}
}
