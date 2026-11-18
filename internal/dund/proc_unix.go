//go:build unix

package dund

import (
	"fmt"
	"os/exec"
	"syscall"
	"time"
)

// ownGroup starts an engine in a process group of its own, so killGroup
// reaches what it started too (MCP servers, jobs) — any of which holding the
// engine's stdout open would otherwise keep a stop waiting for EOF forever.
func ownGroup(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

func killGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	_ = cmd.Process.Kill()
}

// killPidGroup kills an adopted engine: its process group when the group is
// its own (ownGroup), and the process.
func killPidGroup(pid int) {
	if pid <= 1 {
		return
	}
	if pgid, err := syscall.Getpgid(pid); err == nil && pgid == pid {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
}

// pidPoll is how often waitPid looks for a process that is not dund's child.
var pidPoll = 200 * time.Millisecond

// waitPid waits for an adopted engine to be gone: reaped when it is still
// dund's child (an in-place exec keeps children), else polled.
func waitPid(pid int) error {
	for {
		var ws syscall.WaitStatus
		_, err := syscall.Wait4(pid, &ws, 0, nil)
		if err == syscall.EINTR {
			continue
		}
		if err == nil {
			switch {
			case ws.Signaled():
				return fmt.Errorf("signal: %v", ws.Signal())
			case ws.Exited() && ws.ExitStatus() != 0:
				return fmt.Errorf("exit status %d", ws.ExitStatus())
			}
			return nil
		}
		break // ECHILD: another parent's
	}
	for {
		if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
			return nil
		}
		time.Sleep(pidPoll)
	}
}
