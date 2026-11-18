// Package dunhome is where dun keeps its state: ~/.dun, or $DUN_HOME.
// A leaf, so a package split out of harness can find the home without
// importing harness.
package dunhome

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// Dir is dun's home (~/.dun). $DUN_HOME overrides it.
func Dir() string {
	if h := os.Getenv("DUN_HOME"); h != "" {
		return h
	}
	if testing.Testing() {
		// A test that did not set $DUN_HOME never reaches the real one:
		// 1,207 ~/.dun/sessions/-tmp-Test… directories leaked from tests
		// before this (2026-11-14).
		return testHome()
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".dun"
	}
	return filepath.Join(home, ".dun")
}

// testHome is a test binary's stand-in for ~/.dun, one per process. It is
// not created here: a test that only looks leaves nothing behind.
func testHome() string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("dun-test-home-%d", os.Getpid()))
}
