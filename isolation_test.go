package dun

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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

	wt, isRepo, err := NewWorktree(repo)
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

func TestWorktree_NonRepoPassThrough(t *testing.T) {
	dir := t.TempDir()
	wt, isRepo, err := NewWorktree(dir)
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
	out := HostExec{Dir: dir}.Run(context.Background(), "ls")
	if !strings.Contains(out, "x.txt") {
		t.Fatalf("exec ls should list x.txt: %q", out)
	}
	// A non-zero exit is reported in the output, not swallowed.
	e := (HostExec{Dir: dir}).Run(context.Background(), "exit 3")
	if !strings.Contains(e, "exit") {
		t.Fatalf("non-zero exit should be reported: %q", e)
	}
}

func TestWithExec_RoutesExecLocallyElseMCP(t *testing.T) {
	inner := agentDispatch(func(name string) string { return "MCP:" + name })
	d := withExec(inner, HostExec{Dir: t.TempDir()}, nil)

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
