package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iodesystems/dun"
)

// Each shell's script names every subcommand, the real flags (from the flag
// set, so a new flag is completed without editing the script), and the
// sessions helper for --resume; the removed flags are not offered.
func TestCompletion_Scripts(t *testing.T) {
	for _, sh := range []string{"bash", "zsh", "fish"} {
		out, stderr, code := runDun(t, "", nil, "completion", sh)
		if code != 0 {
			t.Fatalf("%s: exit %d: %s", sh, code, stderr)
		}
		for _, c := range subcommands {
			if !strings.Contains(out, c.name) {
				t.Errorf("%s: no subcommand %s", sh, c.name)
			}
		}
		flags := []string{"--resume", "--engine", "--workspace", "--no-tools", "--media", "-p"}
		if sh == "fish" {
			flags = []string{"-l resume", "-l engine", "-l workspace", "-l no-tools", "-l media", "-s p"}
		}
		for _, f := range flags {
			if !strings.Contains(out, f+" ") && !strings.Contains(out, f+")") && !strings.Contains(out, f+`"`) {
				t.Errorf("%s: no flag %s", sh, f)
			}
		}
		if !strings.Contains(out, "dun __complete sessions") {
			t.Errorf("%s: --resume does not complete sessions", sh)
		}
		if strings.Contains(out, "ship") {
			t.Errorf("%s: offers a removed flag", sh)
		}
		if bin, err := exec.LookPath(sh); err == nil {
			f := filepath.Join(t.TempDir(), "c."+sh)
			_ = os.WriteFile(f, []byte(out), 0o644)
			if b, err := exec.Command(bin, "-n", f).CombinedOutput(); err != nil {
				t.Errorf("%s -n: %v\n%s", sh, err, b)
			}
		}
	}
	if out, _, code := runDun(t, "", []string{"SHELL=/usr/bin/fish"}, "completion"); code != 0 || !strings.Contains(out, "complete -c dun") {
		t.Errorf("$SHELL fish: exit %d: %s", code, out)
	}
	if _, stderr, code := runDun(t, "", []string{"SHELL=/bin/tcsh"}, "completion"); code != 2 || !strings.Contains(stderr, "bash|zsh|fish") {
		t.Errorf("$SHELL tcsh: exit %d: %s", code, stderr)
	}
}

// `dun __complete sessions` prints what --sessions lists.
func TestCompletion_Sessions(t *testing.T) {
	ws := t.TempDir()
	path, id := dun.NewSessionFile(ws)
	_ = os.WriteFile(path, []byte(`{"role":"user","content":"hi"}`+"\n"), 0o600)
	var out, errb strings.Builder
	if code := completeHelper([]string{"sessions", "--workspace", ws}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	want := strings.Join(dun.ListSessions(ws), "\n")
	if got := strings.TrimSpace(out.String()); got != want || !strings.Contains(got, id) {
		t.Errorf("got %q, want %q (with %s)", got, want, id)
	}
}
