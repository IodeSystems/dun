package dund

import (
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// Under systemd each engine starts in a scope of its own (USER 2026-11-17):
// systemd-run with no $VAR expansion where systemd knows the flag, without
// it where it does not, and no scope (a plain start) where systemd-run does
// not work.
func TestLinkedCommandScope(t *testing.T) {
	oldProbe := scopeProbe
	t.Cleanup(func() { scopeProbe, scopeOnce, scopePre = oldProbe, sync.Once{}, nil })
	sp := ExecSpawner{Bin: "/b/dun", EnginesDir: "/h/dund/engines", Scope: true}
	for _, tc := range []struct {
		name  string
		probe func(args ...string) error
		want  string
	}{
		{"systemd 254+", func(...string) error { return nil },
			"systemd-run --user --scope --quiet --collect --expand-environment=no -- /b/dun --engine --workspace /w --listen /h/dund/engines --model m"},
		{"older systemd", func(args ...string) error {
			if slices.Contains(args, "--expand-environment=no") {
				return exec.ErrNotFound
			}
			return nil
		}, "systemd-run --user --scope --quiet --collect -- /b/dun --engine --workspace /w --listen /h/dund/engines --model m"},
		{"no systemd-run", func(...string) error { return exec.ErrNotFound },
			"/b/dun --engine --workspace /w --listen /h/dund/engines --model m"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scopeProbe, scopeOnce, scopePre = tc.probe, sync.Once{}, nil
			name, argv := sp.linkedCommand("/w", []string{"--model", "m"})
			if got := name + " " + strings.Join(argv, " "); got != tc.want {
				t.Fatalf("got  %s\nwant %s", got, tc.want)
			}
			// Twice: the probe's answer is kept, and argv does not grow.
			name, argv = sp.linkedCommand("/w", []string{"--model", "m"})
			if got := name + " " + strings.Join(argv, " "); got != tc.want {
				t.Fatalf("second: %s", got)
			}
		})
	}
	sp.Scope = false
	if name, _ := sp.linkedCommand("/w", nil); name != "/b/dun" {
		t.Fatalf("no scope asked: %s", name)
	}
}

func TestLinkableNeedsAShortDir(t *testing.T) {
	if !linkable("/home/me/.dun/dund/engines") || linkable("") || linkable("/"+strings.Repeat("x", 100)) {
		t.Fatal("linkable")
	}
}

// An adopted engine that is still dund's child (an in-place exec keeps
// children) is reaped by waitPid.
func TestWaitPidReapsAChild(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 3")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- waitPid(cmd.Process.Pid) }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "exit status 3") {
			t.Fatalf("waitPid: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waitPid did not return")
	}
}
