package dun

import (
	"fmt"
	"log"
	"os"
	"os/exec"
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
//
// Only TRACKED paths reach this check (untracked ones are never work), so it
// matters when a repo once committed one of these by accident. The .dun/
// entries are what an older dun wrote there; dun writes none of them now.
// .dun/ as a whole is NOT an artifact: a project may commit .dun/dun.json or
// .dun/skills/, and an edit to those is work.
var artifactPaths = []string{
	".dun/agents_read", ".dun/dun.local.json", ".dun/.gitignore", ".dun/worktrees/",
	".poly-lsp-mcp/", // poly-lsp-mcp's index cache; it has no flag to put it elsewhere
}

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
	ours := sessionWorktreeDirs(repoRoot)

	var list []WorktreeInfo
	var cur WorktreeInfo
	flush := func() {
		if cur.Path != "" && underAny(cur.Path, ours) {
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

// sessionWorktreeDirs are the directories dun creates session worktrees of
// repoRoot in: the project's state dir now, and <repo>/.dun/worktrees/ — the
// old layout, still read so trees made before the move are listed, resumed and
// pruned. The legacy dir is checked for the repo's main worktree too, since
// repoRoot may be a linked worktree of it.
func sessionWorktreeDirs(repoRoot string) []string {
	source, top := projectRoots(repoRoot)
	dirs := []string{filepath.Join(repoRoot, DunDir, "worktrees"), filepath.Join(top, DunDir, "worktrees")}
	if source != top {
		dirs = append(dirs, filepath.Join(source, DunDir, "worktrees"))
	}
	if p := ResolveProject(repoRoot); p != nil {
		dirs = append(dirs, p.WorktreesDir())
	}
	var out []string
	for _, d := range dirs {
		out = append(out, d+string(os.PathSeparator))
		if r := realPath(d); r != d {
			out = append(out, r+string(os.PathSeparator))
		}
	}
	return out
}

// underAny reports whether path is below one of the dirs (each ending in a
// separator). A path is compared both as git printed it and resolved: git
// may hold /var/… where the dir was resolved to /private/var/….
func underAny(path string, dirs []string) bool {
	candidates := []string{path}
	if r := realPath(path); r != path {
		candidates = append(candidates, r)
	}
	for _, c := range candidates {
		for _, d := range dirs {
			if strings.HasPrefix(c, d) {
				return true
			}
		}
	}
	return false
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
			overlay := overlayFor(repoRoot, w.Path) // before the tree is gone: it resolves the path
			if _, err := git("", "-C", repoRoot, "worktree", "remove", "--force", w.Path); err != nil {
				continue
			}
			dropOverlay(overlay)
			// The BRANCH goes with it. Keeping a branch whose tree held nothing
			// just moves the clutter somewhere less visible.
			if w.Branch != "" {
				_, _ = git("", "-C", repoRoot, "branch", "-D", w.Branch)
			}
			// …and so does the search index it owned, for the same reason.
			dropIndexFor(repoRoot, w.Path, w.Branch)
			res.Freed += w.Bytes
			res.Pruned = append(res.Pruned, w)
		}
	}
	// Clears registrations for directories removed behind git's back.
	_, _ = git("", "-C", repoRoot, "worktree", "prune")
	sort.Slice(res.Abandoned, func(a, b int) bool { return res.Abandoned[a].Age < res.Abandoned[b].Age })
	return res
}

// RemoveWorktree deletes one worktree, its branch, and its search index — what
// /close does when the human says this work is finished with.
func RemoveWorktree(repoRoot, path, branch string) error {
	if repoRoot == "" || path == "" {
		return nil
	}
	overlay := overlayFor(repoRoot, path)
	if out, err := git("", "-C", repoRoot, "worktree", "remove", "--force", path); err != nil {
		return fmt.Errorf("worktree remove: %s", strings.TrimSpace(out))
	}
	dropOverlay(overlay)
	if branch != "" {
		_, _ = git("", "-C", repoRoot, "branch", "-D", branch)
	}
	dropIndexFor(repoRoot, path, branch)
	return nil
}

// overlayFor is the state overlay dun keeps for worktree path (see project.go),
// or "" when its project is unknown. Only the state dun wrote for that one
// directory is in it — the AGENTS.md guard's memory, a /raglit auto made there —
// and the directory is about to stop existing.
func overlayFor(repoRoot, path string) string {
	p := ResolveProject(repoRoot)
	if p == nil {
		return ""
	}
	return overlayDir(p.StateDir, realPath(path))
}

func dropOverlay(dir string) {
	if dir != "" {
		_ = os.RemoveAll(dir)
	}
}

// dropIndexFor removes the raglit index that belonged to a worktree.
//
// An index is named <directory>-<branch>, so one is minted per worktree per
// branch — and on disk it is a DIRECTORY (sqlite + originals/ + pages/), a few
// megabytes each, that nothing else ever deletes. This repo churned ~13
// worktrees a day and had 1.1 GB of them swept on 2026-08-03; without this the
// indexes reproduce that accumulation somewhere less visible.
//
// The shared POOL is deliberately untouched. It holds the expensive half of
// ingest (extract/OCR/segment/embed, keyed by recipe+file and shared by every
// index), so re-indexing this content anywhere later replays it with no model
// calls. Its own LRU GC governs when that is reclaimed — dropping an index must
// never reach into it, or a cleanup becomes a re-embedding bill.
//
// Best-effort by design: a stale index costs disk, and failing a worktree
// removal over it would be the worse trade.
func dropIndexFor(repoRoot, path, branch string) {
	name := indexNameFor(path, branch)
	if name == "" {
		return
	}
	out, err := exec.Command("raglit", "drop-index",
		"--project", raglitProject(repoRoot), "--index", name).CombinedOutput()
	if err != nil {
		log.Printf("dun: raglit drop-index %s: %v: %s", name, err, strings.TrimSpace(string(out)))
	}
}

// indexNameFor is raglitIndex for a worktree that may already be gone: it takes
// the branch as given rather than asking git, since the tree is removed first.
func indexNameFor(path, branch string) string {
	dir := filepath.Base(strings.TrimRight(path, string(os.PathSeparator)))
	if dir == "" || dir == "." || dir == string(os.PathSeparator) {
		return ""
	}
	if branch != "" {
		return normalizeProject(dir + "-" + branch)
	}
	return normalizeProject(dir)
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
	var removed []string
	// Claimed but never written (a dun that exited before saying anything):
	// nothing to keep. Ten minutes old, so a session that has just started
	// and not yet flushed is not taken out from under its process.
	for _, id := range emptySessionIDs(root, time.Now().Add(-10*time.Minute)) {
		if id != except && os.Remove(filepath.Join(RootDir(root), id+".jsonl")) == nil {
			removed = append(removed, id)
		}
	}
	// Written to but never asked anything — a dun opened and left, whose
	// transcript holds only notices: nothing to resume (USER 2026-10-22).
	// The same ten minutes, by modification time, keep a live one; a session
	// that cannot be read is kept, since it is exactly the one to look at.
	for _, id := range unaskedSessionIDs(root, time.Now().Add(-10*time.Minute)) {
		if id == except {
			continue
		}
		ForgetSession(root, id)
		removed = append(removed, id)
	}
	ids := ListSessions(root) // newest first
	if len(ids) <= keepSessions {
		return removed
	}
	holds := map[string]bool{}
	for _, w := range ScanWorktrees(repoRoot) {
		if w.HoldsWork() {
			holds[w.Path] = true
		}
	}
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
		removeSidecars(root, id)
		removed = append(removed, id)
	}
	return removed
}

