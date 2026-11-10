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
		t.Fatalf("removed flags passed: %q", args[0])
	}
	if strings.Contains(args[0], "--index-anyway") {
		t.Fatalf("--index-anyway passed unasked: %q", args[0])
	}
	if b, err := os.ReadFile(filepath.Join(ResolveProject(ws).WriteDir(), LocalServersFile)); err == nil && strings.Contains(string(b), "lsp_") {
		t.Fatalf("dun wrote an override: %s", b)
	}
}

// The roots value is the spec's when the config sets it. A config that still
// has the removed lsp_max_rss_mb and lsp_idle_stop keys loads, and they are
// ignored.
func TestLSPSafe_Overrides(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws := t.TempDir()
	if _, err := updateLocalServer(ws, ServerCode, func(s *ServerSpec) {
		s.LSPRoots = "auto"
	}); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(ResolveProject(ws).WriteDir(), LocalServersFile)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	old := strings.Replace(string(b), `"lsp_roots"`, `"lsp_max_rss_mb": 0, "lsp_idle_stop": "0", "lsp_roots"`, 1)
	if old == string(b) {
		t.Fatalf("no lsp_roots in %s", b)
	}
	if err := os.WriteFile(p, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	h, log, _ := startIndexed(t, ws, fakeIndexEnv+"=ready", fakeLifecycleEnv+"=1")
	waitIndex(t, h, IndexReady)
	if args := argLines(log); len(args) != 1 || !strings.HasSuffix(args[0], " --lsp-roots auto") {
		t.Fatalf("spawn: %q", args)
	}
}

// With --daemon, the proxy takes --lsp-roots only. An older build gets none.
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
