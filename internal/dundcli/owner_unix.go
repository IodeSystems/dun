//go:build unix

package dundcli

import (
	"io/fs"
	"os"
	"syscall"
)

// ownedByMe reports whether a file belongs to this process's user.
func ownedByMe(st fs.FileInfo) bool {
	sys, ok := st.Sys().(*syscall.Stat_t)
	return ok && int(sys.Uid) == os.Getuid()
}
