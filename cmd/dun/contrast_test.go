package main

import (
	"strings"
	"testing"
)

const (
	sgrDim       = "38;5;240m"
	sgrSecondary = "38;5;246m"
	sgrOrange    = "38;5;214m"
)

// A tool call's result is content the person reads to see how an answer was
// produced: it is in the secondary token (246), not the chrome grey (240).
// Only the arrow is chrome.
func TestToolResult_PreviewIsReadable(t *testing.T) {
	preview, body := renderToolResult(renderCtx{tool: "notes__project_notes", result: "Notes: we release on Fridays."})
	if !strings.Contains(preview, sgrSecondary+"Notes: we release on Fridays.") {
		t.Errorf("preview text is not in the secondary tone: %q", preview)
	}
	if strings.Contains(preview, sgrDim+"  → Notes") {
		t.Errorf("preview text is in the chrome grey: %q", preview)
	}
	if !strings.Contains(body, sgrSecondary) {
		t.Errorf("the expanded body is not in the secondary tone: %q", body)
	}
	if stSecondary.GetForeground() == stDim.GetForeground() {
		t.Error("stSecondary and stDim are the same colour")
	}
}

// Orange means "needs attention / blocked". A started job, a queued ingest,
// a recap, a started child, a message: none of them needs the person.
func TestToolResult_OrangeOnlyForAttention(t *testing.T) {
	neutral := []renderCtx{
		{tool: "exec", result: "Started background job #3: go test ./..."},
		{tool: "exec_monitor", result: "#1 running 3s — a\n#2 exited 0 — b"},
		{tool: "ingest", result: "job 7 queued"},
		{tool: "recap", result: "replaced 12 entries, saved 4.1k tokens"},
		{tool: "agent", result: "Started agent #1: `count the callers`."},
		{tool: "agent", result: "agent #2 is running after 12s (3.4k tokens)\ntask: x"},
		{tool: "agent", result: "agent #2 is idle after 3m40s (41.2k tokens) on m\ntask: x\nanswer: 4"},
		{tool: "agent_monitor", result: "#1 RUNNING 12s — `a`\n#2 IDLE 4s — `b`"},
		{tool: "agent_monitor", result: "Answered agent #1 (\"which file?\") — it is running again."},
		{tool: "tell_parent", args: map[string]any{"message": "halfway"}, result: "sent."},
		{tool: "dun_msg", args: map[string]any{"to": "web", "text": "hi"}, result: "delivered"},
	}
	for _, rc := range neutral {
		if p, _ := renderToolResult(rc); strings.Contains(p, sgrOrange) {
			t.Errorf("%s %q: preview is orange: %q", rc.tool, firstLine(rc.result), p)
		}
	}
	blocked, _ := renderToolResult(renderCtx{tool: "agent_monitor",
		result: "agent #2 is running after 1m (2k tokens)\ntask: x\nBLOCKED, waiting on you: which file?"})
	if !strings.Contains(blocked, sgrOrange) || !strings.Contains(stripANSI(blocked), "#2 blocked") {
		t.Errorf("a blocked child does not ask for attention: %q", blocked)
	}
	refused, _ := renderToolResult(renderCtx{tool: "ship", result: "Verified, but NOT pushed: no remote."})
	if !strings.Contains(refused, sgrOrange) {
		t.Errorf("a ship that left the work local is not marked: %q", refused)
	}
}

// The report's first line as subAgent.report writes it.
func TestAgentReportHead_CurrentFormat(t *testing.T) {
	id, state, cost, ok := agentReportHead("agent #2 is idle after 3m40s (41.2k tokens) on m")
	if !ok || id != "2" || state != "idle" || cost != "41.2k tokens" {
		t.Errorf("got %q %q %q %v", id, state, cost, ok)
	}
}
