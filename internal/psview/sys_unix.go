//go:build unix

package psview

import (
	"fmt"
	"os"
	"syscall"
)

func kill(pid int, force bool) error {
	sig := syscall.SIGTERM
	if force {
		sig = syscall.SIGKILL
	}
	return syscall.Kill(pid, sig)
}

// execResume replaces this process with `dun --resume id`, run in the
// session's workspace (the session file is looked up by workspace).
func execResume(id, workspace string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if workspace != "" {
		if err := os.Chdir(workspace); err != nil {
			return fmt.Errorf("open %s: %w", id, err)
		}
	}
	return syscall.Exec(exe, []string{"dun", "--resume", id}, os.Environ())
}
