//go:build unix

package dundcli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A handover keeps the service's previous build as <exe>.prev, so a rollback
// has something to restore.
func TestInstallHandoverKeepsThePreviousBuild(t *testing.T) {
	svcDir, newDir := t.TempDir(), t.TempDir()
	self, exe := filepath.Join(svcDir, "dun"), filepath.Join(newDir, "dun")
	fakeDun(t, self, "v1", 0, "")
	fakeDun(t, exe, "v2", 0, "")
	if err := installHandover(self, exe, "v2", serviceArgs); err != nil {
		t.Fatal(err)
	}
	if ProbeVersion(self) != "v2" {
		t.Fatalf("the service's binary is not the new build: %q", ProbeVersion(self))
	}
	if ProbeVersion(prevBuildPath(self)) != "v1" {
		t.Fatalf("the previous build is not kept: %q", ProbeVersion(prevBuildPath(self)))
	}
}

// A second consecutive failed start (a build that died at start) restores
// the previous build over the binary, clears the counter, and sets the
// upgrade-refused marker, which canHandOver consults.
func TestRollbackRestoresThePreviousBuild(t *testing.T) {
	svcDir := t.TempDir()
	home := t.TempDir()
	self := filepath.Join(svcDir, "dun")
	fakeDun(t, self, "v1", 0, "")
	prev := prevBuildPath(self)
	// Simulate a handover that left v1 at .prev and a broken build at self.
	if err := os.Rename(self, prev); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(self, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// First failed start: counter to 1, no rollback.
	rollbackGuard(home, self)
	if ProbeVersion(self) != "" {
		t.Fatal("the binary should still be the broken build after the first failure")
	}
	if b, _ := os.ReadFile(failedStartsPath(home)); string(b) != "1\n" {
		t.Fatalf("counter after first failure: %q", b)
	}
	if _, err := os.Stat(upgradeRefusedPath(self)); err == nil {
		t.Fatal("the marker is set after the first failure")
	}

	// Second failed start: rollback fires.
	rollbackGuard(home, self)
	if got := ProbeVersion(self); got != "v1" {
		t.Fatalf("the binary is not the previous build after rollback: %q", got)
	}
	if _, err := os.Stat(failedStartsPath(home)); err == nil {
		t.Fatal("the counter should be cleared after rollback")
	}
	if _, err := os.Stat(upgradeRefusedPath(self)); err != nil {
		t.Fatal("the upgrade-refused marker is not set after rollback")
	}
	// canHandOver refuses upgrades while the marker is there.
	if err := canHandOver(self); err == nil || !strings.Contains(err.Error(), "upgrades are refused") {
		t.Fatalf("canHandOver with marker: %v", err)
	}
}

// A dund that serves clears the counter and the marker: the refusal ends.
func TestRollbackClearsStateWhenServing(t *testing.T) {
	svcDir := t.TempDir()
	home := t.TempDir()
	self := filepath.Join(svcDir, "dun")
	fakeDun(t, self, "v1", 0, "")
	// Set the marker and counter as if a rollback just fired.
	if err := os.MkdirAll(filepath.Join(home, "dund"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(failedStartsPath(home), []byte("2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(upgradeRefusedPath(self), []byte("now\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rollbackClear(home, self)
	if _, err := os.Stat(failedStartsPath(home)); err == nil {
		t.Fatal("the counter should be cleared")
	}
	if _, err := os.Stat(upgradeRefusedPath(self)); err == nil {
		t.Fatal("the marker should be cleared")
	}
	// canHandOver is unrefused.
	if err := canHandOver(self); err != nil {
		t.Fatalf("canHandOver after clear: %v", err)
	}
}
