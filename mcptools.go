package dun

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/iodesystems/agentkit/mcpmgr"
)

// Per-tool cost and on/off.
//
// A server with many tools is opaque: its schemas ride every request and the
// only control was a deny-list nothing showed. This lists every tool a server
// discovered — the hidden ones too, since mcpmgr discovers all of them and
// filterTools only hides — with what each costs, and turns one on or off by
// editing the server's Disable list where the server was defined.

// ToolInfo is one tool of a running server, for a UI.
type ToolInfo struct {
	Server      string
	Name        string
	Description string
	Schema      string // the input schema, indented JSON
	Tokens      int    // what its definition costs in every request
	Enabled     bool
	HiddenBy    string // the Disable entry that hides it, when one does
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
		return nil, fmt.Errorf("%s runs inside the shell, which lists its tools itself: help(%q) in eval — /mcp move %s direct to manage them here",
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
		by := hiddenBy(t.Name, s.Disable)
		out = append(out, ToolInfo{Server: id, Name: t.Name, Description: t.Description, Schema: string(schema),
			Tokens: h.toolTokens(t), Enabled: by == "", HiddenBy: by})
	}
	return out, nil
}

// SetToolEnabled turns one tool of a server on or off by editing its Disable
// list, and rebuilds the tool set at the next turn boundary. It returns the
// file written, "" when nothing changed.
//
// The list is written where the server was defined: the user server list for
// one /mcp add saved, else this project's machine state (dun.local.json under
// $DUN_HOME/projects/<id>/), which overrides a repo's dun.json — dun never
// writes into a repo.
func (h *Harness) SetToolEnabled(id, tool string, on bool) (string, error) {
	tools, err := h.ServerTools(id)
	if err != nil {
		return "", err
	}
	i := slices.IndexFunc(tools, func(t ToolInfo) bool { return t.Name == tool })
	if i < 0 {
		return "", fmt.Errorf("%s has no tool %q", id, tool)
	}
	if tools[i].Enabled == on {
		return "", nil
	}
	h.srvMu.Lock()
	s, _ := h.spec(id)
	h.srvMu.Unlock()
	next := []string{} // non-nil: an emptied list must still override (ServerSpec.MarshalJSON)
	if on {
		for _, p := range s.Disable {
			if p != tool {
				next = append(next, p)
			}
		}
		if by := hiddenBy(tool, next); by != "" {
			return "", fmt.Errorf("%s is hidden by the pattern %q — narrow or remove it in the server's \"disable\"", tool, by)
		}
	} else {
		next = append(append(next, s.Disable...), tool)
	}
	path := UserServersFile()
	found, err := SetUserServerDisable(id, next)
	if err != nil {
		return "", err
	}
	if !found {
		if path, err = SetLocalDisable(h.configDir(), id, next); err != nil {
			return "", err
		}
	}
	h.srvMu.Lock()
	for i := range h.specs {
		if h.specs[i].ID == id {
			h.specs[i].Disable = next
		}
	}
	h.srvMu.Unlock()
	h.applyTools()
	return path, nil
}
