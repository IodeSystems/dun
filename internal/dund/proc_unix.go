//go:build unix

package dund

import (
	"os/exec"
	"syscall"
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
