package main

import (
	"os"
	"testing"
)

// TestMain keeps every test off the real $DUN_HOME: a test that makes a
// worktree registers its temp repo as a project, and that must land in a temp
// home, not in ~/.dun/projects.json.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "dun-cmd-test-home-")
	if err != nil {
		panic(err)
	}
	os.Setenv("DUN_HOME", home)
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}
