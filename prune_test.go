package dun

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// pruneRepo is a repo with a worktree per named branch, so a test can say what
// each one holds.
func pruneRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	gitrun(t, repo, "init", "-q")
	gitrun(t, repo, "config", "user.email", "t@t")
	gitrun(t, repo, "config", "user.name", "t")
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("hello\n"), 0o644)
	gitrun(t, repo, "add", ".")
	gitrun(t, repo, "commit", "-qm", "init")
	return repo
}

func addWorktree(t *testing.T, repo, name string) string {
	t.Helper()
	path := filepath.Join(repo, ".dun", "worktrees", name)
	gitrun(t, repo, "worktree", "add", "-q", "-b", name, path)
	return path
}

// backdate makes a worktree look untouched, past the live-session grace period.
// Both the directory and git's index, since liveness is the newer of the two.
func backdate(t *testing.T, path string) {
	t.Helper()
	old := time.Now().Add(-2 * liveGrace)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if dir, err := git("", "-C", path, "rev-parse", "--git-dir"); err == nil {
		g := strings.TrimSpace(dir)
		if !filepath.IsAbs(g) {
			g = filepath.Join(path, g)
		}
		_ = os.Chtimes(filepath.Join(g, "index"), old, old)
	}
}

// The criterion is "holds no WORK", not "git status is clean". Measured on
// dun's own repo: by the clean-tree test 34 of 37 trees looked dirty — almost
// all from one untracked index artifact — so a status-based prune would have
// reclaimed three of thirty-seven.
func TestPrune_RemovesOnlyTreesHoldingNothing(t *testing.T) {
	repo := pruneRepo(t)
	addWorktree(t, repo, "empty")
	artifact := addWorktree(t, repo, "artifact-only")
	edited := addWorktree(t, repo, "edited")
	committed := addWorktree(t, repo, "committed")

	// Untracked artifacts are not work: an index cache is not a reason to keep
	// a gigabyte.
	os.MkdirAll(filepath.Join(artifact, ".poly-lsp-mcp"), 0o755)
	os.WriteFile(filepath.Join(artifact, ".poly-lsp-mcp", "cache.gob"), []byte("x"), 0o644)
	// An edit to a TRACKED file is.
	os.WriteFile(filepath.Join(edited, "a.txt"), []byte("changed\n"), 0o644)
	// So is a commit.
	os.WriteFile(filepath.Join(committed, "b.txt"), []byte("new\n"), 0o644)
	gitrun(t, committed, "add", ".")
	gitrun(t, committed, "commit", "-qm", "work")

	for _, w := range ScanWorktrees(repo) {
		backdate(t, w.Path)
	}
	res := PruneWorktrees(repo, "")

	pruned := map[string]bool{}
	for _, w := range res.Pruned {
		pruned[filepath.Base(w.Path)] = true
	}
	if !pruned["empty"] || !pruned["artifact-only"] {
		t.Errorf("a tree holding nothing must be pruned, got %v", pruned)
	}
	if pruned["edited"] || pruned["committed"] {
		t.Fatalf("a tree holding work must NEVER be pruned, got %v", pruned)
	}
	abandoned := map[string]bool{}
	for _, w := range res.Abandoned {
		abandoned[filepath.Base(w.Path)] = true
	}
	if !abandoned["edited"] || !abandoned["committed"] {
		t.Errorf("work that was kept must be reported, got %v", abandoned)
	}
	// The branch goes with the tree — keeping it just moves the clutter
	// somewhere less visible.
	if out, _ := git("", "-C", repo, "branch", "--list", "empty"); strings.TrimSpace(out) != "" {
		t.Error("a pruned tree's branch should be deleted too")
	}
	if out, _ := git("", "-C", repo, "branch", "--list", "committed"); strings.TrimSpace(out) == "" {
		t.Error("a kept tree's branch must survive")
	}
}

// Work that already landed is not work: if the repo's HEAD can reach the
// commits, the tree is disposable.
func TestPrune_MergedWorkIsDisposable(t *testing.T) {
	repo := pruneRepo(t)
	path := addWorktree(t, repo, "landed")
	os.WriteFile(filepath.Join(path, "c.txt"), []byte("done\n"), 0o644)
	gitrun(t, path, "add", ".")
	gitrun(t, path, "commit", "-qm", "landed work")
	gitrun(t, repo, "merge", "-q", "landed")
	backdate(t, path)

	res := PruneWorktrees(repo, "")
	if len(res.Abandoned) != 0 {
		t.Fatalf("merged work should not be reported as abandoned: %+v", res.Abandoned)
	}
	if len(res.Pruned) != 1 {
		t.Fatalf("a tree whose commits already landed is disposable, got %+v", res.Pruned)
	}
}

// The live session's own tree is never a candidate.
func TestPrune_KeepsTheLiveWorktree(t *testing.T) {
	repo := pruneRepo(t)
	live := addWorktree(t, repo, "live")

	if res := PruneWorktrees(repo, live); len(res.Pruned) != 0 {
		t.Fatalf("the session's own worktree must survive its own prune: %+v", res.Pruned)
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatal("the live worktree was removed")
	}
}

