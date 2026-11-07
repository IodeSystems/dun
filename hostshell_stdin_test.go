package dun

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A command that reads stdin gets EOF, not the shell's control pipe. The
// persistent shell reads its commands and dun's end-of-command sentinel from
// that pipe; a command sharing it (head, read, an installer's prompt) ate the
// sentinel and hung until its job timed out: Terminal-Bench's
// configure-git-webserver, 2026-11-06, apt-get at 0% CPU with no output.
func TestHostShell_CommandStdinIsDevNull(t *testing.T) {
	hs := &HostShell{Dir: t.TempDir()}
	defer hs.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	r := hs.Run(ctx, "head -c 40; read x; echo got:$x", nil)
	if r.Failed() || time.Since(start) > 3*time.Second {
		t.Fatalf("a stdin read hung or failed (%s): %+v", time.Since(start).Round(time.Millisecond), r)
	}
	if strings.TrimSpace(r.Output) != "got:" {
		t.Errorf("stdin was not empty: %q", r.Output)
	}
	// The shell still works, and state still carries: braces are not a subshell.
	if r := hs.Run(ctx, "export DUN_T=1; cd /", nil); r.Failed() {
		t.Fatal(r)
	}
	if r := hs.Run(ctx, "echo $DUN_T", nil); strings.TrimSpace(r.Output) != "1" {
		t.Errorf("export did not persist: %q", r.Output)
	}
}
