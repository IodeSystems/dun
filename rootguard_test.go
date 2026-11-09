package dun

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/agentkit/llm"
)

// manyRepos makes n git repositories (a .git directory and two files each)
// under a new directory, named r00, r01, …
func manyRepos(t testing.TB, n int) string {
	t.Helper()
	ws := t.TempDir()
	for i := range n {
		r := filepath.Join(ws, fmt.Sprintf("r%02d", i))
		for _, d := range []string{".git", "pkg"} {
			if err := os.MkdirAll(filepath.Join(r, d), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		for _, f := range []string{"a.go", "pkg/b.go"} {
			if err := os.WriteFile(filepath.Join(r, f), []byte("package x\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return ws
}

func TestScanRoot(t *testing.T) {
	ws := manyRepos(t, 20)
	s := ScanRoot(ws)
	if s.InRepo || len(s.Repos) != 20 || s.Repos[3] != "r03" || s.Files != 40 || s.Over || !s.TooBig() {
		t.Fatalf("20 repos: %+v", s)
	}
	if got := scanSummary(s); got != "20 git repos (40 files)" {
		t.Errorf("summary %q", got)
	}

	// Two repos, one nested deeper than the depth: not too big.
	two := t.TempDir()
	for _, d := range []string{"a/.git", "b/c/.git", "x/y/z/w/.git"} {
		os.MkdirAll(filepath.Join(two, d), 0o755)
	}
	if s := ScanRoot(two); len(s.Repos) != 2 || s.TooBig() {
		t.Fatalf("two repos: %+v", s)
	}

	// A git repository: git lists the files; a nested checkout is git's to
	// leave out.
	if _, err := exec.LookPath("git"); err == nil {
		repo := t.TempDir()
		if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
			t.Fatalf("git init: %v %s", err, out)
		}
		for _, f := range []string{"a.go", "b.go", "c/d.go"} {
			os.MkdirAll(filepath.Dir(filepath.Join(repo, f)), 0o755)
			os.WriteFile(filepath.Join(repo, f), []byte("x"), 0o644)
		}
		for i := range 4 {
			os.MkdirAll(filepath.Join(repo, fmt.Sprintf("clones/c%d/.git", i)), 0o755)
		}
		if s := ScanRoot(repo); !s.InRepo || s.Files != 3 || s.TooBig() {
			t.Fatalf("repo: %+v", s)
		}
	}

	// Over the file cap: counting stops there.
	defer func(n int) { guardMaxFiles = n }(guardMaxFiles)
	guardMaxFiles = 30
	if s := ScanRoot(ws); !s.Over || s.Files != 31 || len(s.Repos) != 20 {
		t.Fatalf("over the cap: %+v", s)
	}
}

// What the guard costs: on dun's own tree (a git repository) and on a
// directory of 20 repositories, and on $DUN_GUARD_COST_ROOTS (a list of
// directories) when set. Run with -v to read it.
func TestScanRoot_Cost(t *testing.T) {
	wd, _ := os.Getwd()
	cases := []struct{ name, root string }{{"dun", wd}, {"20 repos", manyRepos(t, 20)}}
	for _, r := range filepath.SplitList(os.Getenv("DUN_GUARD_COST_ROOTS")) {
		cases = append(cases, struct{ name, root string }{r, r})
	}
	for _, c := range cases {
		best := time.Hour
		var s RootScan
		for range 5 {
			s = ScanRoot(c.root)
			best = min(best, s.Took)
		}
		t.Logf("%s: %s, best of 5 %s", c.name, scanSummary(s), best)
		if best > time.Second && !s.TimedOut {
			t.Errorf("%s: the guard took %s", c.name, best)
		}
	}
}

// startGuarded starts a session on ws with the fake poly-lsp (an index
// build that is ready at once) as its code server.
func startGuarded(t *testing.T, ws string, env ...string) (*Harness, string) {
	t.Helper()
	rootScans.Delete(filepath.Clean(ws))
	h, log, _ := startIndexed(t, ws, append([]string{fakeIndexEnv + "=ready"}, env...)...)
	waitIndex(t, h, IndexReady)
	return h, log
}

func dispatchTool(t *testing.T, h *Harness, name, args string) string {
	t.Helper()
	tc := llm.ToolCall{}
	tc.Function.Name, tc.Function.Arguments = name, args
	out, err := h.Session.Dispatch(context.Background(), tc)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// spawnRoot is the --root of a spawn's args line.
func spawnRoot(args string) string {
	f := strings.Fields(args)
	for i := range f {
		if f[i] == "--root" && i+1 < len(f) {
			return f[i+1]
		}
	}
	return ""
}

// calls are the fake's node_query calls, their arguments.
func calls(log string) []string {
	var out []string
	for _, l := range fakeLog(log) {
		if a, ok := strings.CutPrefix(l, "call node_query "); ok {
			out = append(out, a)
		}
	}
	return out
}

// A directory of 20 repositories: poly-lsp starts on the empty placeholder,
// not the root; a call that names no repository is refused with the list; a
// call with a path in one starts poly-lsp on that repository and runs with
// the path made relative to it; a call naming two is refused; a call that
// names none runs where the server is.
func TestCodeGuard_ScopedStart(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws := manyRepos(t, 20)
	h, log := startGuarded(t, ws)
	args := argLines(log)
	if len(args) != 1 || spawnRoot(args[0]) != filepath.Join(DunHome(), "poly-lsp-empty-root") {
		t.Fatalf("first spawn: %q", args)
	}

	out := dispatchTool(t, h, "node_query", `{"query":"#Start"}`)
	want := "ERROR: this folder holds 20 git repos (40 files); poly-lsp works on one repo: give path= inside one of: " +
		"r00, r01, r02, r03, r04, r05, r06, r07, … (12 more)"
	if out != want {
		t.Fatalf("refusal:\n%s\nwant:\n%s", out, want)
	}
	if len(calls(log)) != 0 {
		t.Fatalf("a refused call reached poly-lsp: %q", calls(log))
	}

	out = dispatchTool(t, h, "node_query", `{"query":"path=r03/pkg/b.go func"}`)
	if !strings.HasPrefix(out, "[poly-lsp now runs on r03/ (this folder holds 20 git repos (40 files))") || !strings.HasSuffix(out, "[]") {
		t.Fatalf("scoped call: %q", out)
	}
	args = argLines(log)
	if len(args) != 2 || spawnRoot(args[1]) != filepath.Join(ws, "r03") {
		t.Fatalf("scoped spawn: %q", args)
	}
	if c := calls(log); len(c) != 1 || c[0] != `{"query":"path=pkg/b.go func"}` {
		t.Fatalf("the call poly-lsp got: %q", c)
	}

	if out := dispatchTool(t, h, "node_query", `{"query":"path=r03/a.go , path=`+ws+`/r04/a.go"}`); !strings.Contains(out, "names paths in 2 repos (r03, r04)") {
		t.Fatalf("two repos: %q", out)
	}
	if out := dispatchTool(t, h, "node_query", `{"query":"#Start"}`); out != "[]" {
		t.Fatalf("a call with no path, scoped: %q", out)
	}
	if c := calls(log); len(c) != 2 || len(argLines(log)) != 2 {
		t.Fatalf("calls %q, spawns %q", c, argLines(log))
	}
}

// The person's index-anyway lifts the guard: poly-lsp gets the root.
func TestCodeGuard_IndexAnywayLifts(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws := manyRepos(t, 4)
	if _, err := updateLocalServer(ws, ServerCode, func(s *ServerSpec) { s.IndexAnyway = true }); err != nil {
		t.Fatal(err)
	}
	h, log := startGuarded(t, ws, fakeAnywayEnv+"=1")
	if args := argLines(log); len(args) != 1 || spawnRoot(args[0]) != ws || !strings.HasSuffix(args[0], "--index-anyway") {
		t.Fatalf("spawn: %q", args)
	}
	if out := dispatchTool(t, h, "node_query", `{"query":"#Start"}`); out != "[]" {
		t.Fatalf("call: %q", out)
	}
}

// A small root is not guarded at all.
func TestCodeGuard_SmallRoot(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws := manyRepos(t, 2)
	h, log := startGuarded(t, ws)
	if args := argLines(log); len(args) != 1 || spawnRoot(args[0]) != ws {
		t.Fatalf("spawn: %q", args)
	}
	if out := dispatchTool(t, h, "node_query", `{"query":"path=r01/a.go"}`); out != "[]" {
		t.Fatalf("call: %q", out)
	}
	if c := calls(log); len(c) != 1 || c[0] != `{"query":"path=r01/a.go"}` {
		t.Fatalf("calls %q", c)
	}
}
