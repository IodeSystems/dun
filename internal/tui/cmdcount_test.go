package tui

import (
	"os"
	"strings"
	"testing"

	"github.com/iodesystems/dun/internal/cmdstats"
)

// A command the TUI runs itself is counted once, as "tui", name only; one it
// hands to the engine as a slash line is the engine's to count.
func TestSlashCounted_TUIOnly(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	rec := &recWC{}
	m := newTUIModel(&dunProc{stdin: rec}, "/ws")
	m = resized(m, 100, 30)

	m.runSlash("/keys reload secret-arg")
	b, _ := os.ReadFile(cmdstats.Path())
	if got := string(b); strings.Count(got, "\n") != 1 || !strings.Contains(got, `"cmd":"/keys"`) ||
		!strings.Contains(got, `"surface":"tui"`) || strings.Contains(got, "secret") {
		t.Fatalf("counter: %q", got)
	}

	m.runSlash("/persist status") // a TUI command: counted once, here
	b, _ = os.ReadFile(cmdstats.Path())
	if strings.Count(string(b), `"cmd":"/persist"`) != 1 || strings.Count(string(b), "\n") != 2 {
		t.Fatalf("/persist counter: %q", b)
	}

	m.runSlash("/suggest auto") // forwarded to the engine, which counts it
	b, _ = os.ReadFile(cmdstats.Path())
	if strings.Contains(string(b), "suggest") || strings.Count(string(b), "\n") != 2 {
		t.Fatalf("/suggest counted in the TUI: %q", b)
	}

	m.runSlash("/jobs") // forwarded: the engine counts it
	b, _ = os.ReadFile(cmdstats.Path())
	if strings.Count(string(b), "\n") != 2 || !strings.Contains(rec.String(), `"slash"`) {
		t.Errorf("a forwarded line was counted in the TUI: %q sent %s", b, rec.String())
	}
}
