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
	Path     string // the worktree directory (use this as the workspace)
	Branch   string // the branch it's on ("" when not a git repo)
	repoRoot string // the origin repo's toplevel ("" when not a git repo)
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
	return &Worktree{Path: dir, Branch: branch, repoRoot: root, Mounts: mounts}, true, nil
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
