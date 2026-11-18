//go:build !unix

package dund

import (
	"os"
	"os/exec"
)

func ownGroup(*exec.Cmd) {}

func killGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

func killPidGroup(pid int) {
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}

func reapExited() {}

func waitPid(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return nil
	}
	_, err = p.Wait()
	return err
}

// pidAlive cannot tell here: taken as alive, so nothing is assumed gone.
func pidAlive(pid int) bool { return pid > 0 }
