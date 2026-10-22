package dund

import "golang.org/x/sys/unix"

// peerUID is the uid of a unix socket's peer, from LOCAL_PEERCRED (macOS has
// no SO_PEERCRED).
func peerUID(fd int) (uint32, error) {
	cred, err := unix.GetsockoptXucred(fd, unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	if err != nil {
		return 0, err
	}
	return cred.Uid, nil
}
