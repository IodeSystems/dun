//go:build !linux && !darwin

package dund

import (
	"errors"
	"runtime"
)

// peerUID has no implementation here: without the kernel's word on who the
// peer is, dund refuses the connection rather than trust the client.
func peerUID(int) (uint32, error) {
	return 0, errors.New("peer credentials are not supported on " + runtime.GOOS)
}
