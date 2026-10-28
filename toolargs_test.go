package dun

import (
	"context"
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/llm"
)

// A call leaving out a required parameter (or sending it empty) gets the
// tool's whole signature and what it sent; a complete one goes through.
func TestRequiredArgs(t *testing.T) {
	var td llm.ToolDef
	td.Function.Name = "lookup"
	td.Function.Parameters = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"vin":   map[string]any{"type": "string"},
			"limit": map[string]any{"type": "integer"},
			"tags":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		},
		"required": []any{"vin"},
	}
	reached := false
	d := withRequiredArgs(func(context.Context, llm.ToolCall) (string, error) { reached = true; return "ok", nil },
		func() []llm.ToolDef { return []llm.ToolDef{td} })
	call := func(args string) string {
		var tc llm.ToolCall
		tc.Function.Name, tc.Function.Arguments = "lookup", args
		out, _ := d(context.Background(), tc)
		return out
	}
	out := call(`{"limit": 3, "vin": ""}`)
	for _, want := range []string{"lookup was called without vin (required)", "{vin: string, limit?: integer, tags?: string[]}", "you sent {limit, vin}"} {
		if !strings.Contains(out, want) {
			t.Errorf("lacks %q:\n%s", want, out)
		}
	}
	if reached {
		t.Error("the tool ran without its required parameter")
	}
	if out := call(`{"vin": "KND"}`); out != "ok" || !reached {
		t.Errorf("a complete call: %q", out)
	}
}

// recap with from and no summary: its shape, where reading went, and what
// was sent.
func TestRecap_SaysItsShape(t *testing.T) {
	h := &Harness{}
	d := withRecap(func(context.Context, llm.ToolCall) (string, error) { return "", nil }, h, nil)
	var tc llm.ToolCall
	tc.Function.Name, tc.Function.Arguments = "recap", `{"from": "was reported towed", "keep": null}`
	out, _ := d(context.Background(), tc)
	for _, want := range []string{"without summary", "{summary: string, from?: string, keep?: string[]}", "read_saved {ref}", "You sent {from, keep}"} {
		if !strings.Contains(out, want) {
			t.Errorf("lacks %q:\n%s", want, out)
		}
	}
}
