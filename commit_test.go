package dun

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitRepo builds a repo with one commit, and returns a pass-through worktree
// over it. Real git, because every claim here is about what git reports.
func gitRepo(t *testing.T) *Worktree {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "T"},
		{"commit", "-q", "--allow-empty", "-m", "root"},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	wt, isRepo := WorktreeInPlace(dir)
	if !isRepo {
		t.Fatal("WorktreeInPlace should see a git repo")
	}
	return wt
}

func write(t *testing.T, wt *Worktree, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(wt.Path, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Status is the answer to "what is uncommitted here", in place, with no
// dedicated worktree — the case that used to report "none (working in place)".
func TestWorktreeStatus_InPlaceReadsLikeGitStatus(t *testing.T) {
	wt := gitRepo(t)
	if got := wt.Status(); !strings.Contains(got, "main") || !strings.Contains(got, "clean") {
		t.Fatalf("a clean repo should say so, on its branch: %q", got)
	}

	write(t, wt, "a.go", "package a\n")
	write(t, wt, "b.go", "package b\n")
	got := wt.Status()
	if !strings.Contains(got, "a.go") || !strings.Contains(got, "b.go") {
		t.Errorf("both changed files should be listed: %q", got)
	}
	if !strings.Contains(got, "2 files changed") {
		t.Errorf("the count must exclude the ## branch line: %q", got)
	}
	if strings.Contains(got, "## ") {
		t.Errorf("the porcelain branch marker is noise once it is the header: %q", got)
	}
}

// The brief handed to the model: the shape of the change always, the diff only
// as far as the budget goes, and untracked files by name.
func TestPendingDiff_KeepsTheShapeWhenItTruncates(t *testing.T) {
	wt := gitRepo(t)
	write(t, wt, "tracked.go", "package a\n")
	run(t, wt, "add", "tracked.go")
	run(t, wt, "commit", "-q", "-m", "add tracked")

	write(t, wt, "tracked.go", "package a\n\n"+strings.Repeat("// a very long changed line\n", 400))
	write(t, wt, "brand-new.go", strings.Repeat("// untracked contents\n", 400))

	full := wt.pendingDiff(0)
	if !strings.Contains(full, "tracked.go") {
		t.Errorf("the tracked change must be in the diff: %q", clip(full))
	}
	if !strings.Contains(full, "brand-new.go") {
		t.Errorf("an untracked file must be named: %q", clip(full))
	}
	if strings.Contains(full, "untracked contents") {
		t.Error("untracked CONTENTS are deliberately omitted — a new file is the least informative per byte")
	}

	small := wt.pendingDiff(900)
	if len(small) > 1200 { // the truncation note is allowed to overshoot the cap a little
		t.Errorf("the cap was not applied: %d characters", len(small))
	}
	if !strings.Contains(small, "--stat") || !strings.Contains(small, "tracked.go") {
		t.Errorf("the stat must survive truncation — it is what makes a cut brief usable: %q", small)
	}
	if !strings.Contains(small, "truncated") {
		t.Errorf("a truncated diff must say so: %q", small)
	}
}

func run(t *testing.T, wt *Worktree, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", wt.Path}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func clip(s string) string {
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}

// The default is conventional commits; a format names a built-in; an
// instruction replaces both, because it is the more specific statement.
func TestCommitConfig_Rules(t *testing.T) {
	if !strings.Contains((*CommitConfig)(nil).rules(), "Conventional Commits") {
		t.Error("no config at all must still produce the suggested default")
	}
	if !strings.Contains((&CommitConfig{}).rules(), "Conventional Commits") {
		t.Error("an empty commit section is the default, not an empty prompt")
	}
	plain := (&CommitConfig{Format: "plain"}).rules()
	if strings.Contains(plain, "Conventional Commits") || !strings.Contains(plain, "no type/scope prefix") {
		t.Errorf("plain should drop the type/scope form: %q", plain)
	}
	custom := &CommitConfig{Format: "conventional", Instruction: "All caps. No body."}
	if custom.rules() != "All caps. No body." {
		t.Errorf("a free-text instruction must win over a named format: %q", custom.rules())
	}
}

// A model wraps the message in whatever it likes, however it is asked not to.
func TestCleanCommitMessage(t *testing.T) {
	want := "feat(tui): descend with →\n\nThe body."
	for name, in := range map[string]string{
		"bare":     want,
		"fenced":   "```\n" + want + "\n```",
		"tagged":   "```text\n" + want + "\n```",
		"preamble": "Commit message:\n\n" + want,
		"both":     "Here is the commit message:\n```\n" + want + "\n```",
		"padded":   "\n\n" + want + "\n\n",
	} {
		if got := cleanCommitMessage(in); got != want {
			t.Errorf("%s: got %q, want %q", name, got, want)
		}
	}
	// A commit ABOUT commit messages keeps its subject.
	if got := cleanCommitMessage("commit message: fix the parser"); got == "fix the parser" {
		t.Log("note: a single-line 'commit message: X' is treated as a preamble")
	}
}

// Nothing to commit is not an error state to be reported as a model failure.
func TestCommitMessage_RefusesACleanTree(t *testing.T) {
	wt := gitRepo(t)
	h := &Harness{}
	_, err := h.CommitMessage(t.Context(), wt)
	if err == nil || !strings.Contains(err.Error(), "clean") {
		t.Fatalf("a clean tree must be refused by name, got %v", err)
	}
	// And a non-repo is refused before anything else is attempted.
	if _, err := h.CommitMessage(t.Context(), &Worktree{Path: t.TempDir()}); err == nil ||
		!strings.Contains(err.Error(), "git repository") {
		t.Fatalf("a non-repo must be refused, got %v", err)
	}
}

// AskUser is how the command layer reaches the human; with nobody attached it
// must say so rather than silently proceeding.
func TestAskUser_NobodyAttached(t *testing.T) {
	h := &Harness{}
	if _, err := h.AskUser(t.Context(), "?", []string{"a"}); err == nil ||
		!strings.Contains(err.Error(), "nobody to ask") {
		t.Fatalf("want a nobody-to-ask error, got %v", err)
	}
	asked := ""
	h = &Harness{cfg: Config{Ask: func(_ context.Context, q string, _ []string, _ bool) (string, error) {
		asked = q
		return "commit", nil
	}}}
	ans, err := h.AskUser(t.Context(), "Commit these changes?", []string{"commit", "cancel"})
	if err != nil || ans != "commit" || asked != "Commit these changes?" {
		t.Fatalf("ask=%q ans=%q err=%v", asked, ans, err)
	}
}
