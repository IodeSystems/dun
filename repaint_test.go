package dun

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The engine reads and writes only in a capture directory.
func TestRepaintReportDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DUN_HOME", home)
	ok := filepath.Join(home, "bugreports", "20261023-120000")
	if got, err := RepaintReportDir(ok + "/"); err != nil || got != ok {
		t.Errorf("RepaintReportDir(%q) = %q, %v", ok, got, err)
	}
	for _, bad := range []string{
		filepath.Join(home, "sessions"),
		filepath.Join(home, "bugreports"),
		filepath.Join(home, "bugreports", "x", "y"),
		filepath.Join(home, "bugreports", "..", "sessions"),
		"/etc",
	} {
		if _, err := RepaintReportDir(bad); err == nil {
			t.Errorf("RepaintReportDir(%q) should refuse", bad)
		}
	}
}

// One round trip: prompt.txt goes out as the only message, the answer lands in
// model-output.txt, and the call is counted as a side call.
func TestRepaintReportCallsModel(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DUN_HOME", home)
	dir := filepath.Join(home, "bugreports", "20261023-120000")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "prompt.txt"), []byte("PROMPT"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &cannedRephraseRunner{reply: "=== FILE explanation.md ===\n# x\n=== END ==="}
	h := rephraseHarness(t, r)
	h.sideCalls = &sideCallStats{}
	if err := h.RepaintReport(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	if r.called != 1 || len(r.seen) != 1 || r.seen[0].Content != "PROMPT" {
		t.Fatalf("calls=%d seen=%v", r.called, r.seen)
	}
	b, err := os.ReadFile(filepath.Join(dir, "model-output.txt"))
	if err != nil || !strings.Contains(string(b), "explanation.md") {
		t.Fatalf("model-output.txt = %q, %v", b, err)
	}
	if h.SideCalls()["repaint"]["calls"] != 1 {
		t.Errorf("side calls = %v", h.SideCalls())
	}
}
