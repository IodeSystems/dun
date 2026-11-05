package tui

import (
	"fmt"
	"os"
	"sync"
	"testing"
)

// TestMain keeps every test off the real $DUN_HOME: a test that makes a
// worktree registers its temp repo as a project, and that must land in a temp
// home, not in ~/.dun/projects.json.
// overflowSeen collects every overflow report of the run.
var (
	overflowMu   sync.Mutex
	overflowSeen []string
)

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
	os.Setenv("RAGLIT_ROOT", home+"/rag")
	os.Setenv("DUN_REGISTRY", "off") // never the network
	// /upgrade runs `dun tools`: the test binary is not dun.
	runTools = func(bool) ([]toolState, error) { return nil, nil }
	// Any frame taller than its screen fails the run, whichever test drew it
	// (overflow.go): the global check for a part that pushes the input away.
	onOverflow = func(report string) {
		overflowMu.Lock()
		overflowSeen = append(overflowSeen, report)
		overflowMu.Unlock()
	}
	code := m.Run()
	if len(overflowSeen) > 0 && code == 0 {
		fmt.Fprintf(os.Stderr, "FAIL: %d frame(s) taller than the screen; first: %s\n", len(overflowSeen), overflowSeen[0])
		code = 1
	}
	os.RemoveAll(home)
	os.Exit(code)
}
