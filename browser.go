package dun

import "os/exec"

// startDetached starts cmd and reaps it in the background.
func startDetached(cmd *exec.Cmd) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
