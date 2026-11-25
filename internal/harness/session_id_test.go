package harness

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/dun/internal/project"
)

// A test binary without $DUN_HOME never resolves the real ~/.dun.
func TestSessionsDir_TestsNeverReachTheRealHome(t *testing.T) {
	t.Setenv("DUN_HOME", "")
	home, _ := os.UserHomeDir()
	got := SessionsDir()
	if home != "" && strings.HasPrefix(got, filepath.Join(home, ".dun")) {
		t.Fatalf("SessionsDir = %s, the real home", got)
	}
	if got != SessionsDir() {
		t.Fatal("the stand-in moved between calls")
	}
}

// Two sessions started in the same second get different files.
func TestNewSessionFile_NoCollisionInOneSecond(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws := t.TempDir()
	seen := map[string]bool{}
	for i := 0; i < 5; i++ {
		p, id := NewSessionFile(ws)
		if seen[p] {
			t.Fatalf("session %d reused %s", i, id)
		}
		seen[p] = true
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("the file is claimed on creation: %v", err)
		}
	}
}

// Sidecars and children are not sessions: --continue must not resume
// "<id>.rehead1", and pruning a session takes its sidecars with it.
func TestSessionIDs_SkipSidecars(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws := t.TempDir()
	dir := RootDir(ws)
	os.MkdirAll(dir, 0o755)
	for _, n := range []string{"20261020-072328.jsonl", "20261020-072328.recap1.jsonl",
		"20261020-072328.rehead1.jsonl", "20261020-072328.sub1.jsonl", "20261019-100000.jsonl"} {
		os.WriteFile(filepath.Join(dir, n), []byte("{}\n"), 0o644)
	}
	if got := LatestSession(ws); got != "20261020-072328" {
		t.Errorf("latest = %q, want the session, not a sidecar", got)
	}
	if got := ListSessions(ws); len(got) != 2 {
		t.Errorf("sessions = %v, want 2", got)
	}
	ForgetSession(ws, "20261020-072328")
	left, _ := filepath.Glob(filepath.Join(dir, "20261020-072328*"))
	if len(left) != 0 {
		t.Errorf("forgetting a session leaves %v", left)
	}
}

// A claimed-but-empty session is not resumable, and pruning removes it once
// it is old enough that no live process is about to write it.
func TestEmptySession_SkippedAndPruned(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws := t.TempDir()
	dir := RootDir(ws)
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "20261019-100000.jsonl"), []byte("{}\n"), 0o644)
	old := filepath.Join(dir, "20261020-100000.jsonl")
	fresh := filepath.Join(dir, "20261020-110000.jsonl")
	os.WriteFile(old, nil, 0o644)
	os.WriteFile(fresh, nil, 0o644)
	os.Chtimes(old, time.Now().Add(-time.Hour), time.Now().Add(-time.Hour))
	if got := LatestSession(ws); got != "20261019-100000" {
		t.Errorf("latest = %q, want the newest NON-empty session", got)
	}
	PruneSessions(ws, ws, "")
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("an old empty session should be pruned")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("a fresh empty session may be live and must stay")
	}
}

// A session with entries but no user message is not offered to resume; an
// unreadable one still is.
func TestListSessionInfo_SkipsSessionsWithNoUserMessage(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws := t.TempDir()
	dir := RootDir(ws)
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "20261022-100000.jsonl"), []byte(`{"Kind":"notification","Content":"ready"}`+"\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "20261022-100001.jsonl"), []byte(`{"Kind":"user","Content":"fix the tests"}`+"\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "20261022-100002.jsonl"), []byte("not json\n"), 0o644)
	var ids []string
	for _, s := range ListSessionInfo(ws) {
		ids = append(ids, s.ID)
	}
	if len(ids) != 2 || ids[0] != "20261022-100002" || ids[1] != "20261022-100001" {
		t.Errorf("listed %v; want the asked one and the unreadable one", ids)
	}
}

// A session's latest user message is listed under its opening one; a
// session with one ask has none.
func TestListSessionInfo_LastUserMessage(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws := t.TempDir()
	dir := RootDir(ws)
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "20261029-100000.jsonl"), []byte(`{"Kind":"user","Content":"fix the tests"}`+"\n"+
		`{"Kind":"assistant","Content":"done"}`+"\n"+`{"Kind":"user","Content":"now  release\nit"}`+"\n"+
		`{"Kind":"assistant","Content":"released"}`+"\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "20261029-100001.jsonl"), []byte(`{"Kind":"user","Content":"one ask"}`+"\n"), 0o644)
	got := map[string]string{}
	for _, s := range ListSessionInfo(ws) {
		got[s.ID] = s.Last
	}
	if got["20261029-100000"] != "now release it" || got["20261029-100001"] != "" {
		t.Errorf("last: %q", got)
	}
}

// A cleared session — live transcript notices only, or emptied — is listed:
// its archive holds the ask (SessionAsked). One with no ask anywhere is not.
func TestListSessionInfo_ShowsAClearedSession(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws := t.TempDir()
	dir := RootDir(ws)
	os.MkdirAll(dir, 0o755)
	w := func(name, body string) { os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644) }
	user := `{"Kind":"user","Content":"fix it"}` + "\n"
	note := `{"Kind":"notification","Content":"cleared"}` + "\n"
	w("20261114-100000.jsonl", note)
	w("20261114-100000.cleared.20261114-100500.jsonl", user)
	w("20261114-100001.jsonl", "") // emptied after /clear
	w("20261114-100001.cleared.20261114-100500.jsonl", user)
	w("20261114-100002.jsonl", note) // never asked
	var ids []string
	for _, s := range ListSessionInfo(ws) {
		ids = append(ids, s.ID)
	}
	if len(ids) != 2 || ids[0] != "20261114-100001" || ids[1] != "20261114-100000" {
		t.Errorf("listed %v; want the two cleared sessions", ids)
	}
}

