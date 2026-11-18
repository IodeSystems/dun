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

func waitPid(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return nil
	}
	_, err = p.Wait()
	return err
}
