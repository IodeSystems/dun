package dun

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A build whose `mcp --help` lists the lifecycle flags gets --lsp-roots
// single only; no --index-anyway unless the project set it; and dun writes no
// override.
func TestLSPSafe_Defaults(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws := t.TempDir()
	h, log, _ := startIndexed(t, ws, fakeIndexEnv+"=ready", fakeLifecycleEnv+"=1", fakeAnywayEnv+"=1")
	waitIndex(t, h, IndexReady)
	args := argLines(log)
	if len(args) != 1 || !strings.HasSuffix(args[0], " --lsp-roots single") {
		t.Fatalf("spawn: %q", args)
	}
	if strings.Contains(args[0], "--lsp-max-rss-mb") || strings.Contains(args[0], "--lsp-idle-stop") {
		t.Fatalf("interim flags passed: %q", args[0])
	}
	if strings.Contains(args[0], "--index-anyway") {
		t.Fatalf("--index-anyway passed unasked: %q", args[0])
	}
	if b, err := os.ReadFile(filepath.Join(ResolveProject(ws).WriteDir(), LocalServersFile)); err == nil && strings.Contains(string(b), "lsp_") {
		t.Fatalf("dun wrote an override: %s", b)
	}
}

// Each value is the spec's when the config sets it.
func TestLSPSafe_Overrides(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws := t.TempDir()
	mb := 0
	if _, err := updateLocalServer(ws, ServerCode, func(s *ServerSpec) {
		s.LSPRoots, s.LSPMaxRSSMB, s.LSPIdleStop = "auto", &mb, "0"
	}); err != nil {
		t.Fatal(err)
	}
	h, log, _ := startIndexed(t, ws, fakeIndexEnv+"=ready", fakeLifecycleEnv+"=1")
	waitIndex(t, h, IndexReady)
	if args := argLines(log); len(args) != 1 || !strings.HasSuffix(args[0], " --lsp-roots auto --lsp-max-rss-mb 0 --lsp-idle-stop 0") {
		t.Fatalf("spawn: %q", args)
	}
}

// With --daemon, the proxy takes --lsp-roots only: the other two are the
// daemon's own, and would make the proxy serve in-process. An older build
// gets none.
func TestLSPSafe_DaemonAndOlder(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws := t.TempDir()
	h, log, _ := startIndexed(t, ws, fakeIndexEnv+"=ready", fakeLifecycleEnv+"=1", fakeDaemonEnv+"=anyroot")
	waitIndex(t, h, IndexReady)
	args := argLines(log)
	if len(args) != 1 || !strings.HasSuffix(args[0], " --daemon --lsp-roots single") || strings.Contains(args[0], "--lsp-max-rss-mb") {
		t.Fatalf("daemon spawn: %q", args)
	}
	h.Close()

	h, log, _ = startIndexed(t, ws, fakeIndexEnv+"=ready")
	waitIndex(t, h, IndexReady)
	if args := argLines(log); len(args) != 1 || strings.Contains(args[0], "--lsp-") {
		t.Fatalf("older build got lifecycle flags: %q", args)
	}
}
