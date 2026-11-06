package dun

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// A scripted server that is shared: its shell-mcp.json entry is the
// gateway's endpoint with the shell's bearer, no auth_cmd and no login of
// the engine's; the next shell start gives the last shell's bearer back.
func TestShellConfig_SharedHostedIsTheGateway(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ts, _ := countingMCP(t)
	w := newOAuthWorld(t)
	g := newGateway(t)
	shell := Server{ID: ServerShell, Command: "mcpshell", Args: []string{"mcp"}}
	h := gatewayHarness(t, g, "s1", nil,
		shell,
		Server{ID: "ctr", URL: ts.URL, Via: ViaShell, Autostart: true, Instance: InstanceShared},
		Server{ID: "own", URL: ts.URL + "/own", Via: ViaShell, Autostart: true, Instance: InstanceSession,
			OAuth: &OAuthSpec{ClientID: w.clientID}})
	h.cfg.ConfigDir = h.cfg.Workspace
	ctx := context.Background()
	h.shareHosted(ctx, ServerShell, &startPhase{})
	read := func() map[string]mcpshellServer {
		t.Helper()
		args := h.shellArgs(shell)
		i := slices.Index(args, "--mcp")
		if i < 0 {
			t.Fatalf("no --mcp: %v", args)
		}
		b, err := os.ReadFile(args[i+1])
		if err != nil {
			t.Fatal(err)
		}
		var f struct {
			MCPServers map[string]mcpshellServer `json:"mcpServers"`
		}
		if err := json.Unmarshal(b, &f); err != nil {
			t.Fatal(err)
		}
		return f.MCPServers
	}
	got := read()
	first := h.shellShares["ctr"]
	if e := got["ctr"]; e.URL != first.URL || e.Headers["Authorization"] != "Bearer "+first.Bearer || e.AuthCmd != "" {
		t.Errorf("shared entry: %+v", e)
	}
	if e := got["own"]; e.URL != ts.URL+"/own" || e.AuthCmd == "" {
		t.Errorf("a session instance stays the shell's own: %+v", e)
	}
	if g.Clients(first.Key) != 1 {
		t.Fatalf("clients: %d", g.Clients(first.Key))
	}
	h.shareHosted(ctx, ServerShell, &startPhase{}) // the shell restarts
	if next := h.shellShares["ctr"]; next.Bearer == first.Bearer || g.Clients(first.Key) != 1 {
		t.Errorf("restart: the old bearer kept (%v) or not given back (%d clients)", next.Bearer == first.Bearer, g.Clients(first.Key))
	}
}

// The real mcpshell reaches a shared server through the gateway.
func TestShell_SharedHostedLive(t *testing.T) {
	if _, err := exec.LookPath("mcpshell"); err != nil {
		t.Skip("no mcpshell on PATH")
	}
	t.Setenv("DUN_HOME", t.TempDir())
	ts, inits := countingMCP(t)
	g := newGateway(t)
	shell := Server{ID: ServerShell, Command: "mcpshell", Args: []string{"mcp"}, Autostart: true}
	h := gatewayHarness(t, g, "s1", nil, shell, Server{ID: "ctr", URL: ts.URL, Via: ViaShell, Autostart: true, Instance: InstanceShared})
	h.cfg.ConfigDir = h.cfg.Workspace
	ctx := context.Background()
	if err := h.startServer(ctx, shell); err != nil {
		t.Fatal(err)
	}
	out, err := h.mgr.CallTool(ctx, ServerShell, "eval", map[string]any{"code": `ctr.echo({x: "through"})`})
	if err != nil || !strings.Contains(out, "echo through") {
		t.Fatalf("eval: %q %v\n%s", out, err, h.mgr.ServerStderr(ServerShell))
	}
	if inits.Load() != 1 || g.Starts() != 1 {
		t.Errorf("handshakes %d, starts %d", inits.Load(), g.Starts())
	}
}
