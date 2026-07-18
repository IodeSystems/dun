package dun

import (
	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/mcpmgr"
	"github.com/iodesystems/agentkit/ragnotify"
)

// Proactive notifications — wiring raglit's search tool as an agent.DocFinder so
// agentkit's FinderPreparer can ping relevant docs before each turn (the
// ragnotify pattern, over an MCP server instead of a Go call). The finder reuses
// the SAME raglit server the model searches explicitly.

// docsFinder returns a DocFinder over the discovered raglit `search` tool, or nil
// if raglit isn't in the tool set.
func docsFinder(mgr *mcpmgr.Manager, tools []mcpmgr.MCPTool) agent.DocFinder {
	for _, t := range tools {
		if t.Name == "search" {
			return ragnotify.MCPFinder(mgr, t.ServerID, "search", ragnotify.Opts{})
		}
	}
	return nil
}
