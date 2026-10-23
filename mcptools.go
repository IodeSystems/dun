package dun

import (
	"encoding/json"
	"fmt"

	"github.com/iodesystems/agentkit/mcpmgr"
)

// Per-tool cost and on/off.
//
// A server with many tools is opaque: its schemas ride every request and the
// only control was a deny-list nothing showed. This lists every tool a server
// discovered — the hidden ones too, since mcpmgr discovers all of them and
// filterTools only hides — with what each costs, and turns one on or off at
// a scope (SetToolOn, mcpscope.go).

// ToolInfo is one tool of a running server, for a UI.
type ToolInfo struct {
	Server      string
	Name        string
	Description string
	Schema      string // the input schema, indented JSON
	Tokens      int    // what its definition costs in every request
	Enabled     bool
	HiddenBy    string // the Disable entry that hides it, when one does
	// The switch at each scope ("on", "off", "" unset), and the scope that
	// decided Enabled.
	Global, Project, Session, From string
}

// HeavyServerTokens is where a server's schemas are worth a hint: hide some
// tools, or move the server into the shell.
const HeavyServerTokens = 5000

// toolTokens is what one tool's definition costs: its wire JSON, as the model
// receives it, priced by the meter calibrated on this provider's own usage
// (tokencal.go) — the same ratio every other estimate in a session uses.
func (h *Harness) toolTokens(t mcpmgr.MCPTool) int {
	b, err := json.Marshal(mcpToolDefs([]mcpmgr.MCPTool{t})[0])
	if err != nil {
		return 0
	}
	return h.meter.Estimate(string(b))
}

// ServerTools lists every tool a running, directly connected server
// discovered, hidden ones included, in discovery order.
func (h *Harness) ServerTools(id string) ([]ToolInfo, error) {
	h.srvMu.Lock()
	s, ok := h.spec(id)
	h.srvMu.Unlock()
	switch {
	case !ok:
		return nil, fmt.Errorf("no server %q", id)
	case s.Via == ViaShell:
		return nil, fmt.Errorf("%s runs inside the shell, which lists its tools itself: help(%q) in eval — /mcp via %s direct to manage them here",
			id, ShellNamespace(id), id)
	case !h.mgr.ServerStarted(id):
		return nil, fmt.Errorf("%s is not running — /mcp on %s first", id, id)
	}
	var out []ToolInfo
	for _, t := range h.mgr.GetTools() {
		if t.ServerID != id {
			continue
		}
		schema, _ := json.MarshalIndent(t.InputSchema, "", "  ")
		on, from, by := s.toolState(t.Name)
		g, p, ss := s.toolScopes(t.Name)
		out = append(out, ToolInfo{Server: id, Name: t.Name, Description: t.Description, Schema: string(schema),
			Tokens: h.toolTokens(t), Enabled: on, HiddenBy: by, Global: g, Project: p, Session: ss, From: from})
	}
	return out, nil
}
