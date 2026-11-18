//go:build !linux && !darwin

package procps

import (
	"errors"
	"runtime"
)

// IONote says what the IO column can show here.
const IONote = "IO is not read on this platform"

// Read is not implemented here.
func Read() (Snapshot, error) {
	return Snapshot{}, errors.New("dun ps reads processes on Linux and macOS, not " + runtime.GOOS)
}

// Cwd is unknown here.
func Cwd(int) string { return "" }
