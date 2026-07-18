package dun

import (
	"context"
	"encoding/json"
	"fmt"
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
				return fmt.Sprintf("ERROR: bad arguments: %v", err), nil
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
