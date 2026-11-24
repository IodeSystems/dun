package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestNodeEditRender(t *testing.T) {
	clean := `{"bytesAdded":24,"bytesRemoved":19,"created":false,"diagnostics":[],"node":"/a/X.java","note":"updated X.java -19 +24","validated":true,"validationSkipped":false}`
	incomplete := `{"action":"LANDED, NOT clean: fix the errors","newErrors":["X.java:12: E100: cannot find symbol"],"incomplete":true,"validated":false,"tip":"read the node","bytesAdded":5,"bytesRemoved":2,"node":"/a/X.java","note":"updated X.java -2 +5","diagnostics":[]}`
	diags := `{"bytesAdded":1,"bytesRemoved":1,"note":"n","validated":true,"diagnostics":[{"file":"X.java","severity":"warning","code":"W1","message":"unused","startLine":7,"context":[]}]}`
	skipped := `{"bytesAdded":1,"bytesRemoved":1,"note":"n","validated":false,"validationSkipped":true,"diagnostics":[]}`
	noNote := `{"bytesAdded":3,"bytesRemoved":1,"node":"/a/X.java","validated":true,"diagnostics":[]}`
	withDiff := "{\"note\":\"n\",\"diff\":\"--- a\\n+++ b\\n+x\\n-y\\n\"}"

	tests := []struct {
		name     string
		result   string
		wantPv   []string
		wantBody []string
		noPv     []string
		warned   bool
	}{
		{"clean", clean, []string{"updated X.java -19 +24"}, []string{"updated X.java"}, []string{"incomplete", "diagnostic", "+24 −19"}, false},
		{"no note", noNote, []string{"/a/X.java", "+3 −1"}, nil, []string{"incomplete"}, false},
		{"incomplete", incomplete, []string{"updated X.java -2 +5", "incomplete"}, []string{"LANDED, NOT clean", "X.java:12: E100", "tip: read the node"}, nil, true},
		{"diagnostics", diags, []string{"1 diagnostic"}, []string{"X.java:7: W1: unused"}, []string{"incomplete"}, false},
		{"skipped validation", skipped, []string{"n"}, nil, []string{"incomplete"}, false},
		{"non-JSON", "--- a\n+++ b\n+added\n-removed\n", []string{"+1 -1"}, []string{"added"}, nil, false},
		{"diff field", withDiff, nil, nil, []string{"+1 −1"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pv, body := renderToolResult(renderCtx{tool: "node_edit", result: tc.result})
			pv, body = ansi.Strip(pv), ansi.Strip(body)
			for _, w := range tc.wantPv {
				if !strings.Contains(pv, w) {
					t.Errorf("preview %q lacks %q", pv, w)
				}
			}
			for _, w := range tc.wantBody {
				if !strings.Contains(body, w) {
					t.Errorf("body %q lacks %q", body, w)
				}
			}
			for _, w := range tc.noPv {
				if strings.Contains(pv, w) {
					t.Errorf("preview %q has %q", pv, w)
				}
			}
			if got := toolWarned("node_edit", tc.result); got != tc.warned {
				t.Errorf("toolWarned = %v, want %v", got, tc.warned)
			}
		})
	}
}

func TestNodeEditFailedPayload(t *testing.T) {
	res := `ERROR: mcp tool error: [{"action":"RETRY with fixed code","diagnostics":[{"file":"X.java","severity":"error","code":"E1","message":"boom","startLine":3}]}]`
	_, body := renderToolResult(renderCtx{tool: "node_edit", result: res})
	body = ansi.Strip(body)
	for _, w := range []string{"ERROR: mcp tool error:", "RETRY with fixed code", "X.java:3: E1: boom"} {
		if !strings.Contains(body, w) {
			t.Errorf("body %q lacks %q", body, w)
		}
	}
	if toolWarned("node_edit", res) {
		t.Error("failed result must not be warned")
	}
}

func TestCallMarkWarned(t *testing.T) {
	cases := []struct {
		it   runItem
		want string
	}{
		{runItem{done: true}, "✓"},
		{runItem{done: true, warned: true}, "⚠"},
		{runItem{done: true, warned: true, failed: true}, "✗"},
		{runItem{}, "…"},
	}
	for _, c := range cases {
		if got := ansi.Strip(callMark(&c.it)); !strings.Contains(got, c.want) {
			t.Errorf("callMark(%+v) = %q, want %q", c.it, got, c.want)
		}
	}
}
