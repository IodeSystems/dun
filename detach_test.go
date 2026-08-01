package dun

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

// The guarantee, stated as a test because the failure it prevents is invisible:
// a child that keeps dun's controlling terminal can open /dev/tty and block
// reading it, and the TUI — blocked on the same tty — then loses half the
// keystrokes typed at it, with no error anywhere.
func TestDetach_NoControllingTerminal(t *testing.T) {
	cmd := detach(exec.Command("true"))
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setsid {
		t.Error("detached child must get its own session (Setsid), or it inherits dun's tty")
	}
	var found bool
	for _, e := range cmd.Env {
		if e == "GIT_TERMINAL_PROMPT=0" {
			found = true
		}
	}
	if !found {
		t.Error("GIT_TERMINAL_PROMPT=0 missing: git would fail on a bare ENXIO instead of saying why")
	}
	if len(cmd.Env) < 2 {
		t.Errorf("detach replaced the environment instead of adding to it: %v", cmd.Env)
	}
}

// detach without killGroup is a regression, not a fix: a child in its own
// session no longer dies with the terminal, so cancelling it must take the
// whole tree — the `sh` → `git push` → `git-remote-https` shape.
func TestKillGroup_CancelsTheTree(t *testing.T) {
	cmd := killGroup(exec.CommandContext(context.Background(), "true"))
	if cmd.Cancel == nil {
		t.Fatal("no Cancel: a cancelled command would leak its descendants")
	}
	if cmd.WaitDelay == 0 {
		t.Error("no WaitDelay: Wait blocks forever on a grandchild holding the output pipe")
	}
	// Cancel before Start must not panic on a nil Process.
	if err := cmd.Cancel(); err != nil {
		t.Errorf("Cancel before Start: %v", err)
	}
}

// HostExec is where model-authored commands run — the path that actually hit
// this. The wiring matters more than the helper.
func TestHostExec_DetachesTheCommand(t *testing.T) {
	out := HostExec{Dir: t.TempDir()}.Run(context.Background(), "ps -o tty= -p $$", nil)
	// No controlling terminal renders as "?" (or empty) in ps's tty column.
	if tty := strings.TrimSpace(out.Output); tty != "" && tty != "?" {
		t.Errorf("exec'd command still has a controlling terminal (%q) — it can steal the TUI's keystrokes", tty)
	}
}

// A credential prompt is not the only way git waits for a human. `git rebase
// --continue` runs `git commit … -e`, which launches $EDITOR — and an editor
// with nowhere to draw does not reliably exit, it blocks. Measured live: vim
// held a rebase for the full ten minutes until Go's test timeout killed the
// tree, and the agent read the timeout as a result and hung the same way on its
// next command.
//
// The hang is environment-dependent in the worst direction: with EDITOR unset
// git refuses with a readable error, so it never reproduces in CI.
func TestDetach_SilencesTheGitEditor(t *testing.T) {
	cmd := exec.Command("git", "rebase", "--continue")
	cmd.Env = []string{"EDITOR=vim", "VISUAL=vim"}
	detach(cmd)

	want := map[string]bool{
		"GIT_EDITOR=true":          false,
		"GIT_SEQUENCE_EDITOR=true": false,
		"GIT_TERMINAL_PROMPT=0":    false,
	}
	// LAST wins in execve, so these must come after anything inherited.
	for _, kv := range cmd.Env {
		if _, ok := want[kv]; ok {
			want[kv] = true
		}
	}
	for kv, found := range want {
		if !found {
			t.Errorf("detach must set %s — without it an unattended rebase waits for a human", kv)
		}
	}
	if last := cmd.Env[len(cmd.Env)-1]; last != "GIT_SEQUENCE_EDITOR=true" {
		t.Errorf("the overrides must come last to win over an inherited EDITOR, got %q", last)
	}
}
