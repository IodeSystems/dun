package harness

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/iodesystems/dun/internal/mcpserver"
)

func boolp(b bool) *bool { return &b }

// A session's switches survive the process: a fresh Start on the same
// session file reads them back.
func TestSessionSwitches_PersistAcrossResume(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ts := fakeMCPTools(t, "a")
	file := filepath.Join(t.TempDir(), "s.jsonl")
	cfg := Config{Workspace: t.TempDir(), SessionFile: file, Servers: []mcpserver.Server{{ID: "el", URL: ts.URL, Autostart: true}}}
	ctx := context.Background()
	h, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	path, err := h.SetServerOn(ctx, "el", mcpserver.ScopeSession, boolp(false))
	if err != nil || path != mcpserver.SessionServersFile(file) {
		t.Fatalf("%q %v", path, err)
	}
	if h.mgr.ServerStarted("el") {
		t.Error("off for the session, still running")
	}
	h.Close()
	if _, err := os.Stat(mcpserver.SessionServersFile(file)); err != nil {
		t.Fatal(err)
	}

	h, err = Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	st, _ := serverState(h, "el")
	if st.Auto || st.Session != "off" || st.From != mcpserver.ScopeSession || st.Global != "on" {
		t.Fatalf("resumed: %+v", st)
	}
	if _, err := h.SetServerOn(ctx, "el", mcpserver.ScopeSession, nil); err != nil {
		t.Fatal(err)
	}
	if !h.mgr.ServerStarted("el") {
		t.Error("unset at session: the global on applies and it starts")
	}
}

// A running session follows a global or project change at its next turn,
// unless its own switch is set.
func TestFollowServers_GlobalChangeReachesARunningSession(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ts := fakeMCPTools(t, "a")
	h := newServerHarness(t)
	ctx := context.Background()
	if _, err := h.AddServer(ctx, mcpserver.Spec{ID: "el", URL: ts.URL}); err != nil {
		t.Fatal(err)
	}
	if !h.mgr.ServerStarted("el") {
		t.Fatal("added: not running")
	}
	// Another session turns it off globally: this one's file is all it sees.
	if _, err := mcpserver.UpsertUserServer("el", func(s *mcpserver.Spec) { s.Autostart = boolp(false) }); err != nil {
		t.Fatal(err)
	}
	h.followServers(ctx)
	if h.mgr.ServerStarted("el") || slices.Contains(modelTools(h), "el__a") {
		t.Fatalf("global off not followed: %v", modelTools(h))
	}
	// Its own switch on: a global change no longer moves it.
	if _, err := h.SetServerOn(ctx, "el", mcpserver.ScopeSession, boolp(true)); err != nil || !h.mgr.ServerStarted("el") {
		t.Fatalf("session on: %v", err)
	}
	if _, err := mcpserver.UpsertUserServer("el", func(s *mcpserver.Spec) { s.Autostart = boolp(false) }); err != nil {
		t.Fatal(err)
	}
	h.followServers(ctx)
	if !h.mgr.ServerStarted("el") {
		t.Error("the session switch must hold over the global one")
	}
	// Placement too: a session switch to the shell stops the direct one.
	if _, err := h.SetServerVia(ctx, "el", mcpserver.ScopeSession, mcpserver.ViaShell); err != nil {
		t.Fatal(err)
	}
	if h.mgr.ServerStarted("el") {
		t.Error("in the shell, but still started directly")
	}
	if _, err := h.SetServerVia(ctx, "el", mcpserver.ScopeSession, ""); err != nil || !h.mgr.ServerStarted("el") {
		t.Errorf("via unset: back to direct and started: %v", err)
	}
	if _, err := h.SetServerVia(ctx, mcpserver.ServerCode, mcpserver.ScopeSession, mcpserver.ViaShell); err == nil {
		t.Error("a built-in has no placement")
	}
	if _, err := h.SetServerOn(ctx, "el", mcpserver.ScopeGlobal, nil); err == nil {
		t.Error("global has no unset")
	}
}

// A write of the global server file says so (the engine under dund tells the
// owner's other sessions), and an idle session re-reads at once; one inside a
// turn is left to its next turn, which re-reads at its start.
func TestFollowServersIfIdle_ReachesAnIdleSessionNow(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ts := fakeMCPTools(t, "a")
	h := newServerHarness(t)
	ctx := context.Background()
	if _, err := h.AddServer(ctx, mcpserver.Spec{ID: "el", URL: ts.URL}); err != nil {
		t.Fatal(err)
	}
	writes := 0
	mcpserver.OnUserServersWritten = func() { writes++ }
	t.Cleanup(func() { mcpserver.OnUserServersWritten = nil })
	if _, err := mcpserver.UpsertUserServer("el", func(s *mcpserver.Spec) { s.Autostart = boolp(false) }); err != nil {
		t.Fatal(err)
	}
	if writes != 1 {
		t.Fatalf("the write was announced %d times", writes)
	}
	h.turnMu.Lock() // a turn is running
	if h.FollowServersIfIdle(ctx) {
		t.Fatal("re-read during a turn")
	}
	h.turnMu.Unlock()
	if !h.mgr.ServerStarted("el") {
		t.Fatal("the busy session changed anyway")
	}
	if !h.FollowServersIfIdle(ctx) || h.mgr.ServerStarted("el") {
		t.Fatal("the idle session did not follow the global off")
	}
}
