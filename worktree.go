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

// RepoRoot is the top of the git repo containing dir, or "" when there is none.
// Housekeeping needs it before any Worktree exists.
func RepoRoot(dir string) string {
	out, err := git("", "-C", dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// WorktreeInPlace creates a pass-through Worktree pointing at repoDir itself.
// It detects the git repo root and current branch without creating an actual
// git worktree. Used when --ship is passed without --worktree: the agent works
// in the checked-out directory but still needs ship's verify pipeline.
func WorktreeInPlace(repoDir string) (wt *Worktree, isRepo bool) {
	top, terr := git("", "-C", repoDir, "rev-parse", "--show-toplevel")
	if terr != nil {
		return &Worktree{Path: repoDir}, false // not a git repo
	}
	root := strings.TrimSpace(top)
	baseBranch, _ := git("", "-C", root, "rev-parse", "--abbrev-ref", "HEAD")
	baseBranch = strings.TrimSpace(baseBranch)
	return &Worktree{Path: repoDir, BaseBranch: baseBranch, repoRoot: root}, true
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

// pendingDiff is everything a commit would take: the diff against HEAD (staged
// and unstaged together, since `add -A` is about to make that distinction moot)
// plus untracked files BY NAME.
//
// Untracked contents are deliberately omitted. A new file is usually the
// largest thing in a change and the least informative per byte — its name and
// its presence already say "this was added" — so including it is how a diff
// budget gets spent on a vendored directory instead of on the change.
//
// limit bounds the whole thing; 0 means unbounded.
func (w *Worktree) pendingDiff(limit int) string {
	if w.repoRoot == "" {
		return ""
	}
	stat, _ := git("", "-C", w.Path, "diff", "HEAD", "--stat")
	diff, _ := git("", "-C", w.Path, "diff", "HEAD")
	untracked, _ := git("", "-C", w.Path, "ls-files", "--others", "--exclude-standard")

	var b strings.Builder
	if s := strings.TrimSpace(stat); s != "" {
		b.WriteString("diff vs HEAD --stat:\n" + s + "\n\n")
	}
	if s := strings.TrimSpace(untracked); s != "" {
		b.WriteString("untracked files (contents not shown):\n" + s + "\n\n")
	}
	body := strings.TrimSpace(diff)
	// The stat and the file list go in whole and the DIFF is what gets cut: they
	// are the summary that has to survive for a truncated brief to be usable.
	if limit > 0 {
		if room := limit - b.Len(); room <= 0 {
			body = ""
		} else if len(body) > room {
			body = body[:room] + fmt.Sprintf("\n[…diff truncated at %d of %d characters — "+
				"the --stat above is the full shape of the change]", room, len(body))
		}
	}
	if body != "" {
		b.WriteString("diff vs HEAD:\n" + body)
	}
	return strings.TrimSpace(b.String())
}

// Status is the human's `git status` for this worktree: the branch line and the
// changed files, exactly as porcelain reports them, or "clean".
//
// It exists because `/worktree status` had nothing to say when dun works in
// place (the common case — worktree isolation is opt-in): "none (working in
// place)" answered a question nobody asked, while the one they did ask — what
// is uncommitted here — needed a separate trip to a shell.
func (w *Worktree) Status() string {
	if w == nil || w.repoRoot == "" {
		return "not a git repository"
	}
	status := w.UncommittedStatus()
	lines := strings.Split(status, "\n")
	// Porcelain -b always leads with "## branch...upstream"; keep it as the
	// header and count only the file lines, or "3 changed" counts the branch.
	head, files := "", []string{}
	for _, l := range lines {
		switch {
		case strings.HasPrefix(l, "## "):
			head = strings.TrimPrefix(l, "## ")
		case strings.TrimSpace(l) != "":
			files = append(files, l)
		}
	}
	if head == "" {
		head = w.CurrentBranch()
	}
	if len(files) == 0 {
		return head + "\nclean"
	}
	return fmt.Sprintf("%s\n%s\n%d file%s changed", head, strings.Join(files, "\n"), len(files), plural(len(files)))
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

// Upstream is the remote-tracking branch of branch ("origin/main"), or "" when
// it has none. This is what decides whether ship has anything to verify
// against: a branch nobody has pushed has no remote history to rebase onto.
func (w *Worktree) Upstream(branch string) string {
	if w.repoRoot == "" {
		return ""
	}
	out, err := git("", "-C", w.Path, "rev-parse", "--abbrev-ref", "--symbolic-full-name", branch+"@{upstream}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// Fetch updates origin/<branch> from the remote. Returns output or error.
func (w *Worktree) Fetch(branch string) string {
	if w.repoRoot == "" {
		return "not a git repo"
	}
	if branch == "" {
		branch = w.BaseBranch
	}
	out, err := git("", "-C", w.Path, "fetch", "origin", branch)
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

// IsRepo reports whether this worktree is inside a git repo. Ship needs one;
// the branch it is ON is a separate question (a --no-worktree session sits on
// the base branch, which ship still verifies).
func (w *Worktree) IsRepo() bool { return w.repoRoot != "" }

// RepoRoot returns the top-level directory of the git repo, or "" when not a
// repo. Used to resolve relative mount paths (e.g. ../agentkit from go.mod
// replace) against the origin checkout rather than the throwaway worktree.
func (w *Worktree) RepoRoot() string { return w.repoRoot }

// CurrentBranch is the branch HEAD is on, or "" when detached or not a repo.
// Ship asks git rather than trusting w.Branch: the agent can switch branches
// with exec, and shipping the wrong ref is not a recoverable mistake.
func (w *Worktree) CurrentBranch() string {
	if w.repoRoot == "" {
		return ""
	}
	out, err := git("", "-C", w.Path, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return ""
	}
	if b := strings.TrimSpace(out); b != "HEAD" {
		return b
	}
	return "" // detached
}

// RebaseInProgress reports whether a rebase is stopped mid-way. This is what
// lets ship RESUME instead of making the model declare that it should.
func (w *Worktree) RebaseInProgress() bool {
	if w.repoRoot == "" {
		return false
	}
	for _, name := range []string{"rebase-merge", "rebase-apply"} {
		out, err := git("", "-C", w.Path, "rev-parse", "--git-path", name)
		if err != nil {
			continue
		}
		p := strings.TrimSpace(out)
		if !filepath.IsAbs(p) {
			p = filepath.Join(w.Path, p)
		}
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// RemoteHead is the sha of origin/<branch>, or "" if there is no such ref.
// Ship compares it before and after the checks to catch a base that moved.
func (w *Worktree) RemoteHead(branch string) string {
	if w.repoRoot == "" {
		return ""
	}
	out, err := git("", "-C", w.Path, "rev-parse", "--verify", "--quiet", "origin/"+branch)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// NeedsPush reports whether HEAD differs from origin/<branch>. A branch that
// was never pushed needs one.
func (w *Worktree) NeedsPush(branch string) bool {
	if w.repoRoot == "" {
		return false
	}
	remote := w.RemoteHead(branch)
	if remote == "" {
		return true
	}
	head, err := git("", "-C", w.Path, "rev-parse", "HEAD")
	if err != nil {
		return true
	}
	return strings.TrimSpace(head) != remote
}

// Push pushes branch to origin, setting upstream tracking.
//
// lease sends --force-with-lease, which is REQUIRED after a rebase (the branch
// no longer fast-forwards) and is a compare-and-swap, not a blind force: if
// someone else moved the ref since our last fetch, the push is refused. Never
// pass it for the base branch — a trunk must only ever fast-forward.
func (w *Worktree) Push(branch string, lease bool) string {
	if w.repoRoot == "" {
		return "not a git repo"
	}
	args := []string{"-C", w.Path, "push"}
	if lease {
		args = append(args, "--force-with-lease")
	}
	args = append(args, "-u", "origin", branch)
	out, err := git("", args...)
	if err != nil {
		return "git push: " + err.Error()
	}
	return strings.TrimSpace(out)
}

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	detach(cmd) // fetch/push must never reach dun's terminal — see detach.go
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), err)
	}
	return string(out), nil
}

// ReuseWorktree reuses a previous session's worktree. If the old worktree
// directory still exists and is valid, it returns it directly. If the directory
// was cleaned up but the branch still exists, it creates a new worktree from
// that branch (preserving the commits). Falls back to NewWorktree if neither
// works.
func ReuseWorktree(repoDir string, oldPath, oldBranch string, mounts []MountSpec) (wt *Worktree, isRepo bool, err error) {
	top, terr := git("", "-C", repoDir, "rev-parse", "--show-toplevel")
	if terr != nil {
		return &Worktree{Path: repoDir, Mounts: mounts}, false, nil
	}
	root := strings.TrimSpace(top)

	// Case 1: old worktree directory still exists and is registered
	if oldPath != "" {
		if _, err := os.Stat(oldPath); err == nil {
			out, _ := git("", "-C", root, "worktree", "list", "--porcelain")
			if strings.Contains(out, oldPath) {
				baseBranch, _ := git("", "-C", root, "rev-parse", "--abbrev-ref", "HEAD")
				baseBranch = strings.TrimSpace(baseBranch)
				return &Worktree{Path: oldPath, Branch: oldBranch, BaseBranch: baseBranch, repoRoot: root, Mounts: mounts}, true, nil
			}
		}
	}

	// Case 2: branch still exists — create a new worktree from it
	if oldBranch != "" {
		out, err := git("", "-C", root, "rev-parse", "--verify", oldBranch)
		if err == nil && strings.TrimSpace(out) != "" {
			wtParent := filepath.Join(root, ".dun", "worktrees")
			os.MkdirAll(wtParent, 0755)
			for _, m := range mounts {
				link := filepath.Join(wtParent, m.Name)
				if _, err := os.Lstat(link); err != nil {
					os.Symlink(m.Source, link)
				}
			}
			dir, err := os.MkdirTemp(wtParent, "dun-worktree-")
			if err != nil {
				return nil, false, err
			}
			if _, err := git("", "-C", root, "worktree", "add", dir, oldBranch); err != nil {
				os.RemoveAll(dir)
				return nil, false, fmt.Errorf("dun: git worktree add (reuse): %w", err)
			}
			baseBranch, _ := git("", "-C", root, "rev-parse", "--abbrev-ref", "HEAD")
			baseBranch = strings.TrimSpace(baseBranch)
			return &Worktree{Path: dir, Branch: oldBranch, BaseBranch: baseBranch, repoRoot: root, Mounts: mounts}, true, nil
		}
	}

	// Case 3: fallback — create a fresh worktree
	return NewWorktree(repoDir, mounts)
}
