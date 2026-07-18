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

// mcpToolDefs bridges discovered MCP tools into the OpenAI tool format. The MCP
// InputSchema is already a JSON Schema, so it drops straight into Parameters.
func mcpToolDefs(tools []mcpmgr.MCPTool) []llm.ToolDef {
	out := make([]llm.ToolDef, 0, len(tools))
	for _, t := range tools {
		var td llm.ToolDef
		td.Type = "function"
		td.Function.Name = t.Name
		td.Function.Description = t.Description
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
