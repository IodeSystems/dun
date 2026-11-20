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
	body := strings.Repeat("x", engineExitKeep) + "\npanic: boom\n"
	if err := os.WriteFile(log, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(harness.SessionFile(ws, "s1")), 0o700); err != nil {
		t.Fatal(err)
	}
	got := keepEngineExit(&linkEngine{logPath: log}, ws, "s1")
	if want := strings.TrimSuffix(harness.SessionFile(ws, "s1"), ".jsonl") + ".engine-exit.log"; got != want {
		t.Fatalf("kept at %q, want %q", got, want)
	}
	b, err := os.ReadFile(got)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != engineExitKeep || !strings.HasSuffix(string(b), "panic: boom\n") {
		t.Errorf("kept %d bytes ending %q, want the last %d", len(b), b[len(b)-20:], engineExitKeep)
	}
	if got := keepEngineExit(&linkEngine{}, ws, "s1"); got != "" {
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
