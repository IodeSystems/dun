package dundcli

import (
	"path/filepath"
	"strings"
	"testing"
)

// Every socket under DUN_HOME falls back to the runtime dir when the path
// would be too long to bind, and dund's own name is unchanged by the
// generalisation.
func TestSocketFileFallsBackForALongHome(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1")
	short := "/h"
	if got := SocketFile(short, "launcher"); got != "/h/launcher.sock" {
		t.Errorf("short home: %s", got)
	}
	long := "/" + strings.Repeat("d", 120)
	got := SocketFile(long, "launcher")
	if filepath.Dir(got) != "/run/user/1" || !strings.HasPrefix(filepath.Base(got), "launcher-") || len(got) > maxSocketPath() {
		t.Errorf("long home: %s", got)
	}
	if Socket(long) == got || !strings.HasPrefix(filepath.Base(Socket(long)), "dund-") {
		t.Errorf("dund and the launcher must not share a socket: %s", Socket(long))
	}
}