// git keeps a registration forever after its directory disappears — one such
// entry was already present on the machine this was written on.
func TestPrune_ClearsDeadRegistrations(t *testing.T) {
	repo := pruneRepo(t)
	path := addWorktree(t, repo, "vanished")
	os.RemoveAll(path)

	res := PruneWorktrees(repo, "")
	if len(res.Pruned) != 1 || !res.Pruned[0].Missing {
		t.Fatalf("a registration with no directory should be cleared: %+v", res.Pruned)
	}
	out, _ := git("", "-C", repo, "worktree", "list")
	if strings.Contains(out, "vanished") {
		t.Errorf("the dead registration survived: %q", out)
	}
}

// Trees dun did not create are none of its business.
func TestPrune_IgnoresWorktreesDunDidNotMake(t *testing.T) {
	repo := pruneRepo(t)
	outside := filepath.Join(t.TempDir(), "mine")
	gitrun(t, repo, "worktree", "add", "-q", "-b", "mine", outside)
	backdate(t, outside)

	if res := PruneWorktrees(repo, ""); len(res.Pruned) != 0 || len(res.Abandoned) != 0 {
		t.Fatalf("a human's worktree must be invisible to the pruner: %+v", res)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatal("the pruner removed a worktree it did not create")
	}
}

// The session window is bounded, but a session whose branch still holds work
// keeps its transcript: that is how you find out what the branch was FOR.
func TestPruneSessions_KeepsTheNewestAndAnythingExplainingWork(t *testing.T) {
	repo := pruneRepo(t)
	root := t.TempDir()
	t.Setenv("DUN_HOME", root)

	dir := RootDir(repo)
	os.MkdirAll(dir, 0o755)
	var ids []string
	for i := 0; i < keepSessions+5; i++ {
		id := "2026080" + string(rune('0'+i%10)) + "-" + strings.Repeat("0", 5) + string(rune('a'+i))
		ids = append(ids, id)
		os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte("{}\n"), 0o644)
	}
	// The OLDEST session owns a worktree that still holds work.
	oldest := ids[0]
	path := addWorktree(t, repo, "still-working")
	os.WriteFile(filepath.Join(path, "a.txt"), []byte("changed\n"), 0o644)
	backdate(t, path)
	if err := SaveSessionMeta(repo, oldest, SessionMeta{WorktreePath: path, Branch: "still-working"}); err != nil {
		t.Fatal(err)
	}

	removed := PruneSessions(repo, repo, "")
	if len(removed) == 0 {
		t.Fatal("nothing was pruned from a list well over the window")
	}
	for _, id := range removed {
		if id == oldest {
			t.Fatal("a session explaining an abandoned worktree must be kept")
		}
	}
	if len(ListSessions(repo)) <= keepSessions {
		t.Log("window respected")
	}
}

// /close is the explicit, destructive counterpart to the prune: the human says
// this work is finished with, so it goes — and cannot come back from /resume.
func TestForgetSession_LeavesNothingToResume(t *testing.T) {
	repo := t.TempDir()
	t.Setenv("DUN_HOME", t.TempDir())
	dir := RootDir(repo)
	os.MkdirAll(dir, 0o755)

	id := "20260801-120000"
	os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte("{}\n"), 0o644)
	os.WriteFile(filepath.Join(dir, id+".sub1.jsonl"), []byte("{}\n"), 0o644)
	SaveSessionMeta(repo, id, SessionMeta{Branch: "gone"})

	ForgetSession(repo, id)

	for _, id := range ListSessions(repo) {
		t.Fatalf("a closed session must not be resumable, still listed: %q", id)
	}
	if _, err := os.Stat(filepath.Join(dir, id+".sub1.jsonl")); err == nil {
		t.Error("a child's transcript should go with its parent's")
	}
}

// A machine runs several dun sessions at once — four, while this was being
// written. PruneWorktrees is told which tree is the CALLER's, and a concurrent
// session's tree holding nothing yet is exactly what would be deleted out from
// under it. Recency stands in for liveness.
func TestPrune_SparesARecentlyTouchedTree(t *testing.T) {
	repo := pruneRepo(t)
	fresh := addWorktree(t, repo, "someone-elses-live-session")

	// Not the caller's tree, holds nothing, and would be deleted on age alone.
	if res := PruneWorktrees(repo, ""); len(res.Pruned) != 0 {
		t.Fatalf("a tree touched seconds ago must not be pruned: %+v", res.Pruned)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatal("a live session's worktree was removed")
	}

	// Backdate it past the grace period and it becomes a candidate.
	backdate(t, fresh)
	if res := PruneWorktrees(repo, ""); len(res.Pruned) != 1 {
		t.Fatalf("an old tree holding nothing should be pruned: %+v", res.Pruned)
	}
}
