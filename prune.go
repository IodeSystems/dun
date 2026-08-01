package dun

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Worktree lifecycle — what happens to a session's tree after the session.
//
// A worktree is created per SESSION (not per agent: a sub-agent inherits its
// parent's tree, which is why there is no second index and no second server).
// Nothing removed them. Measured on dun's own repo: 37 registered, 36 on disk,
// 1.1 GB, and one registration already pointing at a directory that no longer
// existed.
//
// The rule for what may be deleted has to be "holds no WORK", not "git status
// is clean". By the clean-tree test 34 of those 37 looked dirty — almost all of
// them from a single untracked index artifact — so a status-based prune would
// have reclaimed three of thirty-seven. Judged by commits and tracked edits, 24
// were disposable.
//
// Nothing that holds work is ever removed automatically. It is REPORTED, which
// is the only safe default for a branch whose commits may exist nowhere else.

// liveGrace protects a worktree that something is still using.
//
// PruneWorktrees is told which tree belongs to the CALLING session, but a
// machine routinely runs several dun sessions at once (this one had four while
// the pruner was being written), and a concurrent session's tree holding
// nothing yet is exactly the tree that would be deleted out from under it. A
// worktree being worked in is touched constantly, so recency stands in for
// liveness: it is a heuristic, and it is deliberately generous, because the
// cost of waiting an hour to reclaim disk is nothing and the cost of deleting
// a running session's tree is its work.
const liveGrace = time.Hour

// artifactPaths are files a tool wrote into the worktree that are not the
// agent's work. They are ignored when deciding whether a tree holds anything:
// an index cache is not a reason to keep a gigabyte.
var artifactPaths = []string{".dun/", ".poly-lsp-mcp/"}

// WorktreeInfo is one session worktree and what it is holding.
type WorktreeInfo struct {
	Path    string
	Branch  string
	Commits int      // commits on this branch not reachable from the repo's HEAD
	Dirty   []string // modified/added TRACKED paths, artifacts excluded
	Age     time.Duration
	Bytes   int64
	// Missing means git still has a registration for a directory that is gone.
	Missing bool
}

// HoldsWork is the only question that decides whether a tree may be deleted.
func (w WorktreeInfo) HoldsWork() bool { return w.Commits > 0 || len(w.Dirty) > 0 }

// Summary is the one-line description used in the abandoned-worktree report.
func (w WorktreeInfo) Summary() string {
	var what []string
	if w.Commits > 0 {
		what = append(what, fmt.Sprintf("%d commit%s", w.Commits, plural(w.Commits)))
	}
	if len(w.Dirty) > 0 {
		s := w.Dirty[0]
		if len(w.Dirty) > 1 {
			s += fmt.Sprintf(" +%d more", len(w.Dirty)-1)
		}
		what = append(what, s)
	}
	if len(what) == 0 {
		what = append(what, "nothing")
	}
	return fmt.Sprintf("%-28s %-22s %s", w.Branch, strings.Join(what, ", "), roundAge(w.Age))
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func roundAge(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
}

// ScanWorktrees lists dun's session worktrees under repoRoot and what each is
// holding. Worktrees dun did not create are ignored entirely — this must never
// touch a tree a human made.
func ScanWorktrees(repoRoot string) []WorktreeInfo {
	if repoRoot == "" {
		return nil
	}
	out, err := git("", "-C", repoRoot, "worktree", "list", "--porcelain")
	if err != nil {
		return nil
	}
	ours := filepath.Join(repoRoot, ".dun", "worktrees") + string(os.PathSeparator)

	var list []WorktreeInfo
	var cur WorktreeInfo
	flush := func() {
		if cur.Path != "" && strings.HasPrefix(cur.Path, ours) {
			list = append(list, cur)
		}
		cur = WorktreeInfo{}
	}
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			flush()
			cur.Path = strings.TrimPrefix(line, "worktree ")
		case strings.HasPrefix(line, "branch "):
			cur.Branch = strings.TrimPrefix(strings.TrimPrefix(line, "branch "), "refs/heads/")
		}
	}
	flush()

	for i := range list {
		fill(repoRoot, &list[i])
	}
	sort.Slice(list, func(a, b int) bool { return list[a].Path < list[b].Path })
	return list
}

