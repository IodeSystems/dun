package harness

import (
	"context"
	"strings"
	"testing"
)

// Re-adding a server replaces its definition but keeps the mode and instance
// it had: they are switches, and the add form does not ask for them.
func TestPutUserServer_KeepsModeAndInstance(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	if _, err := PutUserServer(ServerSpec{ID: "corp", Command: "corp-mcp", Via: ViaShell, Instance: InstanceSession}); err != nil {
		t.Fatal(err)
	}
	if _, err := PutUserServer(ServerSpec{ID: "corp", Command: "corp-mcp2"}); err != nil {
		t.Fatal(err)
	}
	f, _ := readServersFile(UserServersFile())
	if len(f.Servers) != 1 || f.Servers[0].Command != "corp-mcp2" || f.Servers[0].Via != ViaShell || f.Servers[0].Instance != InstanceSession {
		t.Errorf("re-add: %+v", f.Servers)
	}
	// A value the new spec names wins.
	if _, err := PutUserServer(ServerSpec{ID: "corp", Command: "corp-mcp2", Via: ViaDirect, Instance: InstanceShared}); err != nil {
		t.Fatal(err)
	}
	f, _ = readServersFile(UserServersFile())
	if f.Servers[0].Via != ViaDirect || f.Servers[0].Instance != InstanceShared {
		t.Errorf("explicit: %+v", f.Servers)
	}
}

// The default mode is read from mcp.json; a server no scope sets a mode for
// takes it, and says so. A scope's value, a built-in, and instance agent all
// beat it.
func TestResolve_DefaultVia(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	dir := t.TempDir()
	for _, id := range []string{"a", "b", "c", "d"} {
		if _, err := PutUserServer(ServerSpec{ID: id, Command: id + "-mcp", Autostart: boolp(true)}); err != nil {
			t.Fatal(err)
		}
	}
	get := func(sess map[string]SessionServer) map[string]Server {
		t.Helper()
		got, err := resolveServers(dir, dir, "", DefaultServers(dir, ""), sess, nil)
		if err != nil {
			t.Fatal(err)
		}
		return byID(got)
	}
	if v, err := LoadDefaultVia(); err != nil || v != ViaDirect {
		t.Fatalf("unset default: %q %v", v, err)
	}
	if s := get(nil)["a"]; s.Via != "" || s.Scopes.ViaFrom != ScopeGlobal {
		t.Fatalf("standalone default: %q from %q", s.Via, s.Scopes.ViaFrom)
	}
	if _, err := SetDefaultVia("sideways"); err == nil {
		t.Error("a bad default is refused")
	}
	if _, err := SetDefaultVia(ViaShell); err != nil {
		t.Fatal(err)
	}
	// An explicit global standalone (stored as "direct") beats the default;
	// a project's scripted and a session's standalone beat it too.
	if _, err := UpsertUserServer("b", func(s *ServerSpec) { s.Via = ViaDirect }); err != nil {
		t.Fatal(err)
	}
	if _, err := updateLocalServer(dir, "c", func(s *ServerSpec) { s.Via = ViaShell }); err != nil {
		t.Fatal(err)
	}
	if _, err := UpsertUserServer("d", func(s *ServerSpec) { s.Instance = InstanceAgent }); err != nil {
		t.Fatal(err)
	}
	all := get(map[string]SessionServer{"c": {Via: ViaDirect}})
	for _, c := range []struct{ id, via, from string }{
		{"a", ViaShell, ViaFromDefault},
		{"b", "", ScopeGlobal},
		{"c", "", ScopeSession},
		{"d", "", ViaFromDefaultAgent}, // agent cannot be scripted: standalone, and the cell says why
		{ServerShell, "", ""},
		{ServerCode, "", ""},
	} {
		if s := all[c.id]; s.Via != c.via || s.Scopes.ViaFrom != c.from {
			t.Errorf("%s: via %q from %q, want %q from %q", c.id, s.Via, s.Scopes.ViaFrom, c.via, c.from)
		}
	}
	if s := all["d"]; s.Instance != InstanceAgent {
		t.Errorf("d keeps its instance: %q", s.Instance)
	}
	if _, err := SetDefaultVia(ViaDirect); err != nil {
		t.Fatal(err)
	}
	if s := get(nil)["a"]; s.Via != "" || s.Scopes.ViaFrom != ScopeGlobal {
		t.Errorf("back to standalone: %q from %q", s.Via, s.Scopes.ViaFrom)
	}
}

