package dund

import (
	"os"
	"testing"
)

// TestMain keeps every test off the real $DUN_HOME: a session's snapshot
// writes under its sessions/ (it was ~/.dun/sessions/-tmp-Test…, 1,200 of
// them by 2026-11-04).
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "dund-test-home-")
	if err != nil {
		panic(err)
	}
	os.Setenv("DUN_HOME", home)
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}
