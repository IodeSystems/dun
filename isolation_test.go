package dun

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iodesystems/agentkit/llm"
)

func gitrun(t *testing.T, dir string, args ...string) {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestWorktree_IsolatesChanges(t *testing.T) {
	repo := t.TempDir()
	gitrun(t, repo, "init", "-q")
	gitrun(t, repo, "config", "user.email", "t@t")
	gitrun(t, repo, "config", "user.name", "t")
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("hello\n"), 0o644)
	gitrun(t, repo, "add", ".")
	gitrun(t, repo, "commit", "-qm", "init")

	wt, isRepo, err := NewWorktree(repo, nil)
	if err != nil || !isRepo {
		t.Fatalf("NewWorktree: isRepo=%v err=%v", isRepo, err)
	}
	defer wt.Cleanup()
	if wt.Branch == "" || wt.Path == repo {
		t.Fatalf("expected an isolated worktree, got %+v", wt)
	}

	// Edit in the worktree; the ORIGINAL checkout must be untouched.
	os.WriteFile(filepath.Join(wt.Path, "a.txt"), []byte("hello\nworld\n"), 0o644)
	orig, _ := os.ReadFile(filepath.Join(repo, "a.txt"))
	if string(orig) != "hello\n" {
		t.Fatalf("worktree edit leaked into the main checkout: %q", orig)
	}
	if !strings.Contains(wt.Diff(), "world") {
		t.Fatalf("Diff should show the change:\n%s", wt.Diff())
	}
}

// A worktree lives under .dun/worktrees/, so a go.mod "replace => ../agentkit"
// only resolves from inside it through a symlink beside it. Without the
// symlink the isolation is real and the first build in it fails on a
// dependency that resolves fine in the checkout it was made from.
func TestWorktree_SymlinksItsMounts(t *testing.T) {
	repo := t.TempDir()
	gitrun(t, repo, "init", "-q")
	gitrun(t, repo, "config", "user.email", "t@t")
	gitrun(t, repo, "config", "user.name", "t")
	gitrun(t, repo, "commit", "-qm", "init", "--allow-empty")

	// The sibling module the replace directive points at.
	dep := t.TempDir()
	if err := os.WriteFile(filepath.Join(dep, "go.mod"), []byte("module dep\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	wt, isRepo, err := NewWorktree(repo, []MountSpec{{Source: dep, Name: "dep"}})
	if err != nil || !isRepo {
		t.Fatalf("NewWorktree: isRepo=%v err=%v", isRepo, err)
	}
	defer wt.Cleanup()

	// "../dep" FROM the worktree is the symlink in the worktree parent.
	via := filepath.Join(wt.Path, "..", "dep", "go.mod")
	if _, err := os.Stat(via); err != nil {
		t.Fatalf("the mount must resolve from inside the worktree (%s): %v", via, err)
	}
	if len(wt.Mounts) != 1 || wt.Mounts[0].Name != "dep" {
		t.Errorf("the worktree should carry its mounts, got %+v", wt.Mounts)
	}
}

// Harness.Mounts is what a MID-SESSION worktree is built from (/worktree new).
// It used to pass nil, so a worktree made at runtime got no symlinks at all
// while one made at startup did.
func TestHarnessMounts_AreTheSessionsOwn(t *testing.T) {
	want := []MountSpec{{Source: "/opt/agentkit", Name: "agentkit"}}
	h := &Harness{cfg: Config{ExtraMounts: want}}
	got := h.Mounts()
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("Mounts() = %+v, want %+v", got, want)
	}
	if (&Harness{}).Mounts() != nil {
		t.Error("a session with no mounts must report none, not an empty non-nil surprise")
	}
}

func TestWorktree_NonRepoPassThrough(t *testing.T) {
	dir := t.TempDir()
	wt, isRepo, err := NewWorktree(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if isRepo {
		t.Fatal("a bare temp dir should not be a git repo")
	}
	if wt.Path != dir {
		t.Fatalf("pass-through should use the dir, got %q", wt.Path)
	}
	wt.Cleanup() // no-op, must not panic
}

func TestHostExec(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "x.txt"), []byte("hi"), 0o644)
	out := HostExec{Dir: dir}.Run(context.Background(), "ls", nil)
	if !strings.Contains(out.Output, "x.txt") {
		t.Fatalf("exec ls should list x.txt: %q", out.Output)
	}
	if out.Failed() {
		t.Errorf("`ls` of a real dir must not be a failure: %+v", out)
	}
	// The exit status is a FACT on the result now, not a substring of the text.
	e := (HostExec{Dir: dir}).Run(context.Background(), "exit 3", nil)
	if e.Code != 3 || !e.Failed() {
		t.Fatalf("want code 3, got %+v", e)
	}
	if !strings.Contains(e.Render(), "[exit:") {
		t.Errorf("the model still needs the marker: %q", e.Render())
	}
}

// The regression that motivated ExecResult: a check whose OUTPUT contains the
// marker is not a failing check. Pass/fail is the exit code.
func TestExecResult_OutputCannotFakeAFailure(t *testing.T) {
	out := HostExec{Dir: t.TempDir()}.Run(context.Background(), `echo "[exit: 1] not really"`, nil)
	if out.Failed() {
		t.Errorf("a command that PRINTS the marker exited 0 and passed: %+v", out)
	}
	cfg := &ShipConfig{Checks: []map[string]string{{"compile": "printer"}}}
	exec := func(context.Context, string) ExecResult { return out }
	if fail := runChecks(context.Background(), cfg, exec); fail != "" {
		t.Errorf("runChecks failed a passing check on its own output:\n%s", fail)
	}
}

