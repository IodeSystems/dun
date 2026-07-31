package dun

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	Path       string // the worktree directory (use this as the workspace)
	Branch     string // the branch it's on ("dun/<ts>")
	BaseBranch string // the branch the worktree was created from (e.g. "main")
	repoRoot   string // the origin repo's toplevel ("" when not a git repo)
	// Mounts are the extra local path references resolved for this worktree.
	// Symlinks are created in the worktree parent directory so go.mod replace
	// directives (and equivalent mechanisms in other ecosystems) resolve.
	Mounts []MountSpec
}

// NewWorktree creates a fresh worktree+branch off repoDir's HEAD. If repoDir is
// not inside a git repo, it returns a pass-through Worktree at repoDir (no
// isolation) and isRepo=false.
//
// mounts declares extra local paths that must be accessible from the worktree.
// For each mount, a symlink is created in the worktree parent directory pointing
// to the resolved source path. This is how go.mod replace directives (and
// equivalent mechanisms in other ecosystems) resolve inside the worktree.
func NewWorktree(repoDir string, mounts []MountSpec) (wt *Worktree, isRepo bool, err error) {
	top, terr := git("", "-C", repoDir, "rev-parse", "--show-toplevel")
	if terr != nil {
		return &Worktree{Path: repoDir, Mounts: mounts}, false, nil // not a git repo → work in place
	}
	root := strings.TrimSpace(top)
	// Remember what branch we're on — this is the base the worktree branches from.
	baseBranch, _ := git("", "-C", root, "rev-parse", "--abbrev-ref", "HEAD")
	baseBranch = strings.TrimSpace(baseBranch)
	// Create worktrees under .dun/worktrees/ so the go.mod replace directive
	// ("replace => ../agentkit") resolves via the symlink at
	// .dun/worktrees/agentkit → ../../../agentkit. This keeps every session's
	// files inside the repo tree — no /tmp/ orphans.
	wtParent := filepath.Join(root, ".dun", "worktrees")
	if err := os.MkdirAll(wtParent, 0755); err != nil {
		return nil, false, fmt.Errorf("dun: mkdir worktrees: %w", err)
	}
	// Create symlinks for each mount (idempotent).
	for _, m := range mounts {
		link := filepath.Join(wtParent, m.Name)
		if _, err := os.Lstat(link); err != nil {
			if err := os.Symlink(m.Source, link); err != nil {
				return nil, false, fmt.Errorf("dun: symlink %s: %w", m.Name, err)
			}
		}
	}
	dir, err := os.MkdirTemp(wtParent, "dun-worktree-")
	if err != nil {
		return nil, false, err
	}
	branch := fmt.Sprintf("dun/%d", time.Now().Unix())
	if _, err := git("", "-C", root, "worktree", "add", "-b", branch, dir, "HEAD"); err != nil {
		os.RemoveAll(dir)
		return nil, false, fmt.Errorf("dun: git worktree add: %w", err)
	}
	return &Worktree{Path: dir, Branch: branch, BaseBranch: baseBranch, repoRoot: root, Mounts: mounts}, true, nil
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

// IsClean reports whether the worktree has no uncommitted changes (tracked or
// untracked). Ship requires a clean tree.
func (w *Worktree) IsClean() bool {
	if w.repoRoot == "" {
		return true
	}
	status, err := git("", "-C", w.Path, "status", "--porcelain")
	return err == nil && strings.TrimSpace(status) == ""
}

// UncommittedStatus returns the porcelain status of uncommitted changes, or ""
// when clean. Used by ship to report what needs committing first.
func (w *Worktree) UncommittedStatus() string {
	if w.repoRoot == "" {
		return ""
	}
	status, err := git("", "-C", w.Path, "status", "--porcelain", "-b")
	if err != nil {
		return err.Error()
	}
	return strings.TrimSpace(status)
}

// Commit stages all changes and commits them with the given message. Returns
// the commit hash on success, or an error string. "nothing to commit" is not
// an error — it means the tree was already clean at that point.
func (w *Worktree) Commit(message string) string {
	if w.repoRoot == "" {
		return "not a git repo"
	}
	if _, err := git("", "-C", w.Path, "add", "-A"); err != nil {
		return "git add: " + err.Error()
	}
	out, err := git("", "-C", w.Path, "commit", "-m", message)
	if err != nil {
		if strings.Contains(out, "nothing to commit") {
			return "already clean — nothing to commit"
		}
		return "git commit: " + err.Error()
	}
	// Extract the short commit hash from the output (first line: "[branch hash (commit type)] message")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) > 0 {
		return lines[0]
	}
	return out
}

// Fetch updates origin/<baseBranch> from the remote. Returns output or error.
func (w *Worktree) Fetch() string {
	if w.repoRoot == "" {
		return "not a git repo"
	}
	out, err := git("", "-C", w.Path, "fetch", "origin", w.BaseBranch)
	if err != nil {
		return "git fetch: " + err.Error()
	}
	return strings.TrimSpace(out)
}

// RebaseOnto rebases the current worktree branch onto the given upstream ref
// (e.g. "origin/main"). Returns output or an error string. If the rebase
// fails with conflicts, the error message contains the conflict info and the
// worktree is left in a mid-rebase state for the agent to resolve.
func (w *Worktree) RebaseOnto(upstream string) string {
	if w.repoRoot == "" {
		return "not a git repo"
	}
	out, err := git("", "-C", w.Path, "rebase", upstream)
	if err != nil {
		return "git rebase: " + err.Error()
	}
	return strings.TrimSpace(out)
}

// RebaseContinue stages all files and continues a rebase after conflict resolution.
func (w *Worktree) RebaseContinue() string {
	if w.repoRoot == "" {
		return "not a git repo"
	}
	if _, err := git("", "-C", w.Path, "add", "-A"); err != nil {
		return "git add: " + err.Error()
	}
	out, err := git("", "-C", w.Path, "rebase", "--continue")
	if err != nil {
		return "git rebase --continue: " + err.Error()
	}
	return strings.TrimSpace(out)
}

// PushWithUpstream pushes the current branch to origin and sets upstream tracking.
func (w *Worktree) PushWithUpstream() string {
	if w.repoRoot == "" {
		return "not a git repo"
	}
	out, err := git("", "-C", w.Path, "push", "-u", "origin", w.Branch)
	if err != nil {
		return "git push: " + err.Error()
	}
	return strings.TrimSpace(out)
}

// FastForwardLocal fast-forwards the local base branch (e.g. "main") to match
// the worktree branch. This integrates the worktree's commits into the local
// base branch. Returns an error if the base branch is not a strict ancestor
// (use rebase first).
func (w *Worktree) FastForwardLocal() string {
	if w.repoRoot == "" {
		return "not a git repo"
	}
	// Check out the base branch in the main worktree and ff it to our branch.
	out, err := git("", "-C", w.repoRoot, "checkout", w.BaseBranch)
	if err != nil {
		return "checkout base: " + err.Error()
	}
	out, err = git("", "-C", w.repoRoot, "merge", "--ff-only", w.Branch)
	if err != nil {
		// Clean up: switch back. The ff-only failed, which is expected if
		// main has diverged. The caller should handle this.
		return "merge --ff-only: " + err.Error()
	}
	return strings.TrimSpace(out)
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
