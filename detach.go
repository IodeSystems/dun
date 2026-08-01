package dun

import (
	"os"
	"os/exec"
	"syscall"
	"time"
)

// Isolation, tier 0 — the terminal.
//
// Every child dun spawns inherits dun's controlling terminal, and that is not
// only dun's problem: `git push` to an https remote with no cached credential
// opens /dev/tty DIRECTLY and blocks reading it. The prompt does not go through
// stdin, so pointing stdin at /dev/null (which CombinedOutput already does)
// stops nothing. The TUI's Bubble Tea reader is blocked on that same tty, so
// the kernel hands each arriving keystroke to whichever of the two readers it
// wakes first — the TUI swallows roughly half of what you type, and git's
// prompt is invisible underneath the alt-screen. Measured: a push sat on a
// credential prompt for 21 minutes while the session looked merely broken.
//
// It is not a git problem either. ssh, sudo, docker login and anything else
// that wants a password does the same thing. The fix has to be at the spawn
// site, for every spawn.

// detach severs cmd from dun's controlling terminal. Setsid puts it in a new
// session with no ctty, so open("/dev/tty") fails and the child can never race
// the TUI for input. GIT_TERMINAL_PROMPT=0 is the same guarantee said in a
// language git will quote back in a readable error, rather than failing on a
// bare ENXIO.
//
// GIT_EDITOR/GIT_SEQUENCE_EDITOR are the same hazard by a different route, and
// they were missed the first time. A credential prompt is not the only way git
// waits for a human: `git rebase --continue` runs
//
//	git commit -n --no-gpg-sign -F .git/rebase-merge/message -e
//
// and that -e launches $EDITOR. Measured on a live session: EDITOR=vim (from
// the operator's login shell, which `sh -lc` sources), vim sat there for the
// full ten minutes until Go's test timeout killed the process tree — and the
// agent, reading "FAIL … 600.024s", simply ran the next command and hung the
// same way. Setsid does NOT save you here: an editor with nowhere to draw does
// not necessarily exit, it just blocks somewhere else.
//
// `true` is the editor because it exits 0 immediately and leaves the prepared
// message alone, which is exactly what an unattended rebase wants. Note this is
// environment-dependent in the worst way: with EDITOR unset, git refuses with a
// readable error instead of waiting, so the hang is invisible in CI and on any
// machine whose operator does not set one.
//
// Callers that spawn with CommandContext should pair this with killGroup.
func detach(cmd *exec.Cmd) *exec.Cmd {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	cmd.Env = append(cmd.Env,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_EDITOR=true",
		"GIT_SEQUENCE_EDITOR=true",
	)
	return cmd
}

// killGroupDelay bounds how long Wait tolerates a child that ignored the kill
// (or grandchildren still holding the output pipe) before giving up on it.
const killGroupDelay = 5 * time.Second

// killGroup makes context cancellation kill the child's whole process group
// rather than just the child.
//
// This is the other half of detach, not a separate nicety: a detached child no
// longer dies when the terminal hangs up, so without it a cancelled `sh -lc`
// would leave behind the descendants that were doing the real work — exactly
// the `sh` → `git push` → `git-remote-https` tree that motivated this. Setsid
// makes the child its own group leader, so -pid addresses the whole tree.
func killGroup(cmd *exec.Cmd) *exec.Cmd {
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = killGroupDelay
	return cmd
}