// unaskedSessionIDs lists sessions with entries but no user message, last
// modified before cutoff.
func unaskedSessionIDs(root string, cutoff time.Time) []string {
	var out []string
	for _, id := range ListSessions(root) {
		path := SessionFile(root, id)
		st, err := os.Stat(path)
		if err != nil || !st.ModTime().Before(cutoff) {
			continue
		}
		if entries, preview, unreadable := scanSession(path); entries > 0 && preview == "" && !unreadable {
			out = append(out, id)
		}
	}
	return out
}

// ForgetSession removes one session outright — transcript, children, metadata —
// so it cannot come back from /resume. Used by /close.
func ForgetSession(root, id string) {
	_ = os.Remove(filepath.Join(RootDir(root), id+".jsonl"))
	_ = os.Remove(MetaFile(root, id))
	removeSidecars(root, id)
	// The shell's saved output (Harness.overflowDir), the children's too.
	over, _ := filepath.Glob(filepath.Join(RootDir(root), "overflow", id+".*"))
	for _, d := range append(over, filepath.Join(RootDir(root), "overflow", id)) {
		_ = os.RemoveAll(d)
	}
}

// removeSidecars deletes what belongs to a session besides its transcript:
// children ("<id>.subN.jsonl") and the archives recap and re-heading write
// ("<id>.recapN.jsonl", "<id>.reheadN.jsonl"). Only ".sub*" used to go, which
// left the archives behind with no session to explain them.
func removeSidecars(root, id string) {
	side, _ := filepath.Glob(filepath.Join(RootDir(root), id+".*.jsonl"))
	for _, s := range side {
		_ = os.Remove(s)
	}
}