// F7: a repo with sessions under its old path-keyed location is migrated to
// the project-id key on first use after the keying change.
func TestRootDir_MigratesLegacySessions(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws := t.TempDir()

	// Write a session under the OLD path-keyed layout (what pre-change dun wrote).
	legacyDir := legacyRootDir(ws)
	os.MkdirAll(legacyDir, 0o755)
	sessionID := "20261001-120000"
	os.WriteFile(filepath.Join(legacyDir, sessionID+".jsonl"),
		[]byte(`{"Kind":"user","Content":"fix the tests"}`+"\n"), 0o644)

	// Register the project. This is what a new dun does on first sight.
	p, err := project.Ensure(ws)
	if err != nil {
		t.Fatal(err)
	}
	if p == nil {
		t.Fatal("Ensure returned nil")
	}

	// RootDir now returns the id-keyed location.
	dir := RootDir(ws)
	wantDir := project.SessionDir(p.ID)
	if dir != wantDir {
		t.Fatalf("RootDir = %s, want %s", dir, wantDir)
	}

	// The session file was copied into the new location.
	newFile := filepath.Join(dir, sessionID+".jsonl")
	data, err := os.ReadFile(newFile)
	if err != nil {
		t.Fatalf("migrated session not found: %v", err)
	}
	if !strings.Contains(string(data), "fix the tests") {
		t.Errorf("migrated session lost content: %q", data)
	}

	// The old file is still there (migration is copy, not move).
	if _, err := os.Stat(filepath.Join(legacyDir, sessionID+".jsonl")); err != nil {
		t.Error("legacy file should remain after migration")
	}

	// The session is resumable from the new location.
	if got := LatestSession(ws); got != sessionID {
		t.Errorf("LatestSession = %q, want %q", got, sessionID)
	}
}

// F7: a linked worktree shares its source repo's project id, so sessions
// created in the worktree appear in the source repo's session list.
func TestRootDir_WorktreeSharesSourceRepo(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	repo := t.TempDir()
	gitrun(t, repo, "init", "-q")
	gitrun(t, repo, "config", "user.email", "t@t")
	gitrun(t, repo, "config", "user.name", "t")
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("hello\n"), 0o644)
	gitrun(t, repo, "add", ".")
	gitrun(t, repo, "commit", "-qm", "init")

	// Register the source repo.
	p, err := project.Ensure(repo)
	if err != nil {
		t.Fatal(err)
	}

	// Create a linked worktree.
	wt := filepath.Join(t.TempDir(), "wt")
	gitrun(t, repo, "worktree", "add", "-q", "-b", "feature", wt)

	// Both the source repo and the worktree resolve to the same session dir.
	srcDir := RootDir(repo)
	wtDir := RootDir(wt)
	if srcDir != wtDir {
		t.Fatalf("RootDir(repo) = %s, RootDir(worktree) = %s; want the same", srcDir, wtDir)
	}
	if srcDir != project.SessionDir(p.ID) {
		t.Fatalf("RootDir = %s, want %s", srcDir, project.SessionDir(p.ID))
	}

	// A session created in the worktree is visible from the source repo.
	sp, id := NewSessionFile(wt)
	os.WriteFile(sp, []byte(`{"Kind":"user","Content":"do something"}`+"\n"), 0o644)
	ids := ListSessions(repo)
	found := false
	for _, x := range ids {
		if x == id {
			found = true
		}
	}
	if !found {
		t.Errorf("session %q created in the worktree is not in the source repo's list %v", id, ids)
	}
}

// F7: a repo whose checked-out path changed (re-pointed) keeps its sessions
// under its registry id.
func TestRootDir_MovedRepoKeepsSessions(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	oldPath := t.TempDir()
	gitrun(t, oldPath, "init", "-q")
	gitrun(t, oldPath, "config", "user.email", "t@t")
	gitrun(t, oldPath, "config", "user.name", "t")
	os.WriteFile(filepath.Join(oldPath, "a.txt"), []byte("hello\n"), 0o644)
	gitrun(t, oldPath, "add", ".")
	gitrun(t, oldPath, "commit", "-qm", "init")

	// Register the repo and create a session.
	p, err := project.Ensure(oldPath)
	if err != nil {
		t.Fatal(err)
	}
	sp, sessionID := NewSessionFile(oldPath)
	if sessionID == "" {
		t.Fatal("no session created")
	}
	os.WriteFile(sp, []byte(`{"Kind":"user","Content":"do something"}`+"\n"), 0o644)

	// "Move" the repo: simulate by re-pointing the registry to a new path.
	// The new path must be a valid git repo with the same git-common-dir
	// structure for Roots() to resolve it correctly.
	newPath := filepath.Join(t.TempDir(), "moved")
	// Copy the git repo to the new location.
	if out, err := exec.Command("cp", "-r", oldPath, newPath).CombinedOutput(); err != nil {
		t.Fatalf("cp: %v: %s", err, out)
	}

	// Re-point the project to the new path.
	if err := project.Repoint(p.ID, newPath); err != nil {
		t.Fatal(err)
	}

	// The session dir is still keyed by the project id, not the path.
	dir := RootDir(newPath)
	wantDir := project.SessionDir(p.ID)
	if dir != wantDir {
		t.Fatalf("RootDir(moved) = %s, want %s", dir, wantDir)
	}

	// The existing session is visible from the new path.
	ids := ListSessions(newPath)
	found := false
	for _, x := range ids {
		if x == sessionID {
			found = true
		}
	}
	if !found {
		t.Errorf("session %q not found after move; got %v", sessionID, ids)
	}
}
