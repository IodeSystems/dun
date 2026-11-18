package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// sessionDirsHarness is a session with a session file of a name no other
// test uses (its isolation tmp is /tmp/dun-<id>), whose Ask is a (none when
// a is nil).
func sessionDirsHarness(t *testing.T, a *accessAsker) (*Harness, string) {
	t.Helper()
	t.Setenv("DUN_HOME", t.TempDir())
	id := fmt.Sprintf("sessdirs-%d-%d", os.Getpid(), time.Now().UnixNano())
	sf := filepath.Join(t.TempDir(), id+".jsonl")
	t.Cleanup(func() { _ = os.RemoveAll(IsoTmpDir(sf)) })
	return startSessionDirs(t, sf, a), sf
}

// startSessionDirs starts a session on sf whose Ask is a (none when nil).
func startSessionDirs(t *testing.T, sf string, a *accessAsker) *Harness {
	t.Helper()
	cfg := Config{Workspace: t.TempDir(), SessionFile: sf, Client: &scriptRunner{}}
	if a != nil {
		cfg.Ask = a.ask
	}
	h, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	return h
}

// otherSessionLog is a job log in another session's isolation tmp.
func otherSessionLog(t *testing.T) string {
	t.Helper()
	other := filepath.Join(t.TempDir(), fmt.Sprintf("sessdirs-other-%d.jsonl", time.Now().UnixNano()))
	t.Cleanup(func() { _ = os.RemoveAll(IsoTmpDir(other)) })
	dir := MkIsoTmp(other)
	if dir == "" {
		t.Fatal("no tmp for the other session")
	}
	theirs := filepath.Join(dir, "job-1.log")
	if err := os.WriteFile(theirs, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return theirs
}

// The path a background job's notice cites (logLine) is read by file_read
// as printed, with no question.
func TestSessionDirs_LogLinePathReadable(t *testing.T) {
	a := &accessAsker{ans: accessNo}
	h, _ := sessionDirsHarness(t, a)
	j := h.newJobStarted(HostExec{Dir: t.TempDir()}, "echo hello-log")
	waitFor(t, 10*time.Second, func() bool { return strings.Contains(j.status(), "in ") })
	line := j.logLine()
	path, ok := strings.CutPrefix(line, "Output: ")
	path, ok2 := strings.CutSuffix(path, " (file_read).")
	if !ok || !ok2 {
		t.Fatalf("logLine: %q", line)
	}
	if out := fileCall(t, h, "file_read", map[string]any{"path": path}); !strings.Contains(out, "hello-log") || len(a.asks) != 0 {
		t.Fatalf("file_read %s: %s (%d asks)", path, out, len(a.asks))
	}
}

// A session dir named through a link to it (macOS: /var/folders/… is a
// link to /private/var/folders/…, and sessionReadDirs keeps the real path)
// is routed to its reader. It was refused: the path was compared unresolved.
func TestSessionDirs_TmpRootThroughLink(t *testing.T) {
	real, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "T")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	h, _ := sessionDirsHarness(t, nil)
	cfg, _ := filesToolset(h.cfg.Files, h.cfg.Workspace)
	tmp := tmpReaders(cfg, real)
	args, _ := json.Marshal(map[string]any{"path": filepath.Join(link, "x.log")})
	if got := tmpReader(h.fileTools(), tmp, "file_read", string(args)); got == nil || got.Root() != real {
		t.Fatalf("read through the link routed to %v, want the reader at %s", got, real)
	}
}

// A job log dun saved for this session is read by its path with no
// question; a write there is not opened (it asks, and a no refuses);
// another session's dun-* dir asks, and a no refuses.
func TestSessionDirs_JobLogReadable(t *testing.T) {
	a := &accessAsker{ans: accessNo}
	h, _ := sessionDirsHarness(t, a)
	log := h.saveBoth("job-18.log", []byte("built ok\n"))
	if log == "" || !strings.HasPrefix(log, h.isoTmp()) {
		t.Fatalf("log %q not in the isolation tmp %q", log, h.isoTmp())
	}
	if out := fileCall(t, h, "file_read", map[string]any{"path": log}); !strings.Contains(out, "built ok") || len(a.asks) != 0 {
		t.Fatalf("read: %s (%d asks)", out, len(a.asks))
	}

	out := fileCall(t, h, "file_write", map[string]any{"path": log, "content": "x"})
	if !strings.HasPrefix(out, "ERROR") || len(a.asks) != 1 {
		t.Fatalf("write: %s (%d asks)", out, len(a.asks))
	}
	if b, _ := os.ReadFile(log); string(b) != "built ok\n" {
		t.Errorf("written: %q", b)
	}

	theirs := otherSessionLog(t)
	if out := fileCall(t, h, "file_read", map[string]any{"path": theirs}); strings.Contains(out, "secret") || !strings.HasPrefix(out, "ERROR") || len(a.asks) != 2 {
		t.Errorf("another session's log: %s (%d asks)", out, len(a.asks))
	}
}

