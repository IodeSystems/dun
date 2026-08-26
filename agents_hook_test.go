package dun

// The AGENTS.md guard: the hook script must cancel an operation when an
// AGENTS.md exists in the target's directory chain, and let it through when
// one does not, or when the target IS the AGENTS.md.

import (
	"time"
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

// TestAgentsHook_InjectReadRetry is the critical flow: the model is blocked,
// reads the AGENTS.md, and the retry succeeds. Without session memory, the
// retry would be blocked again — the model would be trapped.
func TestAgentsHook_InjectReadRetry(t *testing.T) {
	dir := setupWorktree(t, map[string]string{
		"AGENTS.md": "always use tabs",
		"main.go":   "package main",
	})
	target := filepath.Join(dir, "main.go")

	// 1. First access: blocked.
	code, out := runHook(t, "read", target)
	if code != 2 {
		t.Fatalf("first access should be blocked: exit %d, out: %s", code, out)
	}

	// 2. Read the AGENTS.md: allowed, and marks it as read.
	code, _ = runHook(t, "read", filepath.Join(dir, "AGENTS.md"))
	if code != 0 {
		t.Fatalf("reading AGENTS.md should proceed: exit %d", code)
	}

	// 3. Retry the original access: should now succeed.
	code, out = runHook(t, "read", target)
	if code != 0 {
		t.Errorf("retry after reading AGENTS.md should proceed: exit %d, out: %s", code, out)
	}
}

// TestAgentsHook_EditAfterRead: editing is also unblocked after the read.
func TestAgentsHook_EditAfterRead(t *testing.T) {
	dir := setupWorktree(t, map[string]string{
		"AGENTS.md": "rules",
		"main.go":   "package main",
	})
	target := filepath.Join(dir, "main.go")

	// Blocked for edit.
	code, _ := runHook(t, "edit", target)
	if code != 2 {
		t.Fatalf("edit should be blocked: exit %d", code)
	}

	// Read the AGENTS.md.
	runHook(t, "read", filepath.Join(dir, "AGENTS.md"))

	// Edit now proceeds.
	code, out := runHook(t, "edit", target)
	if code != 0 {
		t.Errorf("edit after reading AGENTS.md should proceed: exit %d, out: %s", code, out)
	}
}

// TestAgentsHook_StateFileIsPerWorkspace: two workspaces do not share state.
func TestAgentsHook_StateFileIsPerWorkspace(t *testing.T) {
	dir1 := setupWorktree(t, map[string]string{
		"AGENTS.md": "rules1",
		"main.go":   "package main",
	})
	dir2 := setupWorktree(t, map[string]string{
		"AGENTS.md": "rules2",
		"main.go":   "package main",
	})

	// Read AGENTS.md in workspace 1.
	runHook(t, "read", filepath.Join(dir1, "AGENTS.md"))

	// Workspace 1 is unblocked.
	code, _ := runHook(t, "read", filepath.Join(dir1, "main.go"))
	if code != 0 {
		t.Errorf("workspace 1 should be unblocked after read: exit %d", code)
	}

	// Workspace 2 is still blocked (different .dun/agents_read).
	code, _ = runHook(t, "read", filepath.Join(dir2, "main.go"))
	if code != 2 {
		t.Errorf("workspace 2 should still be blocked: exit %d", code)
	}
}

// TestAgentsHook_MtimeInvalidation: if the AGENTS.md is modified after being
// read, the guard re-engages (the mtime no longer matches the state file).
func TestAgentsHook_MtimeInvalidation(t *testing.T) {
	dir := setupWorktree(t, map[string]string{
		"AGENTS.md": "old rules",
		"main.go":   "package main",
	})
	target := filepath.Join(dir, "main.go")

	// Read the AGENTS.md.
	runHook(t, "read", filepath.Join(dir, "AGENTS.md"))

	// Now it's unblocked.
	code, _ := runHook(t, "read", target)
	if code != 0 {
		t.Fatalf("should be unblocked after read: exit %d", code)
	}

	// Modify the AGENTS.md (changes mtime).
	time.Sleep(1100 * time.Millisecond) // ensure mtime changes
	os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("new rules"), 0o644)

	// Now it's blocked again.
	code, out := runHook(t, "read", target)
	if code != 2 {
		t.Errorf("should be re-blocked after AGENTS.md changes: exit %d, out: %s", code, out)
	}
}

// rootAgentsMDBlock: the system-prompt injection of the workspace's root
// AGENTS.md. This is the "always show the root AGENTS.md" default — the rules
// are standing context, not something that waits for a guard to fire.
func TestRootAgentsMDBlock(t *testing.T) {
	// No workspace: nothing.
	if got := rootAgentsMDBlock(""); got != "" {
		t.Errorf("empty workspace should give no block: %q", got)
	}

	// Workspace with no AGENTS.md: nothing.
	if got := rootAgentsMDBlock(t.TempDir()); got != "" {
		t.Errorf("no AGENTS.md should give no block: %q", got)
	}

	// Workspace with a root AGENTS.md: the content is in the block.
	ws := t.TempDir()
	os.WriteFile(filepath.Join(ws, "AGENTS.md"), []byte("always use tabs"), 0o644)
	got := rootAgentsMDBlock(ws)
	if !strings.Contains(got, "always use tabs") {
		t.Errorf("block should carry the file content: %q", got)
	}
	if !strings.Contains(got, "AGENTS.md") {
		t.Errorf("block should name the source: %q", got)
	}

	// Lowercase agents.md is also honored.
	ws2 := t.TempDir()
	os.WriteFile(filepath.Join(ws2, "agents.md"), []byte("use gofmt"), 0o644)
	got = rootAgentsMDBlock(ws2)
	if !strings.Contains(got, "use gofmt") {
		t.Errorf("lowercase agents.md should be read: %q", got)
	}

	// A NESTED AGENTS.md is NOT the root: it must not appear in the block.
	// (The guard surfaces it on access; the system prompt only carries the root.)
	ws3 := t.TempDir()
	os.MkdirAll(filepath.Join(ws3, "src"), 0o755)
	os.WriteFile(filepath.Join(ws3, "src", "AGENTS.md"), []byte("nested only"), 0o644)
	got = rootAgentsMDBlock(ws3)
	if strings.Contains(got, "nested only") {
		t.Errorf("a nested AGENTS.md must not appear in the root block: %q", got)
	}
}