// fill measures one worktree: what it holds, how old it is, how big.
func fill(repoRoot string, w *WorktreeInfo) {
	st, err := os.Stat(w.Path)
	if err != nil {
		w.Missing = true
		return
	}
	// Read BEFORE any git command below: liveness is measured, and measuring
	// must not disturb what it measures.
	//
	// Liveness is the NEWEST of the directory and git's index. The directory's
	// own mtime changes only when an entry is added or removed in it, so an
	// agent editing a file three levels down leaves the root untouched — a live
	// session would look hours idle. Any git command touches the index, and an
	// agent runs them constantly.
	w.Age = time.Since(newest(st.ModTime(), indexTime(w.Path)))
	w.Bytes = dirSize(w.Path)

	// Commits the repo's own HEAD cannot reach. Work that was merged is
	// therefore NOT work: it already landed, and the tree is disposable.
	if w.Branch != "" {
		if out, err := git("", "-C", repoRoot, "rev-list", "--count", "HEAD.."+w.Branch); err == nil {
			w.Commits, _ = strconv.Atoi(strings.TrimSpace(out))
		}
	}
	// --no-optional-locks: a plain `git status` REFRESHES the index, which is a
	// write. The first scan would therefore stamp every worktree as touched-now
	// and the pruner would spend the rest of its life believing every tree was
	// live — a liveness check that destroys the evidence it reads. Measured
	// exactly that way: 30 trees all "touched 12m ago", which was when the
	// scan ran. This flag exists for tools that poll.
	status, err := git("", "-C", w.Path, "--no-optional-locks", "status", "--porcelain")
	if err != nil {
		return
	}
	for _, line := range strings.Split(strings.TrimSpace(status), "\n") {
		if len(line) < 4 {
			continue
		}
		path := strings.TrimSpace(line[2:])
		// Untracked files are not work by themselves — an agent that created a
		// file it never added left nothing anyone can name. Tracked edits are.
		if strings.HasPrefix(line, "??") || isArtifact(path) {
			continue
		}
		w.Dirty = append(w.Dirty, path)
	}
}

// indexTime is when git last wrote this worktree's index, or the zero time.
func indexTime(path string) time.Time {
	dir, err := git("", "-C", path, "rev-parse", "--git-dir")
	if err != nil {
		return time.Time{}
	}
	gitDir := strings.TrimSpace(dir)
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(path, gitDir)
	}
	st, err := os.Stat(filepath.Join(gitDir, "index"))
	if err != nil {
		return time.Time{}
	}
	return st.ModTime()
}

func newest(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

func isArtifact(path string) bool {
	for _, a := range artifactPaths {
		if strings.HasPrefix(path, a) || path == strings.TrimSuffix(a, "/") {
			return true
		}
	}
	return false
}

func dirSize(path string) int64 {
	var n int64
	_ = filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err == nil && info != nil && !info.IsDir() {
			n += info.Size()
		}
		return nil
	})
	return n
}

// PruneResult is what a prune pass did and what it deliberately left alone.
type PruneResult struct {
	Pruned    []WorktreeInfo // removed: held nothing
	Abandoned []WorktreeInfo // kept: holds work nobody has claimed
	Freed     int64
}

