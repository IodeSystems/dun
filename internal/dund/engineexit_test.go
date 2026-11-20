package dund

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/dun/internal/harness"
)

// An engine that exits with an error leaves the end of its output beside
// the session's transcript, so a panic is there to read.
func TestKeepEngineExit(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws := t.TempDir()
	log := filepath.Join(t.TempDir(), "123.log")
	body := strings.Repeat("x", engineExitKeep) + "\npanic: boom\n\ngoroutine 1 [running]:\n"
	if err := os.WriteFile(log, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(harness.SessionFile(ws, "s1")), 0o700); err != nil {
		t.Fatal(err)
	}
	got, why := keepEngineExit(&linkEngine{logPath: log}, ws, "s1")
	if why != "panic: boom" {
		t.Errorf("why %q, want the panic line", why)
	}
	if want := strings.TrimSuffix(harness.SessionFile(ws, "s1"), ".jsonl") + ".engine-exit.log"; got != want {
		t.Fatalf("kept at %q, want %q", got, want)
	}
	b, err := os.ReadFile(got)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != engineExitKeep || !strings.HasSuffix(string(b), "goroutine 1 [running]:\n") {
		t.Errorf("kept %d bytes ending %q, want the last %d", len(b), b[len(b)-20:], engineExitKeep)
	}
	if got, _ := keepEngineExit(&linkEngine{}, ws, "s1"); got != "" {
		t.Errorf("no log: kept %q", got)
	}
}

// An unlinked engine's stderr goes to dund's, one stamped and named line at
// a time, a line split across writes kept whole.
func TestPrefixWriter(t *testing.T) {
	var out strings.Builder
	at := time.Date(2026, 11, 20, 9, 5, 0, 0, time.Local)
	p := &prefixWriter{w: &out, prefix: "[engine s1] ", now: func() time.Time { return at }}
	_, _ = p.Write([]byte("panic: bo"))
	_, _ = p.Write([]byte("om\n\ngoroutine 1\n"))
	want := "2026/11/20 09:05:00 [engine s1] panic: boom\n" +
		"2026/11/20 09:05:00 [engine s1] \n" +
		"2026/11/20 09:05:00 [engine s1] goroutine 1\n"
	if out.String() != want {
		t.Errorf("wrote %q, want %q", out.String(), want)
	}
}

// A startup error is the last line; a panic's own line wins over the
// stacks after it.
func TestExitLine(t *testing.T) {
	if got := exitLine([]byte("dun: loading\ndun: flag provided but not defined: -x\n")); got != "dun: flag provided but not defined: -x" {
		t.Errorf("startup: %q", got)
	}
	if got := exitLine([]byte("log line\nfatal error: concurrent map writes\n\ngoroutine 7 [running]:\n")); got != "fatal error: concurrent map writes" {
		t.Errorf("fatal: %q", got)
	}
}

// A session whose workspace was deleted does not start an engine there: it
// says the workspace is gone.
func TestDeletedWorkspaceDoesNotStart(t *testing.T) {
	svc, _, _ := newSvc(t)
	ws := filepath.Join(t.TempDir(), "eva-onboard")
	if err := os.Mkdir(ws, 0o700); err != nil {
		t.Fatal(err)
	}
	id, err := svc.Create(alice, ws, []string{"--model", "m"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(ws); err != nil {
		t.Fatal(err)
	}
	ses, _ := svc.Get(alice, id)
	ses.stop()
	if err := ses.ensureRunning(); err == nil || !strings.Contains(err.Error(), "no longer exists") {
		t.Fatalf("ensureRunning: %v, want the workspace named gone", err)
	}
}