// Streaming is what makes a background job watchable: the tee sees bytes while
// the command is still running, and the result still carries all of them.
func TestHostExec_TeesOutputWhileRunning(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	w := writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		seen = append(seen, string(p))
		mu.Unlock()
		return len(p), nil
	})
	res := HostExec{Dir: t.TempDir()}.Run(context.Background(), "echo one; echo two", w)
	mu.Lock()
	got := strings.Join(seen, "")
	mu.Unlock()
	if !strings.Contains(got, "one") || !strings.Contains(got, "two") {
		t.Errorf("the tee missed output: %q", got)
	}
	if !strings.Contains(res.Output, "one") || !strings.Contains(res.Output, "two") {
		t.Errorf("teeing must not consume the output: %q", res.Output)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// A foreground command that never finishes must be killed and REPORTED as
// killed. The real case is a command blocked on input it can never receive —
// `gh auth login` polling a device flow with stdin on /dev/null — which held a
// session for 14 minutes and looked, from the outside, like slow work.
func TestHostExec_ForegroundDeadlineKillsAndReports(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	out := HostExec{Dir: t.TempDir()}.Run(ctx, "sleep 30", nil)
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("the deadline did not kill the command: took %s", d)
	}
	if !out.TimedOut || !out.Failed() {
		t.Errorf("a killed command must report as timed out and failed: %+v", out)
	}
	if !strings.Contains(out.Render(), "TIMED OUT") {
		t.Errorf("a timeout must say so, not surface as a bare signal: %q", out.Render())
	}
}

// The default only applies where nothing better is known: a caller with its own
// deadline (ship's checks carry ship.checkTimeout) keeps it, and a background
// job — whose entire purpose is the long build — is exempt.
func TestBound_DefaultsOnlyWhenNothingElseSaysOtherwise(t *testing.T) {
	_, cancel, limit := bound(context.Background())
	defer cancel()
	if limit != defaultExecTimeout {
		t.Errorf("a bare foreground exec should get the default, got %s", limit)
	}

	own, ownCancel := context.WithTimeout(context.Background(), time.Hour)
	defer ownCancel()
	ctx, cancel2, limit2 := bound(own)
	defer cancel2()
	if d, ok := ctx.Deadline(); !ok || time.Until(d) < 30*time.Minute {
		t.Error("an explicit deadline was silently shortened to the default")
	}
	if limit2 < 30*time.Minute {
		t.Errorf("reported limit should be the caller's, got %s", limit2)
	}

	bg, cancel3, limit3 := bound(WithoutExecTimeout(context.Background()))
	defer cancel3()
	if _, ok := bg.Deadline(); ok {
		t.Error("a background job must not be bounded — that is what background:true is for")
	}
	if limit3 != 0 {
		t.Errorf("no limit in force should report 0, got %s", limit3)
	}
}

func TestWithExec_RoutesExecLocallyElseMCP(t *testing.T) {
	inner := agentDispatch(func(name string) string { return "MCP:" + name })
	d := withExec(inner, HostExec{Dir: t.TempDir()}, nil, nil, nil)

	var call llm.ToolCall
	call.Function.Name = "exec"
	call.Function.Arguments = `{"command":"echo hello-exec"}`
	out, _ := d(context.Background(), call)
	if !strings.Contains(out, "hello-exec") {
		t.Fatalf("exec should run the backend: %q", out)
	}

	call = llm.ToolCall{}
	call.Function.Name = "search"
	out, _ = d(context.Background(), call)
	if out != "MCP:search" {
		t.Fatalf("non-exec tools should route to MCP: %q", out)
	}
}

// A container that outlives the timeout that killed it is a leak: `docker run`
// dying does not stop what it started, so the run has to be NAMEABLE and the
// cancel path has to stop it by name.
func TestDockerExec_ContainerIsStoppableByName(t *testing.T) {
	d := DockerExec{Dir: "/wt", Image: "golang:1.26"}
	args := d.runArgs("dun-42-1", "go test ./...")

	var named string
	for i, a := range args {
		if a == "--name" && i+1 < len(args) {
			named = args[i+1]
		}
	}
	if named != "dun-42-1" {
		t.Fatalf("the run must be named or it cannot be stopped; args=%v", args)
	}
	if joined := strings.Join(args, " "); !strings.Contains(joined, "--network none") {
		t.Errorf("the no-egress default was lost: %v", args)
	}

	stop := dockerStop(named)
	if got := strings.Join(stop.Args, " "); !strings.Contains(got, "stop") || !strings.Contains(got, named) {
		t.Errorf("cancel must stop THAT container: %q", got)
	}

	// Two runs must never share a name, or stopping one stops the other.
	if containerName() == containerName() {
		t.Error("container names collide")
	}
}

func TestExecToolDef(t *testing.T) {
	td := execToolDef()
	if td.Function.Name != "exec" {
		t.Fatalf("name = %q", td.Function.Name)
	}
	params, _ := td.Function.Parameters.(map[string]any)
	req, _ := params["required"].([]string)
	if len(req) != 1 || req[0] != "command" {
		t.Fatalf("exec should require command: %+v", params["required"])
	}
}

// agentDispatch adapts a name→result func to a ToolDispatcher.
func agentDispatch(f func(name string) string) func(context.Context, llm.ToolCall) (string, error) {
	return func(_ context.Context, tc llm.ToolCall) (string, error) {
		return f(tc.Function.Name), nil
	}
}