// PruneWorktrees removes every session worktree that holds no work, except
// keep (the live session's own tree). Registrations whose directory is gone are
// cleared too — git keeps those forever otherwise.
//
// It never removes a tree that holds work. Those come back in Abandoned, for
// the caller to report: a branch with unpushed commits may exist nowhere else
// in the world, and a cleanup pass is not the place to decide it is worthless.
func PruneWorktrees(repoRoot, keep string) PruneResult {
	var res PruneResult
	for _, w := range ScanWorktrees(repoRoot) {
		switch {
		case w.Path == keep:
			continue
		case w.Missing:
			// The directory is gone; only the registration is left.
			res.Pruned = append(res.Pruned, w)
		case w.Age < liveGrace:
			continue // too recently touched to be sure nobody is in it
		case w.HoldsWork():
			res.Abandoned = append(res.Abandoned, w)
		default:
			if _, err := git("", "-C", repoRoot, "worktree", "remove", "--force", w.Path); err != nil {
				continue
			}
			// The BRANCH goes with it. Keeping a branch whose tree held nothing
			// just moves the clutter somewhere less visible.
			if w.Branch != "" {
				_, _ = git("", "-C", repoRoot, "branch", "-D", w.Branch)
			}
			res.Freed += w.Bytes
			res.Pruned = append(res.Pruned, w)
		}
	}
	// Clears registrations for directories removed behind git's back.
	_, _ = git("", "-C", repoRoot, "worktree", "prune")
	sort.Slice(res.Abandoned, func(a, b int) bool { return res.Abandoned[a].Age < res.Abandoned[b].Age })
	return res
}

// RemoveWorktree deletes one worktree and its branch unconditionally — what
// /close does when the human says this work is finished with.
func RemoveWorktree(repoRoot, path, branch string) error {
	if repoRoot == "" || path == "" {
		return nil
	}
	if out, err := git("", "-C", repoRoot, "worktree", "remove", "--force", path); err != nil {
		return fmt.Errorf("worktree remove: %s", strings.TrimSpace(out))
	}
	if branch != "" {
		_, _ = git("", "-C", repoRoot, "branch", "-D", branch)
	}
	return nil
}

// keepSessions is how many sessions survive a prune. Enough to find the one you
// meant last week; few enough that the list stays readable and the worktrees
// stay bounded.
const keepSessions = 20

// PruneSessions deletes all but the newest keepSessions transcripts for a
// workspace, and returns the ids it removed.
//
// A session whose worktree still holds work is KEPT regardless of its age: the
// transcript is how you find out what that branch was for, and deleting it
// would leave an unexplained branch behind. except is never removed (the live
// session).
func PruneSessions(root, repoRoot, except string) []string {
	ids := ListSessions(root) // newest first
	if len(ids) <= keepSessions {
		return nil
	}
	holds := map[string]bool{}
	for _, w := range ScanWorktrees(repoRoot) {
		if w.HoldsWork() {
			holds[w.Path] = true
		}
	}
	var removed []string
	for _, id := range ids[keepSessions:] {
		if id == except {
			continue
		}
		if meta := LoadSessionMeta(root, id); meta.WorktreePath != "" && holds[meta.WorktreePath] {
			continue // its branch still holds work; the transcript explains it
		}
		if err := os.Remove(filepath.Join(RootDir(root), id+".jsonl")); err != nil {
			continue
		}
		_ = os.Remove(MetaFile(root, id))
		// A child's transcript is not a session of its own, but it is this
		// session's, and it goes with it.
		subs, _ := filepath.Glob(filepath.Join(RootDir(root), id+".sub*.jsonl"))
		for _, s := range subs {
			_ = os.Remove(s)
		}
		removed = append(removed, id)
	}
	return removed
}

// ForgetSession removes one session outright — transcript, children, metadata —
// so it cannot come back from /resume. Used by /close.
func ForgetSession(root, id string) {
	_ = os.Remove(filepath.Join(RootDir(root), id+".jsonl"))
	_ = os.Remove(MetaFile(root, id))
	subs, _ := filepath.Glob(filepath.Join(RootDir(root), id+".sub*.jsonl"))
	for _, s := range subs {
		_ = os.Remove(s)
	}
}
