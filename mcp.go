package dun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/agentkit/mcpmgr"
)

// The MCP bridge — the whole "give the agent these tools" story is two small
// functions (mcpToolDefs advertises, mcpDispatcher executes), exactly as
// agentkit's demo shows. dun just runs it over THREE servers at once.

// toolDocs supplements a bridged tool's description with an ultra-short,
// example-FIRST cheat sheet. Rationale (from watching real sessions thrash):
// prescription mostly bounces off small models — so we only PRESCRIBE what is
// UNAVAILABLE (a model can't discover the absence of `new`), and let EXAMPLES
// carry everything else. mcpshell's own eval description buries these; the full
// reference is gated behind its `prompt` tool, which the model rarely calls. We
// fold the essentials into the definition it always sees. Every example here is
// verified to run against mcpshell.
var toolDocs = map[string]string{
	// node_query's own description teaches the grammar; what it cannot teach is
	// which HABIT from another language will bite you. Across 205 recorded calls
	// in real sessions, 39 distinct selectors failed and the spread was lopsided:
	// 25 were a bare file path written where an id belongs, 8 were a grep pattern
	// quoted a second time out of shell reflex. Everything else was a long tail
	// of one. So the four lines below are those two mistakes, pre-empted, plus
	// the shapes that answer most questions. Every example is verified to run.
	"node_query": "\n\nA BARE PATH IS NOT A SELECTOR — scope by path= instead (no quotes needed):\n" +
		"  path=cmd/dun/tui.go func                // ✓ what's in a file. NOT `cmd/dun/tui.go` (that parses as a type)\n" +
		"  #Start                                  // find by name anywhere; #'harness.go#Start' pins one\n" +
		"  #'harness.go#Start'::in.call > *        // who calls it (::out.call = what it calls)\n" +
		"  path=harness.go ::grep('-E Server|Harness')  // text search. ONE quoted arg — no inner \"quotes\", that matches nothing\n" +
		"  func name~=^Start                       // ~= is the regex op; = ^= $= *= are literal\n" +
		"Attribute brackets are optional: path=a/b.go ≡ [path=a/b.go]. Types are a fixed set (func method type struct file dir import …) — you cannot invent one.",
	"eval": "\n\nmcpshell is a JS subset — do the whole task in ONE eval that ENDS in the value:\n" +
		"  export let total = [1,2,3].reduce((a,b) => a + b, 0)   // export → survives to your NEXT eval; plain let/const does not\n" +
		"  total * 2                                              // the LAST expression is the output (console.log is not the result)\n" +
		"  [3,1,2,1] |> unique()                                  // dedup. NOT `new Set(...)` — `new` does not exist here\n" +
		"There is no `new`/`class`/`this`/`import`/`async`. Dedup, count, sort etc. are pipe commands — help() lists them.",
}

// mcpToolDefs bridges discovered MCP tools into the OpenAI tool format. The MCP
// InputSchema is already a JSON Schema, so it drops straight into Parameters.
// Descriptions in toolDocs are appended so the model sees the essentials inline
// (see the toolDocs comment for why this beats the "call the prompt tool" path).
func mcpToolDefs(tools []mcpmgr.MCPTool) []llm.ToolDef {
	out := make([]llm.ToolDef, 0, len(tools))
	for _, t := range tools {
		var td llm.ToolDef
		td.Type = "function"
		td.Function.Name = t.Name
		td.Function.Description = t.Description + toolDocs[t.Name]
		td.Function.Parameters = t.InputSchema
		out = append(out, td)
	}
	return out
}

// mcpDispatcher routes a model tool call to the owning MCP server (by tool name).
// Errors meant for the model (unknown tool, bad args, tool failure) are formatted
// INTO the result so the loop stays alive.
func mcpDispatcher(mgr *mcpmgr.Manager, tools []mcpmgr.MCPTool, onCall func(tool string, args map[string]any, result string)) agent.ToolDispatcher {
	serverOf := make(map[string]string, len(tools))
	for _, t := range tools {
		serverOf[t.Name] = t.ServerID
	}
	return func(ctx context.Context, tc llm.ToolCall) (string, error) {
		serverID, ok := serverOf[tc.Function.Name]
		if !ok {
			return fmt.Sprintf("ERROR: unknown tool %q", tc.Function.Name), nil
		}
		var args map[string]any
		if s := strings.TrimSpace(tc.Function.Arguments); s != "" && s != "null" {
			if err := json.Unmarshal([]byte(s), &args); err != nil {
				// Name TRUNCATION explicitly when that is what it is. "bad
				// arguments: unexpected end of JSON input" reads like the model
				// wrote malformed JSON; in practice the far more common cause is
				// the provider cutting the stream mid-write, and the model's
				// correct response is different — retry SMALLER, not "fix your
				// syntax". A valid prefix that simply ends is the signature.
				if isTruncatedJSON(s) {
					return fmt.Sprintf("ERROR: malformed json — truncation detected: the arguments "+
						"were cut off after %d characters and this call was NOT executed. "+
						"Retry with a smaller call (split large writes into successive edits).",
						len(s)), nil
				}
				return fmt.Sprintf("ERROR: malformed json: %v", err), nil
			}
		}
		res, err := mgr.CallTool(ctx, serverID, tc.Function.Name, args)
		if err != nil {
			res = fmt.Sprintf("ERROR: %v", err)
		}
		if onCall != nil {
			onCall(tc.Function.Name, args, res)
		}
		return res, nil
	}
}

// isTruncatedJSON reports whether s looks CUT OFF rather than mis-written: a
// valid prefix that simply ends. json.Decoder distinguishes the two — a
// truncated document yields io.ErrUnexpectedEOF, while genuinely bad syntax
// (a stray comma, an unquoted key) yields a SyntaxError at the offending byte.
//
// The distinction changes the advice given to the model, so it is worth making
// rather than reporting every parse failure identically.
func isTruncatedJSON(s string) bool {
	var v json.RawMessage
	err := json.NewDecoder(strings.NewReader(s)).Decode(&v)
	if err == nil {
		return false // a complete value; whatever failed, it was not truncation
	}
	// Mis-written JSON fails AT a byte with a SyntaxError. Truncated JSON runs
	// out of input: mid-string gives ErrUnexpectedEOF, and ending on a clean
	// token boundary ({"a": or {) gives EOF. Both mean "cut off".
	var se *json.SyntaxError
	if errors.As(err, &se) {
		return false
	}
	return errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF)
}
