package harness

import (
	"context"
	"strings"
	"testing"

	"github.com/iodesystems/dun/internal/mcpserver"
)

// A global standalone is stored as "direct": absence would mean the default.
func TestSetServerVia_GlobalStandaloneIsExplicit(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ts := fakeMCPTools(t, "a")
	h := newServerHarness(t)
	ctx := context.Background()
	if _, err := h.AddServer(ctx, mcpserver.Spec{ID: "el", URL: ts.URL}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.SetServerVia(ctx, "el", mcpserver.ScopeGlobal, mcpserver.ViaDirect); err != nil {
		t.Fatal(err)
	}
	f, _ := mcpserver.ReadConfig(mcpserver.UserServersFile())
	if f.Servers[0].Via != mcpserver.ViaDirect {
		t.Errorf("stored %q, want %q", f.Servers[0].Via, mcpserver.ViaDirect)
	}
	// Re-adding keeps it.
	if _, err := h.AddServer(ctx, mcpserver.Spec{ID: "el", URL: ts.URL}); err != nil {
		t.Fatal(err)
	}
	if f, _ = mcpserver.ReadConfig(mcpserver.UserServersFile()); f.Servers[0].Via != mcpserver.ViaDirect {
		t.Errorf("re-add dropped it: %q", f.Servers[0].Via)
	}
	if _, err := h.SetDefaultMode(ctx, "bogus"); err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Errorf("bad default: %v", err)
	}
}

// An explicit standalone in a re-add replaces a stored scripted; an
// unspecified mode keeps it. Global unset clears the mode: the default
// decides again. A re-add that would store scripted with agent is refused.
func TestAddServer_ExplicitStandaloneOverScripted(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ts := fakeMCP(t, "tok")
	defer ts.Close()
	h := newServerHarness(t)
	ctx := context.Background()
	spec := mcpserver.Spec{ID: "corp", URL: ts.URL, Bearer: "tok"}
	via := func() string {
		f, _ := mcpserver.ReadConfig(mcpserver.UserServersFile())
		return f.Servers[0].Via
	}
	scripted := spec
	scripted.Via = mcpserver.ViaShell
	if _, err := h.AddServer(ctx, scripted); err != nil {
		t.Fatal(err)
	}
	if _, err := h.AddServer(ctx, spec); err != nil || via() != mcpserver.ViaShell {
		t.Fatalf("unspecified keeps scripted: %q %v", via(), err)
	}
	// The conflict is checked on what will be stored.
	agent := spec
	agent.Instance = mcpserver.InstanceAgent
	if _, err := h.AddServer(ctx, agent); err == nil || !strings.Contains(err.Error(), "instance agent") {
		t.Errorf("merged shell+agent must be refused: %v", err)
	}
	standalone := spec
	standalone.Via = "standalone"
	if _, err := h.AddServer(ctx, standalone); err != nil || via() != mcpserver.ViaDirect {
		t.Fatalf("explicit standalone: %q %v", via(), err)
	}
	if _, err := h.SetServerVia(ctx, "corp", mcpserver.ScopeGlobal, ""); err != nil || via() != "" {
		t.Errorf("unset at global: %q %v", via(), err)
	}
}

// Under the scripted default, instance agent is allowed: the server stays
// standalone. A mode set at a scope still refuses it.
func TestSetServerInstance_AgentUnderScriptedDefault(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ts := fakeMCP(t, "tok")
	defer ts.Close()
	h := newServerHarness(t)
	ctx := context.Background()
	if _, err := h.AddServer(ctx, mcpserver.Spec{ID: "corp", URL: ts.URL, Bearer: "tok"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.SetDefaultMode(ctx, mcpserver.ViaShell); err != nil {
		t.Fatal(err)
	}
	state := func() ServerState {
		for _, st := range h.Servers() {
			if st.ID == "corp" {
				return st
			}
		}
		t.Fatal("no corp")
		return ServerState{}
	}
	if st := state(); st.Via != mcpserver.ViaShell || st.ViaFrom != mcpserver.ViaFromDefault {
		t.Fatalf("default scripted: %+v", st)
	}
	if _, err := h.SetServerInstance(ctx, "corp", mcpserver.ScopeSession, mcpserver.InstanceAgent); err != nil {
		t.Fatalf("agent under the default: %v", err)
	}
	if st := state(); st.Via != mcpserver.ViaDirect || st.ViaFrom != mcpserver.ViaFromDefaultAgent || st.Instance != mcpserver.InstanceAgent {
		t.Errorf("standalone, and why: %+v", st)
	}
	if _, err := h.SetServerVia(ctx, "corp", mcpserver.ScopeSession, mcpserver.ViaShell); err == nil {
		t.Error("an explicit scripted with agent is refused")
	}
	if _, err := h.SetServerInstance(ctx, "corp", mcpserver.ScopeSession, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := h.SetServerVia(ctx, "corp", mcpserver.ScopeSession, mcpserver.ViaShell); err != nil {
		t.Fatal(err)
	}
	if _, err := h.SetServerInstance(ctx, "corp", mcpserver.ScopeProject, mcpserver.InstanceAgent); err == nil {
		t.Error("agent over an explicit scripted is refused")
	}
}
