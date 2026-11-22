package main

import (
	"os"
	"strings"
	"testing"

	"github.com/iodesystems/dun/internal/cmdstats"
	"github.com/iodesystems/dun/internal/enginecmd"
)

// The engine counts a routed line by the command's name, only when it is one
// it lists, and never the arguments.
func TestRecordSlash(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	known := enginecmd.CommandList(nil, "").Items
	recordSlashIn("/Retry attempts 3", known)
	recordSlashIn("/suggest auto", known)
	recordSlashIn("/notacommand", known)
	recordSlashIn("", known)
	b, _ := os.ReadFile(cmdstats.Path())
	got := string(b)
	if strings.Count(got, "\n") != 2 || !strings.Contains(got, `"cmd":"/retry"`) || strings.Count(got, `"cmd":"/suggest"`) != 1 ||
		!strings.Contains(got, `"surface":"engine"`) || strings.Contains(got, "attempts") {
		t.Errorf("counter: %q", got)
	}
}
