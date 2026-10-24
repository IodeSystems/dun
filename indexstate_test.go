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

// An index out of its startup budget, mostly other checkouts: dun leaves
// them out, saves that for the project, restarts the server with --exclude,
// and says what it did and how to undo it. The next session starts with the
// exclude and fits.
func TestIndex_BudgetExcludesAndRetries(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws := t.TempDir()
	env := []string{fakeIndexEnv + "=budget", fakeIndexOKEnv + "=.claude/worktrees"}
	h, log, msgs := startIndexed(t, ws, env...)
	msg := nextMsg(t, msgs)
	for _, want := range []string{"the index did not fit its startup budget — 30000ms read 4,100 of 22,012 indexable files",
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
	if len(st.Exclude) != 1 || st.Exclude[0] != ".claude/worktrees" || st.IndexBudget != "" {
		t.Fatalf("excludes: %+v, budget %q", st.Exclude, st.IndexBudget)
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

// Out of budget with nothing obvious to leave out: dun raises the budget to
// the one the remedy names, saves it, restarts with it, and says how to
// undo it. The next session starts with it; budget default takes it back.
func TestIndex_BudgetRaisedWhenNothingObvious(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws := t.TempDir()
	env := []string{fakeIndexEnv + "=budget-plain", fakeIndexOKEnv + "=budget=48000ms"}
	h, log, msgs := startIndexed(t, ws, env...)
	msg := nextMsg(t, msgs)
	for _, want := range []string{"30000ms read 90,000 of 120,000 indexable files, and no directory is an obvious one to leave out",
		"dun raised the budget to 48000ms, for this project; poly-lsp is restarting with it.", "src/", "docs/",
		"To undo: /poly-lsp budget default. To leave a directory out instead: /poly-lsp exclude <dir>."} {
		if !strings.Contains(msg, want) {
			t.Errorf("the report lacks %q:\n%s", want, msg)
		}
	}
	if m := nextMsg(t, msgs); m != "" {
		t.Errorf("the ready index said something: %q", m)
	}
	if st := waitIndex(t, h, IndexReady); st.IndexBudget != "48000ms" || len(st.Exclude) != 0 {
		t.Fatalf("%+v", st)
	}
	if args := argLines(log); len(args) != 2 || strings.Contains(args[0], "--index-budget") || !strings.HasSuffix(args[1], "--index-budget 48000ms") {
		t.Fatalf("spawns: %q", args)
	}
	b, err := os.ReadFile(filepath.Join(ResolveProject(ws).WriteDir(), LocalServersFile))
	if err != nil || !strings.Contains(string(b), `"index_budget": "48000ms"`) {
		t.Fatalf("not saved for the project: %s %v", b, err)
	}
	h.Close()

	h2, log2, msgs2 := startIndexed(t, ws, env...)
	if m := nextMsg(t, msgs2); m != "" {
		t.Errorf("the later session reported %q", m)
	}
	if args := argLines(log2); len(args) != 1 || !strings.HasSuffix(args[0], "--index-budget 48000ms") {
		t.Fatalf("later spawn: %q", args)
	}
	if _, _, err := h2.SetIndexBudget(context.Background(), ServerCode, "soon"); err == nil {
		t.Error("a budget that is not one was taken")
	}
	if _, changed, err := h2.SetIndexBudget(context.Background(), ServerCode, "default"); err != nil || !changed {
		t.Fatalf("budget default: %v %v", changed, err)
	}
	if b := h2.IndexBudget(ServerCode); b != "" {
		t.Fatalf("budget still %q", b)
	}
	if args := argLines(log2); len(args) != 2 || strings.Contains(args[1], "--index-budget") {
		t.Fatalf("after default: %q", args)
	}
}

// The retry is made once: a second failure is reported, not retried.
func TestIndex_RetryFailsReports(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws := t.TempDir()
	h, log, msgs := startIndexed(t, ws, fakeIndexEnv+"=budget")
	if m := nextMsg(t, msgs); !strings.Contains(m, "dun left out") {
		t.Fatalf("first report: %s", m)
	}
	m := nextMsg(t, msgs)
	for _, want := range []string{"did not fit its startup budget", "again, after dun's change (excluded: .claude/worktrees)",
		"remedy: get under budget", "/poly-lsp exclude <dir>, /poly-lsp include <dir>, /poly-lsp budget <Nms|Nops|default>"} {
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

// The interim build (a file cap, no budget): the same excludes, and no
// --index-budget even when the project has one; with nothing obvious, dun
// changes nothing and says why, with the post-mortem and the remedy.
func TestIndex_InterimLimit(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws := t.TempDir()
	if _, err := updateLocalServer(ws, ServerCode, func(s *ServerSpec) { s.IndexBudget = "48000ms" }); err != nil {
		t.Fatal(err)
	}
	h, log, msgs := startIndexed(t, ws, fakeIndexEnv+"=limit", fakeIndexOKEnv+"=.claude/worktrees")
	if m := nextMsg(t, msgs); !strings.Contains(m, "the index stopped at its limit — 22,012 indexable files, limit 10,000. dun left out") {
		t.Fatalf("report: %s", m)
	}
	waitIndex(t, h, IndexReady)
	for _, a := range argLines(log) {
		if strings.Contains(a, "--index-budget") {
			t.Fatalf("the interim build got --index-budget: %s", a)
		}
	}
	h.Close()

	t.Setenv("DUN_HOME", t.TempDir())
	ws = t.TempDir()
	h, log, msgs = startIndexed(t, ws, fakeIndexEnv+"=limit-plain")
	m := nextMsg(t, msgs)
	for _, want := range []string{"No directory is an obvious one", "src/", "docs/", "remedy:", "/poly-lsp exclude <dir>"} {
		if !strings.Contains(m, want) {
			t.Errorf("report lacks %q:\n%s", want, m)
		}
	}
	st := waitIndex(t, h, IndexFailed)
	if len(st.Exclude) != 0 || st.IndexBudget != "" || !strings.Contains(st.IndexErr, "index limit") {
		t.Fatalf("%+v", st)
	}
	time.Sleep(200 * time.Millisecond)
	if n := len(argLines(log)); n != 1 {
		t.Fatalf("%d spawns, want 1", n)
	}
}

func TestRaisedBudget(t *testing.T) {
	rep := &IndexReport{Files: 12000}
	for _, c := range []struct {
		n    indexNote
		want string
	}{
		{indexNote{Remedy: "Or raise it: --index-budget 3000ms (now 300ms).", Budget: "300ms", Read: 10, Index: rep}, "3000ms"},
		{indexNote{Budget: "30000ms", Read: 9000, Index: rep}, "48000ms"}, // 30 s × 12000/9000 × 1.2
		{indexNote{Budget: "5000ops", Read: 5000, Index: rep}, "14401ops"},
		{indexNote{Budget: "soon", Index: rep}, ""},
	} {
		if got := raisedBudget(c.n); got != c.want {
			t.Errorf("raisedBudget(%+v) = %q, want %q", c.n, got, c.want)
		}
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
	if sup, _, err := IndexProtocol(lsp); sup.Protocol || sup.Budget || err != nil {
		t.Fatalf("an old build speaks the protocol? %+v %v", sup, err)
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

// A poly-lsp whose --daemon proxy keeps the hook is started as that proxy,
// with the project's options and the hook's env, for a workspace under
// $HOME; one without the marker, or a workspace elsewhere (the daemon would
// refuse it), is started in-process, as before.
func TestIndex_DaemonWhenTheHookIsKept(t *testing.T) {
	for _, c := range []struct{ marker, home, daemon bool }{{true, true, true}, {false, true, false}, {true, false, false}} {
		daemon := c.daemon
		t.Setenv("DUN_HOME", t.TempDir())
		ws := t.TempDir()
		t.Setenv("HOME", t.TempDir())
		if c.home {
			t.Setenv("HOME", filepath.Dir(ws))
		}
		env := []string{fakeIndexEnv + "=ready"}
		if c.marker {
			env = append(env, fakeDaemonEnv+"=1")
		}
		h, log, _ := startIndexed(t, ws, env...)
		waitIndex(t, h, IndexReady)
		args := argLines(log)
		if len(args) != 1 || strings.HasSuffix(args[0], " --daemon") != daemon {
			t.Errorf("%+v: spawns %q", c, args)
		}
		if !strings.Contains(strings.Join(fakeLog(log), "\n"), "start POLY_LSP_HOOK=/") {
			t.Errorf("%+v: no hook in the env:\n%s", c, strings.Join(fakeLog(log), "\n"))
		}
		h.Close()
	}
}
