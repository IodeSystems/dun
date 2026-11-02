package dun

import (
	"context"
	"strings"
	"testing"
)

// HostExec is where model-authored commands run — the path that actually hit
// this. The wiring matters more than the helper.
func TestHostExec_DetachesTheCommand(t *testing.T) {
	out := HostExec{Dir: t.TempDir()}.Run(context.Background(), "ps -o tty= -p $$", nil)
	// No controlling terminal renders as "?" (or empty) in ps's tty column.
	if tty := strings.TrimSpace(out.Output); tty != "" && tty != "?" {
		t.Errorf("exec'd command still has a controlling terminal (%q) — it can steal the TUI's keystrokes", tty)
	}
}
