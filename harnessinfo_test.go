package dun

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// What dun is, three ways: the session tool, the file dun session info
// reads, and the system prompt's line (USER 2026-10-27).
func TestHarnessInfo(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	ws := t.TempDir()
	sf := filepath.Join(t.TempDir(), "20261027-101500.jsonl")
	h := &Harness{cfg: Config{Workspace: ws, SessionFile: sf, Version: "v0.1.3",
		Provider: "openrouter", Endpoint: Endpoint{Model: "deepseek/x"}, Exec: &HostShell{Dir: ws}}}
	h.effort.Store(EffortHigh)
	in := h.HarnessInfo()
	if in.Name != "dun" || in.Icon != "👷" || in.SessionID != "20261027-101500" || in.Version != "v0.1.3" ||
		in.Model != "deepseek/x" || in.Provider != "openrouter" || in.Effort != "high" || in.Workspace != ws || in.Context == nil {
		t.Errorf("info: %+v", in)
	}
	// The line names the env var, not the path: no session id in the system
	// prompt (the same text in every session, for the cache).
	line := h.identityLine()
	if !strings.Contains(line, "$DUN_HARNESS") || strings.Contains(line, "20261027") {
		t.Errorf("identity line: %s", line)
	}
	// "You are dun" is said once, in the preamble, with the icon.
	if strings.Contains(line, "You are dun") || !strings.Contains(systemPreamble, "You are dun (👷)") {
		t.Errorf("identity said twice or icon missing: %q / %q", line, systemPreamble)
	}
	h.writeHarnessInfo()
	b, err := os.ReadFile(HarnessInfoFile(sf))
	if err != nil || !strings.Contains(string(b), `"session_id": "20261027-101500"`) {
		t.Errorf("file: %v %s", err, b)
	}
	// exec reads it through $DUN_HARNESS, as the model would.
	sh := h.withExecEnv(h.cfg.Exec).(*HostShell)
	defer sh.Close()
	if r := sh.Run(context.Background(), `cat "$DUN_HARNESS"`, nil); !strings.Contains(r.Output, `"session_id": "20261027-101500"`) {
		t.Errorf("cat $DUN_HARNESS: %+v", r)
	}
}
