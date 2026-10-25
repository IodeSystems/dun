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
	// Off the user's shared tool daemons, as the dun package's TestMain:
	// a real poly-lsp-mcp would proxy to ~/.poly-lsp's, raglit read ~/.raglit.
	os.Setenv("POLY_LSP_NO_DAEMON", "1")
	os.Setenv("POLY_LSP_HOME", home+"/poly-lsp")
	os.Setenv("XDG_RUNTIME_DIR", home)
	os.Setenv("RAGLIT_ROOT", home+"/raglit")
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}
