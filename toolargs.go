package dun

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
)

// A call missing what its tool requires is answered with the tool's whole
// signature, not one word about one field (USER 2026-10-27: recap failed
// three times on "needs a summary"; the model could not see what a correct
// call looks like). For any tool, built in or a server's:
//
//	ERROR: recap was called without summary (required).
//	recap takes {from: string, summary: string, keep?: string[], user?: string} — you sent {from, keep}.

// paramSig is a tool's parameters as {a: type, b?: type}: required ones
// bare, the rest with a ?, required first.
func paramSig(params any) string {
	p, _ := params.(map[string]any)
	props, _ := p["properties"].(map[string]any)
	req := requiredOf(p)
	names := make([]string, 0, len(props))
	for n := range props {
		names = append(names, n)
	}
	sort.SliceStable(names, func(i, j int) bool {
		ri, rj := req[names[i]], req[names[j]]
		if ri != rj {
			return ri
		}
		return names[i] < names[j]
	})
	parts := make([]string, 0, len(names))
	for _, n := range names {
		mark := "?"
		if req[n] {
			mark = ""
		}
		parts = append(parts, n+mark+": "+typeOf(props[n]))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

func requiredOf(p map[string]any) map[string]bool {
	out := map[string]bool{}
	switch r := p["required"].(type) {
	case []string:
		for _, n := range r {
			out[n] = true
		}
	case []any:
		for _, n := range r {
			if s, ok := n.(string); ok {
				out[s] = true
			}
		}
	}
	return out
}

// typeOf is a property's type as a word: string, integer, string[], object.
func typeOf(prop any) string {
	m, _ := prop.(map[string]any)
	t, _ := m["type"].(string)
	if t == "array" {
		return typeOf(m["items"]) + "[]"
	}
	if t == "" {
		return "any"
	}
	return t
}

// sentSig is the argument names a call sent, as {a, b}.
func sentSig(args map[string]any) string {
	names := make([]string, 0, len(args))
	for n := range args {
		names = append(names, n)
	}
	sort.Strings(names)
	return "{" + strings.Join(names, ", ") + "}"
}

// missingRequired is the required parameters a call left out or sent empty.
func missingRequired(params any, args map[string]any) []string {
	p, _ := params.(map[string]any)
	var out []string
	for n := range requiredOf(p) {
		v, ok := args[n]
		if s, isStr := v.(string); !ok || v == nil || (isStr && strings.TrimSpace(s) == "") {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// signatureError is a call's error with its tool's signature and what it
// sent.
func signatureError(tool, why string, params any, args map[string]any) string {
	return fmt.Sprintf("ERROR: %s %s.\n%s takes %s — you sent %s.", tool, why, tool, paramSig(params), sentSig(args))
}

// withRequiredArgs answers a call that leaves out a required parameter
// with its tool's signature, before the tool sees it.
func withRequiredArgs(inner agent.ToolDispatcher, defs func() []llm.ToolDef) agent.ToolDispatcher {
	return func(ctx context.Context, tc llm.ToolCall) (string, error) {
		for _, d := range defs() {
			if d.Function.Name != tc.Function.Name {
				continue
			}
			var args map[string]any
			_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
			if args == nil {
				args = map[string]any{}
			}
			if miss := missingRequired(d.Function.Parameters, args); len(miss) > 0 {
				return signatureError(tc.Function.Name, "was called without "+strings.Join(miss, ", ")+" (required)",
					d.Function.Parameters, args), nil
			}
			break
		}
		return inner(ctx, tc)
	}
}