// The temp dir as a whole is not open (USER 2026-11-18): another session's
// dun-* dir is refused when nobody can be asked, and a plain temp file asks.
func TestSessionDirs_TempDirClosed(t *testing.T) {
	theirs := otherSessionLog(t)
	h, _ := sessionDirsHarness(t, nil)
	if out := fileCall(t, h, "file_read", map[string]any{"path": theirs}); strings.Contains(out, "secret") || !strings.HasPrefix(out, "ERROR") {
		t.Errorf("another session's log, no Ask: %s", out)
	}

	plain := filepath.Join(t.TempDir(), "x.txt")
	if err := os.WriteFile(plain, []byte("plain tmp\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &accessAsker{ans: accessNo}
	h2, _ := sessionDirsHarness(t, a)
	if out := fileCall(t, h2, "file_read", map[string]any{"path": plain}); strings.Contains(out, "plain tmp") || len(a.asks) != 1 {
		t.Fatalf("plain temp file, no: %s (%d asks)", out, len(a.asks))
	}
	a.ans = accessSession
	if out := fileCall(t, h2, "file_read", map[string]any{"path": plain}); !strings.Contains(out, "plain tmp") || len(a.asks) != 2 {
		t.Errorf("plain temp file, yes: %s (%d asks)", out, len(a.asks))
	}
}

// A sub-agent (no Ask) reads its parent's job log by the path it was
// passed, and not another session's.
func TestSessionDirs_SubAgentReadsParent(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	sf := filepath.Join(t.TempDir(), fmt.Sprintf("sessdirs-par-%d.jsonl", time.Now().UnixNano()))
	t.Cleanup(func() { _ = os.RemoveAll(IsoTmpDir(sf)) })
	if !mkParentDirs(t, sf) {
		return
	}
	log := filepath.Join(IsoTmpDir(sf), "job-3.log")
	if err := os.WriteFile(log, []byte("parent log\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	child := startSessionDirs(t, childSessionFile(sf, 1), nil)
	if out := fileCall(t, child, "file_read", map[string]any{"path": log}); !strings.Contains(out, "parent log") {
		t.Errorf("parent's log from the child: %s", out)
	}
	if out := fileCall(t, child, "file_read", map[string]any{"path": otherSessionLog(t)}); strings.Contains(out, "secret") || !strings.HasPrefix(out, "ERROR") {
		t.Errorf("another session's log from the child: %s", out)
	}
}

// A sub-agent's dirs hold its parent's isolation tmp and overflow dir, and
// a parent's isolation tmp holds its child's.
func TestSessionDirs_SubAgent(t *testing.T) {
	sf := filepath.Join(t.TempDir(), fmt.Sprintf("sessdirs-sub-%d.jsonl", time.Now().UnixNano()))
	t.Cleanup(func() { _ = os.RemoveAll(IsoTmpDir(sf)) })
	child := &Harness{cfg: Config{SessionFile: childSessionFile(sf, 2)}}
	if got := parentSessionFile(child.cfg.SessionFile); got != sf {
		t.Fatalf("parent of %s: %s", child.cfg.SessionFile, got)
	}
	if got := parentSessionFile(sf); got != "" {
		t.Errorf("parent of a top-level session: %q", got)
	}
	if !mkParentDirs(t, sf) {
		return
	}
	dirs := child.sessionReadDirs()
	parentTmp, _ := filepath.EvalSymlinks(IsoTmpDir(sf))
	parentOver, _ := filepath.EvalSymlinks((&Harness{cfg: Config{SessionFile: sf}}).overflowDir())
	if !slices.Contains(dirs, parentTmp) || !slices.Contains(dirs, parentOver) {
		t.Errorf("child dirs %q lack the parent's %s / %s", dirs, parentTmp, parentOver)
	}
	if ct := IsoTmpDir(child.cfg.SessionFile); !strings.HasPrefix(ct, IsoTmpDir(sf)+string(filepath.Separator)) {
		t.Errorf("child tmp %s not under the parent's %s", ct, IsoTmpDir(sf))
	}
}

// mkParentDirs makes the parent's isolation tmp and overflow dir, as the parent
// session would have.
func mkParentDirs(t *testing.T, sf string) bool {
	t.Helper()
	if MkIsoTmp(sf) == "" {
		t.Fatal("no isolation tmp")
		return false
	}
	if err := os.MkdirAll((&Harness{cfg: Config{SessionFile: sf}}).overflowDir(), 0o700); err != nil {
		t.Fatal(err)
		return false
	}
	return true
}
