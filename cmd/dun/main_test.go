package main

import (
	"os"
	"os/exec"
	"strings"
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
	keepGoCaches()
	os.Setenv("DUN_HOME", home)
	// And off the real home directory: a harness reads the user's rules
	// files (~/AGENTS.md, ~/CLAUDE.md, ~/.claude/CLAUDE.md, dun/rules.go).
	os.Setenv("HOME", home)
	// Off the user's shared tool daemons, as the dun package's TestMain:
	// a real poly-lsp-mcp would proxy to ~/.poly-lsp's, raglit read ~/.raglit.
	os.Setenv("POLY_LSP_NO_DAEMON", "1")
	os.Setenv("POLY_LSP_HOME", home+"/poly-lsp")
	os.Setenv("XDG_RUNTIME_DIR", home)
	os.Setenv("RAGLIT_ROOT", home+"/rag")
	os.Setenv("DUN_REGISTRY", "off") // never the network
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}

// keepGoCaches pins the go command's build and module caches to the ones it
// uses now, before HOME moves: under the temp HOME, each `go build` a test
// runs (testDun, the adoption test's two builds) started from an empty
// build cache and an empty module cache, recompiled every dependency and
// fetched every module: 40-80 s a build. A test that wants its own cache
// still sets one in its command's env (toolver_test.go).
func keepGoCaches() {
	out, err := exec.Command("go", "env", "GOCACHE", "GOMODCACHE").Output()
	if err != nil {
		return // no go command: the builds fail on their own, as before
	}
	v := strings.Split(strings.TrimSpace(string(out)), "\n")
	for i, k := range []string{"GOCACHE", "GOMODCACHE"} {
		if i < len(v) && v[i] != "" && os.Getenv(k) == "" {
			os.Setenv(k, v[i])
		}
	}
}
