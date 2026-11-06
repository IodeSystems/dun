package dun

import (
	"context"
	"strings"
	"testing"
)

// The instance switch resolves as the mode does: session, then project,
// then global; unset everywhere is shared. A built-in's is fixed.
func TestResolve_Instance(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	dir := t.TempDir()
	if _, err := PutUserServer(ServerSpec{ID: "x", Command: "x-mcp", Autostart: boolp(true)}); err != nil {
		t.Fatal(err)
	}
	get := func(sess map[string]SessionServer) map[string]Server {
		t.Helper()
		got, err := resolveServers(dir, dir, "", DefaultServers(dir, ""), sess, nil)
		if err != nil {
			t.Fatal(err)
		}
		return byID(got)
	}
	if s := get(nil)["x"]; s.Instance != InstanceShared || s.Scopes.InstanceFrom != ScopeGlobal {
		t.Fatalf("default: %q from %q", s.Instance, s.Scopes.InstanceFrom)
	}
	if _, err := updateLocalServer(dir, "x", func(s *ServerSpec) { s.Instance = InstanceAgent }); err != nil {
		t.Fatal(err)
	}
	if s := get(nil)["x"]; s.Instance != InstanceAgent || s.Scopes.InstanceFrom != ScopeProject {
		t.Fatalf("project: %q from %q", s.Instance, s.Scopes.InstanceFrom)
	}
	sess := map[string]SessionServer{"x": {Instance: InstanceSession}}
	if s := get(sess)["x"]; s.Instance != InstanceSession || s.Scopes.InstanceFrom != ScopeSession || s.Scopes.InstanceProject != InstanceAgent {
		t.Fatalf("session: %q %+v", s.Instance, s.Scopes)
	}
	// Scripted with agent runs as session: a child has no shell of its own.
	if s := get(map[string]SessionServer{"x": {Via: ViaShell}})["x"]; s.Instance != InstanceSession {
		t.Fatalf("scripted agent: %q", s.Instance)
	}
	all := get(nil)
	for id, want := range map[string]string{ServerShell: InstanceSession, ServerCode: InstanceWorkspace, ServerDocs: InstanceShared} {
		if s := all[id]; s.Instance != want || s.Scopes.InstanceFrom != "" {
			t.Errorf("%s: %q from %q, want %q fixed", id, s.Instance, s.Scopes.InstanceFrom, want)
		}
	}
	if _, err := resolveServers(dir, dir, "", DefaultServers(dir, ""), map[string]SessionServer{ServerShell: {Instance: InstanceAgent}}, nil); err == nil || !strings.Contains(err.Error(), "fixed") {
		t.Errorf("a built-in's instance set: %v", err)
	}
	if _, err := NormalizeInstance("thread"); err == nil {
		t.Error("an unknown instance is refused")
	}
}

// SetServerInstance writes the scope, and refuses built-ins, dun's own tools,
// scripted+agent and global unset.
func TestSetServerInstance(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ts := fakeMCPTools(t, "a")
	h := newServerHarness(t)
	ctx := context.Background()
	if _, err := h.AddServer(ctx, ServerSpec{ID: "el", URL: ts.URL}); err != nil {
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
	if st := state(); st.Instance != InstanceShared || st.InstanceFrom != ScopeGlobal {
		t.Fatalf("default: %+v", st)
	}
	if _, err := h.SetServerInstance(ctx, "el", ScopeSession, InstanceAgent); err != nil {
		t.Fatal(err)
	}
	if st := state(); st.Instance != InstanceAgent || st.InstanceSession != InstanceAgent || st.InstanceFrom != ScopeSession {
		t.Fatalf("session agent: %+v", st)
	}
	if got := LoadSessionServers(h.cfg.SessionFile)["el"].Instance; h.cfg.SessionFile != "" && got != InstanceAgent {
		t.Errorf("sidecar: %q", got)
	}
	if _, err := h.SetServerVia(ctx, "el", ScopeSession, ViaShell); err == nil {
		t.Error("scripted with agent must be refused")
	}
	if _, err := h.SetServerInstance(ctx, "el", ScopeSession, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := h.SetServerInstance(ctx, "el", ScopeGlobal, InstanceSession); err != nil {
		t.Fatal(err)
	}
	if f, _ := readServersFile(UserServersFile()); f == nil || f.Servers[0].Instance != InstanceSession {
		t.Errorf("global not written: %+v", f)
	}
	if _, err := h.SetServerInstance(ctx, "el", ScopeGlobal, InstanceShared); err != nil {
		t.Fatal(err)
	}
	if f, _ := readServersFile(UserServersFile()); f.Servers[0].Instance != "" {
		t.Errorf("global shared is stored as its absence: %q", f.Servers[0].Instance)
	}
	if _, err := h.SetServerInstance(ctx, "el", ScopeGlobal, ""); err == nil {
		t.Error("global has no unset")
	}
	if _, err := h.SetServerVia(ctx, "el", ScopeSession, ViaShell); err != nil {
		t.Fatal(err)
	}
	if _, err := h.SetServerInstance(ctx, "el", ScopeSession, InstanceAgent); err == nil {
		t.Error("agent for a scripted server must be refused")
	}
	if _, err := h.SetServerInstance(ctx, ServerCode, ScopeSession, InstanceSession); err == nil || !strings.Contains(err.Error(), "fixed") {
		t.Errorf("a built-in: %v", err)
	}
	if _, err := h.SetServerInstance(ctx, "el", ScopeSession, "thread"); err == nil {
		t.Error("an unknown instance")
	}
}
