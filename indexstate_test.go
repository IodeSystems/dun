package dun

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// startIndexed starts a session whose code server is the fake poly-lsp in
// the given mode (fakeIndexReport; "" is an older build), and returns the
// harness, the fake's log, and the index reports dun made.
func startIndexed(t *testing.T, ws string, env ...string) (*Harness, string, chan string) {
	t.Helper()
	lsp, log := fakeBin(t, "poly-lsp-mcp", "poly-lsp", env...)
	msgs := make(chan string, 8)
	h, err := Start(context.Background(), Config{
		Workspace: ws, Client: &scriptRunner{}, SessionFile: filepath.Join(ws, "s.jsonl"),
		Servers:       []Server{{ID: ServerCode, Command: lsp, Args: []string{"mcp", "--root", ws}, Timeout: 20, Autostart: true}},
		OnServerIndex: func(id, msg string) { msgs <- msg },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return h, log, msgs
}

func nextMsg(t *testing.T, msgs chan string) string {
	t.Helper()
	select {
	case m := <-msgs:
		return m
	case <-time.After(15 * time.Second):
		t.Fatal("no index report")
	}
	return ""
}

func codeState(h *Harness) ServerState {
	for _, st := range h.Servers() {
		if st.ID == ServerCode {
			return st
		}
	}
	return ServerState{}
}

// waitIndex waits for the code server's index to reach state.
func waitIndex(t *testing.T, h *Harness, state string) ServerState {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if st := codeState(h); st.Index == state {
			return st
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("index never %s: %+v", state, codeState(h))
	return ServerState{}
}

// argLines are the fake's "args" lines: one per spawn.
func argLines(log string) []string {
	var out []string
	for _, l := range fakeLog(log) {
		if a, ok := strings.CutPrefix(l, "args "); ok {
			out = append(out, a)
		}
	}
	return out
}

// An index that stops at its cap, mostly other checkouts: dun leaves them
// out, saves that for the project, restarts the server with --exclude, and
// says what it did and how to undo it. The next session starts with the
// exclude and does not hit the cap.
func TestIndex_LimitExcludesAndRetries(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws := t.TempDir()
	env := []string{fakeIndexEnv + "=limit", fakeIndexOKEnv + "=.claude/worktrees"}
	h, log, msgs := startIndexed(t, ws, env...)
	msg := nextMsg(t, msgs)
	for _, want := range []string{"stopped at its limit", "22,012 indexable files, limit 10,000",
		".claude/worktrees/ — 21,557 files, 97% of the index, 48 other git checkouts",
		"restarting without it", "To undo: /poly-lsp include .claude/worktrees."} {
		if !strings.Contains(msg, want) {
			t.Errorf("the report lacks %q:\n%s", want, msg)
		}
	}
	if m := nextMsg(t, msgs); m != "" {
		t.Errorf("a ready small index said something: %q", m)
	}
	st := waitIndex(t, h, IndexReady)
	if len(st.Exclude) != 1 || st.Exclude[0] != ".claude/worktrees" {
		t.Fatalf("excludes: %+v", st.Exclude)
	}
	args := argLines(log)
	if len(args) != 2 || strings.Contains(args[0], "--exclude") || !strings.HasSuffix(args[1], "--exclude .claude/worktrees") {
		t.Fatalf("spawns: %q", args)
	}
	p := ResolveProject(ws)
	b, err := os.ReadFile(filepath.Join(p.WriteDir(), LocalServersFile))
	if err != nil || !strings.Contains(string(b), `"exclude"`) {
		t.Fatalf("not saved for the project: %s %v", b, err)
	}
	h.Close()

	// A later session: the saved exclude is on the first spawn.
	h2, log2, msgs2 := startIndexed(t, ws, env...)
	if m := nextMsg(t, msgs2); m != "" {
		t.Errorf("the later session reported %q", m)
	}
	waitIndex(t, h2, IndexReady)
	if args := argLines(log2); len(args) != 1 || !strings.HasSuffix(args[0], "--exclude .claude/worktrees") {
		t.Fatalf("later spawn: %q", args)
	}

	// The undo: include takes it back and restarts without it.
	path, changed, err := h2.SetIndexExclude(context.Background(), ServerCode, ".claude/worktrees/", false)
	if err != nil || !changed || path == "" {
		t.Fatalf("include: %q %v %v", path, changed, err)
	}
	if ex := h2.IndexExcludes(ServerCode); len(ex) != 0 {
		t.Fatalf("still excluded: %v", ex)
	}
	if args := argLines(log2); len(args) != 2 || strings.Contains(args[1], "--exclude") {
		t.Fatalf("after include: %q", args)
	}
}

// The retry is made once: a second failure is reported, not retried.
func TestIndex_RetryFailsReports(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws := t.TempDir()
	h, log, msgs := startIndexed(t, ws, fakeIndexEnv+"=limit")
	if m := nextMsg(t, msgs); !strings.Contains(m, "dun left out") {
		t.Fatalf("first report: %s", m)
	}
	m := nextMsg(t, msgs)
	for _, want := range []string{"failed again", ".claude/worktrees", "remedy: exclude the heavy directories", "/poly-lsp exclude <dir>"} {
		if !strings.Contains(m, want) {
			t.Errorf("second report lacks %q:\n%s", want, m)
		}
	}
	waitIndex(t, h, IndexFailed)
	time.Sleep(200 * time.Millisecond)
	if n := len(argLines(log)); n != 2 {
		t.Fatalf("%d spawns, want 2", n)
	}
}

// Nothing obvious to leave out: dun changes nothing and says why, with the
// post-mortem and the remedy.
func TestIndex_LimitNothingObvious(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws := t.TempDir()
	h, log, msgs := startIndexed(t, ws, fakeIndexEnv+"=plain")
	m := nextMsg(t, msgs)
	for _, want := range []string{"No directory is an obvious one", "src/", "docs/", "remedy:", "/poly-lsp exclude <dir>"} {
		if !strings.Contains(m, want) {
			t.Errorf("report lacks %q:\n%s", want, m)
		}
	}
	st := waitIndex(t, h, IndexFailed)
	if len(st.Exclude) != 0 || !strings.Contains(st.IndexErr, "index limit") {
		t.Fatalf("%+v", st)
	}
	time.Sleep(200 * time.Millisecond)
	if n := len(argLines(log)); n != 1 {
		t.Fatalf("%d spawns, want 1", n)
	}
}

// A large workspace is a warning: said once, with the heaviest directories
// and the offer to exclude the obvious one; nothing is excluded.
func TestIndex_LargeWarnsAndOffers(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws := t.TempDir()
	h, log, msgs := startIndexed(t, ws, fakeIndexEnv+"=large")
	m := nextMsg(t, msgs)
	for _, want := range []string{"a large workspace — 22,012 indexable files (it warns above 10,000)", "The index is built.",
		".claude/worktrees/", "48 nested checkout(s)", "/poly-lsp exclude .claude/worktrees", "/poly-lsp include <dir> undoes it"} {
		if !strings.Contains(m, want) {
			t.Errorf("warning lacks %q:\n%s", want, m)
		}
	}
	if st := waitIndex(t, h, IndexReady); len(st.Exclude) != 0 {
		t.Fatalf("a warning excluded: %+v", st.Exclude)
	}
	if n := len(argLines(log)); n != 1 {
		t.Fatalf("%d spawns, want 1", n)
	}
}

// A ready index with a checkout in it: one quiet line, with the command.
func TestIndex_NestedCheckoutWarning(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws := t.TempDir()
	_, _, msgs := startIndexed(t, ws, fakeIndexEnv+"=nested")
	m := nextMsg(t, msgs)
	if !strings.Contains(m, "the index holds .claude/worktrees/") || !strings.Contains(m, "To leave it out: /poly-lsp exclude .claude/worktrees") ||
		strings.Contains(m, "\n") {
		t.Fatalf("warning: %q", m)
	}
}

// An older poly-lsp-mcp: no index state, and no --exclude even when the
// project has one saved (it would refuse the flag).
func TestIndex_OldBinary(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws := t.TempDir()
	if _, err := updateLocalServer(ws, ServerCode, func(s *ServerSpec) { s.Exclude = []string{"vendor"} }); err != nil {
		t.Fatal(err)
	}
	lsp, log := fakeBin(t, "poly-lsp-mcp", "poly-lsp")
	h, err := Start(context.Background(), Config{
		Workspace: ws, Client: &scriptRunner{}, SessionFile: filepath.Join(ws, "s.jsonl"),
		Servers: []Server{{ID: ServerCode, Command: lsp, Args: []string{"mcp", "--root", ws}, Timeout: 20, Autostart: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	h.WaitServers(context.Background())
	st := codeState(h)
	if !st.Running || st.Index != "" {
		t.Fatalf("%+v", st)
	}
	if ok, _, err := IndexProtocol(lsp); ok || err != nil {
		t.Fatalf("an old build speaks the protocol? %v %v", ok, err)
	}
	for _, l := range fakeLog(log) {
		if strings.Contains(l, "--exclude") {
			t.Fatalf("an old build got --exclude: %s", l)
		}
	}
}

// While it builds, a running server says indexing.
func TestIndex_BuildingState(t *testing.T) {
	h := &Harness{}
	h.markIndexBuilding(ServerCode, true)
	var st ServerState
	h.srvMu.Lock()
	h.indexOf(ServerCode, &st)
	h.srvMu.Unlock()
	if st.Index != IndexBuilding {
		t.Fatalf("%+v", st)
	}
	h.markIndexBuilding(ServerCode, false)
	st = ServerState{}
	h.indexOf(ServerCode, &st)
	if st.Index != "" {
		t.Fatalf("%+v", st)
	}
}

func TestChooseExcludes(t *testing.T) {
	r := IndexReport{Files: 30000, Heaviest: []IndexDir{
		{Dir: "node_modules/", Files: 18000, Share: 0.6},
		{Dir: "src/", Files: 6000, Share: 0.2},
		{Dir: "third/", Files: 4000, Share: 0.1, NestedCheckouts: 1},
	}}
	got := chooseExcludes(r, 10000)
	if len(got) != 2 || got[0].Dir != "node_modules/" || got[1].Dir != "third/" {
		t.Fatalf("%+v", got)
	}
	// One is enough: the smallest set.
	if got := chooseExcludes(r, 15000); len(got) != 1 || got[0].Dir != "node_modules/" {
		t.Fatalf("%+v", got)
	}
	// Not enough even with all of them: none.
	if got := chooseExcludes(r, 2000); got != nil {
		t.Fatalf("%+v", got)
	}
	// A generated name under half the index is not obvious.
	r2 := IndexReport{Files: 20000, Heaviest: []IndexDir{{Dir: "build/", Files: 12000, Share: 0.4}}}
	if got := chooseExcludes(r2, 10000); got != nil {
		t.Fatalf("%+v", got)
	}
	for dir, want := range map[string]bool{"a/.claude/worktrees/": true, ".claude/worktrees/": true, "vendor/": true,
		"x/.venv/": true, "target/": true, ".git-rewrite/": true, ".github/": false, "src/": false, "pkg/build/": true} {
		if looksGenerated(dir) != want {
			t.Errorf("looksGenerated(%q) != %v", dir, want)
		}
	}
	if cleanExclude("../x") != "" || cleanExclude("/abs") != "" || cleanExclude("a/b/") != "a/b" {
		t.Error("cleanExclude")
	}
}
