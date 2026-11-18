package harness

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// sessionDirsHarness is a session with a session file of a name no other
// test uses (its isolation tmp is /tmp/dun-<id>), the whole temp dir closed
// (tmpRoots off, or only roots), whose Ask is a.
func sessionDirsHarness(t *testing.T, a *accessAsker, roots ...string) (*Harness, string) {
	t.Helper()
	t.Setenv("DUN_HOME", t.TempDir())
	old := tmpRoots
	tmpRoots = func() []string { return roots }
	t.Cleanup(func() { tmpRoots = old })
	id := fmt.Sprintf("sessdirs-%d-%d", os.Getpid(), time.Now().UnixNano())
	sf := filepath.Join(t.TempDir(), id+".jsonl")
	t.Cleanup(func() { _ = os.RemoveAll(IsoTmpDir(sf)) })
	h, err := Start(context.Background(), Config{Workspace: t.TempDir(), SessionFile: sf, Client: &scriptRunner{}, Ask: a.ask})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	return h, sf
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

// A tmp root named through a link to it (macOS: /var/folders/… is a link
// to /private/var/folders/…, and tmpRoots keeps the real path) is read with
// no question. It was refused: the path was compared unresolved.
func TestSessionDirs_TmpRootThroughLink(t *testing.T) {
	real, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "T")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "x.log"), []byte("linked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &accessAsker{ans: accessNo}
	h, _ := sessionDirsHarness(t, a, real)
	if out := fileCall(t, h, "file_read", map[string]any{"path": filepath.Join(link, "x.log")}); !strings.Contains(out, "linked") || len(a.asks) != 0 {
		t.Fatalf("read through the link: %s (%d asks)", out, len(a.asks))
	}
}

// A job log dun saved for this session is read by its path with no
// question; a write there is not opened (it asks, and a no refuses);
// another session's dun-* dir is refused.
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
	if out := fileCall(t, h, "file_read", map[string]any{"path": theirs}); strings.Contains(out, "secret") || !strings.HasPrefix(out, "ERROR") {
		t.Errorf("another session's log: %s", out)
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