// A global standalone is stored as "direct": absence would mean the default.
func TestSetServerVia_GlobalStandaloneIsExplicit(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ts := fakeMCPTools(t, "a")
	h := newServerHarness(t)
	ctx := context.Background()
	if _, err := h.AddServer(ctx, ServerSpec{ID: "el", URL: ts.URL}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.SetServerVia(ctx, "el", ScopeGlobal, ViaDirect); err != nil {
		t.Fatal(err)
	}
	f, _ := readServersFile(UserServersFile())
	if f.Servers[0].Via != ViaDirect {
		t.Errorf("stored %q, want %q", f.Servers[0].Via, ViaDirect)
	}
	// Re-adding keeps it.
	if _, err := h.AddServer(ctx, ServerSpec{ID: "el", URL: ts.URL}); err != nil {
		t.Fatal(err)
	}
	if f, _ = readServersFile(UserServersFile()); f.Servers[0].Via != ViaDirect {
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
	spec := ServerSpec{ID: "corp", URL: ts.URL, Bearer: "tok"}
	via := func() string {
		f, _ := readServersFile(UserServersFile())
		return f.Servers[0].Via
	}
	scripted := spec
	scripted.Via = ViaShell
	if _, err := h.AddServer(ctx, scripted); err != nil {
		t.Fatal(err)
	}
	if _, err := h.AddServer(ctx, spec); err != nil || via() != ViaShell {
		t.Fatalf("unspecified keeps scripted: %q %v", via(), err)
	}
	// The conflict is checked on what will be stored.
	agent := spec
	agent.Instance = InstanceAgent
	if _, err := h.AddServer(ctx, agent); err == nil || !strings.Contains(err.Error(), "instance agent") {
		t.Errorf("merged shell+agent must be refused: %v", err)
	}
	standalone := spec
	standalone.Via = "standalone"
	if _, err := h.AddServer(ctx, standalone); err != nil || via() != ViaDirect {
		t.Fatalf("explicit standalone: %q %v", via(), err)
	}
	if _, err := h.SetServerVia(ctx, "corp", ScopeGlobal, ""); err != nil || via() != "" {
		t.Errorf("unset at global: %q %v", via(), err)
	}
}

// A bad default_via reads as standalone and takes no server down; setting
// one by command is still refused.
func TestResolve_BadDefaultViaFallsBack(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	dir := t.TempDir()
	if _, err := PutUserServer(ServerSpec{ID: "a", Command: "a-mcp", Autostart: boolp(true)}); err != nil {
		t.Fatal(err)
	}
	f, _ := readServersFile(UserServersFile())
	f.DefaultVia = "sideways"
	if err := writeUserServers(UserServersFile(), f); err != nil {
		t.Fatal(err)
	}
	got, err := resolveServers(dir, dir, "", DefaultServers(dir, ""), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s := byID(got)["a"]; s.Via != "" || s.Scopes.ViaFrom != ScopeGlobal {
		t.Errorf("fallback: %q from %q", s.Via, s.Scopes.ViaFrom)
	}
	if _, err := SetDefaultVia("sideways"); err == nil {
		t.Error("a bad default is refused when set")
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
	if _, err := h.AddServer(ctx, ServerSpec{ID: "corp", URL: ts.URL, Bearer: "tok"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.SetDefaultMode(ctx, ViaShell); err != nil {
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
	if st := state(); st.Via != ViaShell || st.ViaFrom != ViaFromDefault {
		t.Fatalf("default scripted: %+v", st)
	}
	if _, err := h.SetServerInstance(ctx, "corp", ScopeSession, InstanceAgent); err != nil {
		t.Fatalf("agent under the default: %v", err)
	}
	if st := state(); st.Via != ViaDirect || st.ViaFrom != ViaFromDefaultAgent || st.Instance != InstanceAgent {
		t.Errorf("standalone, and why: %+v", st)
	}
	if _, err := h.SetServerVia(ctx, "corp", ScopeSession, ViaShell); err == nil {
		t.Error("an explicit scripted with agent is refused")
	}
	if _, err := h.SetServerInstance(ctx, "corp", ScopeSession, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := h.SetServerVia(ctx, "corp", ScopeSession, ViaShell); err != nil {
		t.Fatal(err)
	}
	if _, err := h.SetServerInstance(ctx, "corp", ScopeProject, InstanceAgent); err == nil {
		t.Error("agent over an explicit scripted is refused")
	}
}
