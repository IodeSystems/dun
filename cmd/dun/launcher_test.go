package main

import (
	"testing"
	"time"
)

// The launcher registry end-to-end over the real unix socket: a session
// registers, `status` shows it, `shutdown` refuses while attached, and closing
// the registration deregisters it.
func TestLauncher_Registry(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	go func() { _ = runLauncher() }()

	waitUntil(t, func() bool { return dialOK(launcherSocket()) }, "launcher socket")

	lc := registerSession("tui", "/ws")
	if lc == nil {
		t.Fatal("registerSession returned nil")
	}
	defer lc.close()

	r, ok := query(req{Op: "status"})
	if !ok || len(r.Sessions) != 1 {
		t.Fatalf("status should show 1 session, got ok=%v %+v", ok, r.Sessions)
	}
	if s := r.Sessions[0]; s.Kind != "tui" || s.Workspace != "/ws" {
		t.Fatalf("session meta wrong: %+v", s)
	}

	// shutdown refuses while attached (the kick-warning) — must NOT exit here.
	if sr, _ := query(req{Op: "shutdown", Force: false}); sr.OK || sr.Attached != 1 {
		t.Fatalf("shutdown should refuse with 1 attached, got %+v", sr)
	}

	// Closing the registration deregisters the session.
	lc.close()
	waitUntil(t, func() bool {
		r, _ := query(req{Op: "status"})
		return len(r.Sessions) == 0
	}, "deregister")
}

func waitUntil(t *testing.T, cond func() bool, what string) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
