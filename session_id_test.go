package dun

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

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
