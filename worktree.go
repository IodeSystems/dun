package dun

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Isolation, tier 1 — the git worktree.
//
// The agent's FILE changes (poly-lsp node_edit, mcpshell file writes) land in a
// throwaway worktree on a fresh branch, never on the repo's checked-out branch.
// At the end the diff is reviewable and the branch can become a PR (Slice 4).
// If the workspace isn't a git repo, dun works in place (no isolation) with a
// warning — nothing to branch from.

// Worktree is a git worktree dedicated to one dun session.
type Worktree struct {
	Path     string // the worktree directory (use this as the workspace)
	Branch   string // the branch it's on ("" when not a git repo)
	repoRoot string // the origin repo's toplevel ("" when not a git repo)
}

// NewWorktree creates a fresh worktree+branch off repoDir's HEAD. If repoDir is
// not inside a git repo, it returns a pass-through Worktree at repoDir (no
// isolation) and isRepo=false.
func NewWorktree(repoDir string) (wt *Worktree, isRepo bool, err error) {
	top, terr := git("", "-C", repoDir, "rev-parse", "--show-toplevel")
	if terr != nil {
		return &Worktree{Path: repoDir}, false, nil // not a git repo → work in place
	}
	root := strings.TrimSpace(top)
	dir, err := os.MkdirTemp("", "dun-worktree-")
	if err != nil {
		return nil, false, err
	}
	branch := fmt.Sprintf("dun/%d", time.Now().Unix())
	if _, err := git("", "-C", root, "worktree", "add", "-b", branch, dir, "HEAD"); err != nil {
		os.RemoveAll(dir)
		return nil, false, fmt.Errorf("dun: git worktree add: %w", err)
	}
	return &Worktree{Path: dir, Branch: branch, repoRoot: root}, true, nil
}

// Diff returns the worktree's changes vs its base (tracked + a list of untracked
// files). Empty when nothing changed.
func (w *Worktree) Diff() string {
	if w.repoRoot == "" {
		return ""
	}
	diff, _ := git("", "-C", w.Path, "diff")
	untracked, _ := git("", "-C", w.Path, "ls-files", "--others", "--exclude-standard")
	out := diff
	if strings.TrimSpace(untracked) != "" {
		out += "\n--- untracked ---\n" + untracked
	}
	return out
}

// Cleanup removes the worktree (a no-op for a pass-through). The branch is kept
// so the work isn't lost — remove it with `git branch -D <branch>` if unwanted.
func (w *Worktree) Cleanup() {
	if w.repoRoot == "" {
		return
	}
	_, _ = git("", "-C", w.repoRoot, "worktree", "remove", "--force", w.Path)
}

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), err)
	}
	return string(out), nil
}
