package dun

// The AGENTS.md guard: the hook script must cancel an operation when an
// AGENTS.md exists in the target's directory chain, and let it through when
// one does not, or when the target IS the AGENTS.md.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runHook writes the embedded script to a temp file and runs it with the
// given op and target path. Returns the exit code and stdout.
func runHook(t *testing.T, op, target string) (int, string) {
	t.Helper()
	path, err := writeAgentsHook()
	if err != nil {
		t.Fatalf("writeAgentsHook: %v", err)
	}
	defer os.Remove(path)

	cmd := exec.Command(path, op, target)
	cmd.Dir = filepath.Dir(target)
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			t.Fatalf("running hook: %v\n%s", err, out)
		}
	}
	return code, string(out)
}

// setupWorktree creates a minimal git worktree with the given files.
func setupWorktree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	// Initialize a git repo so the hook finds a .git.
	git := exec.Command("git", "init", "-q", dir)
	git.Dir = dir
	git.Run()
	must := func(name, content string) {
		t.Helper()
		p := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for name, content := range files {
		must(name, content)
	}
	// Commit so .git exists.
	cmd := exec.Command("git", "add", "-A")
	cmd.Dir = dir
	cmd.Run()
	cmd = exec.Command("git", "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-qm", "init")
	cmd.Dir = dir
	cmd.Run()
	return dir
}

func TestAgentsHook_NoAGENTSmd(t *testing.T) {
	dir := setupWorktree(t, map[string]string{"main.go": "package main"})
	code, out := runHook(t, "read", filepath.Join(dir, "main.go"))
	if code != 0 {
		t.Errorf("no AGENTS.md should proceed: exit %d, out: %s", code, out)
	}
}

func TestAgentsHook_RootAGENTSmd(t *testing.T) {
	dir := setupWorktree(t, map[string]string{
		"AGENTS.md": "always use tabs",
		"main.go":   "package main",
	})
	code, out := runHook(t, "edit", filepath.Join(dir, "main.go"))
	if code != 2 {
		t.Errorf("root AGENTS.md should cancel: exit %d, out: %s", code, out)
	}
	if !strings.Contains(out, "AGENTS.md") {
		t.Errorf("note should mention AGENTS.md: %s", out)
	}
}

func TestAgentsHook_NestedAGENTSmd(t *testing.T) {
	dir := setupWorktree(t, map[string]string{
		"src/AGENTS.md": "use gofmt",
		"src/foo.go":    "package src",
		"main.go":       "package main",
	})
	// A file under src/ should be blocked by src/AGENTS.md.
	code, out := runHook(t, "read", filepath.Join(dir, "src", "foo.go"))
	if code != 2 {
		t.Errorf("nested AGENTS.md should cancel: exit %d, out: %s", code, out)
	}
	// A file at the root should NOT be blocked by src/AGENTS.md.
	code, out = runHook(t, "read", filepath.Join(dir, "main.go"))
	if code != 0 {
		t.Errorf("root file should not be blocked by nested AGENTS.md: exit %d, out: %s", code, out)
	}
}

func TestAgentsHook_ReadingAGENTSmdItself(t *testing.T) {
	dir := setupWorktree(t, map[string]string{
		"AGENTS.md": "rules",
		"main.go":   "package main",
	})
	code, _ := runHook(t, "read", filepath.Join(dir, "AGENTS.md"))
	if code != 0 {
		t.Errorf("reading AGENTS.md itself should proceed: exit %d", code)
	}
	code, _ = runHook(t, "edit", filepath.Join(dir, "AGENTS.md"))
	if code != 0 {
		t.Errorf("editing AGENTS.md itself should proceed: exit %d", code)
	}
}

func TestAgentsHook_CreateOp(t *testing.T) {
	dir := setupWorktree(t, map[string]string{
		"AGENTS.md": "rules",
		"main.go":   "package main",
	})
	code, out := runHook(t, "create", filepath.Join(dir, "newfile.go"))
	if code != 2 {
		t.Errorf("create with AGENTS.md should cancel: exit %d, out: %s", code, out)
	}
}

func TestAgentsHook_NoGit(t *testing.T) {
	// A directory with no .git: the hook should proceed.
	dir := t.TempDir()
	p := filepath.Join(dir, "test.go")
	os.WriteFile(p, []byte("package test"), 0o644)
	code, out := runHook(t, "read", p)
	if code != 0 {
		t.Errorf("no .git should proceed: exit %d, out: %s", code, out)
	}
}
