package harness

import (
	"context"
	"strings"
	"testing"

	"github.com/iodesystems/dun/internal/mcpserver"
)

// SetServerInstance writes the scope, and refuses built-ins, dun's own tools,
// scripted+agent and global unset.
func TestSetServerInstance(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ts := fakeMCPTools(t, "a")
	h := newServerHarness(t)
	ctx := context.Background()
	if _, err := h.AddServer(ctx, mcpserver.Spec{ID: "el", URL: ts.URL}); err != nil {
		t.Fatal(err)
	}
	state := func() ServerState {
		for _, st := range h.Servers() {
			if st.ID == "el" {
				return st
			}
		}
		t.Fatal("no el")
		return ServerState{}
	}
	if st := state(); st.Instance != mcpserver.InstanceShared || st.InstanceFrom != mcpserver.ScopeGlobal {
		t.Fatalf("default: %+v", st)
	}
	if _, err := h.SetServerInstance(ctx, "el", mcpserver.ScopeSession, mcpserver.InstanceAgent); err != nil {
		t.Fatal(err)
	}
	if st := state(); st.Instance != mcpserver.InstanceAgent || st.InstanceSession != mcpserver.InstanceAgent || st.InstanceFrom != mcpserver.ScopeSession {
		t.Fatalf("session agent: %+v", st)
	}
	if got := mcpserver.LoadSessionServers(h.cfg.SessionFile)["el"].Instance; h.cfg.SessionFile != "" && got != mcpserver.InstanceAgent {
		t.Errorf("sidecar: %q", got)
	}
	if _, err := h.SetServerVia(ctx, "el", mcpserver.ScopeSession, mcpserver.ViaShell); err == nil {
		t.Error("scripted with agent must be refused")
	}
	if _, err := h.SetServerInstance(ctx, "el", mcpserver.ScopeSession, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := h.SetServerInstance(ctx, "el", mcpserver.ScopeGlobal, mcpserver.InstanceSession); err != nil {
		t.Fatal(err)
	}
	if f, _ := mcpserver.ReadConfig(mcpserver.UserServersFile()); f == nil || f.Servers[0].Instance != mcpserver.InstanceSession {
		t.Errorf("global not written: %+v", f)
	}
	if _, err := h.SetServerInstance(ctx, "el", mcpserver.ScopeGlobal, mcpserver.InstanceShared); err != nil {
		t.Fatal(err)
	}
	if f, _ := mcpserver.ReadConfig(mcpserver.UserServersFile()); f.Servers[0].Instance != "" {
		t.Errorf("global shared is stored as its absence: %q", f.Servers[0].Instance)
	}
	if _, err := h.SetServerInstance(ctx, "el", mcpserver.ScopeGlobal, ""); err == nil {
		t.Error("global has no unset")
	}
	if _, err := h.SetServerVia(ctx, "el", mcpserver.ScopeSession, mcpserver.ViaShell); err != nil {
		t.Fatal(err)
	}
	if _, err := h.SetServerInstance(ctx, "el", mcpserver.ScopeSession, mcpserver.InstanceAgent); err == nil {
		t.Error("agent for a scripted server must be refused")
	}
	if _, err := h.SetServerInstance(ctx, mcpserver.ServerCode, mcpserver.ScopeSession, mcpserver.InstanceSession); err == nil || !strings.Contains(err.Error(), "fixed") {
		t.Errorf("a built-in: %v", err)
	}
	if _, err := h.SetServerInstance(ctx, "el", mcpserver.ScopeSession, "thread"); err == nil {
		t.Error("an unknown instance")
	}
}
