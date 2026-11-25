//go:build unix

package dundcli

import (
	"os"
	"strconv"
	"strings"
	"testing"
)

// One dund per home: claimPidfile refuses a second start while a live
// dund owns the pidfile, and leaves the running instance's file untouched.
// A pidfile whose process is gone is replaced, not an error.
func TestClaimPidfileOnePerHome(t *testing.T) {
	home := t.TempDir()
	l := &loader{home: home}
	pf := l.pidfile()

	// The running instance claims the pidfile first.
	live := os.Getppid() // alive and is not this test process
	if err := os.WriteFile(pf, []byte(strconv.Itoa(live)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := claimPidfile(pf); err == nil {
		t.Fatal("second start was not refused: the live instance owns the home")
	} else if !strings.Contains(err.Error(), "already running") {
		t.Fatalf("refusal does not name the live pid: %v", err)
	}
	// The running instance's pidfile is untouched: it still names the live pid.
	b, err := os.ReadFile(pf)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(b)); got != strconv.Itoa(live) {
		t.Fatalf("the running instance's pidfile was changed: got pid %s, want %d", got, live)
	}

	// A pidfile whose process is gone is replaced, not an error.
	dead := 1
	for {
		_, alive := pidAlive(pf)
		if !alive {
			break
		}
		dead++
		_ = os.WriteFile(pf, []byte(strconv.Itoa(dead)+"\n"), 0o600)
	}
	if err := claimPidfile(pf); err != nil {
		t.Fatalf("a stale pidfile was not replaced: %v", err)
	}
	b, _ = os.ReadFile(pf)
	if got := strings.TrimSpace(string(b)); got != strconv.Itoa(os.Getpid()) {
		t.Fatalf("the pidfile does not name this process after the claim: %s", got)
	}
}
