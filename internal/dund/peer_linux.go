package dund

import "golang.org/x/sys/unix"

// peerUID is the uid of a unix socket's peer, from SO_PEERCRED.
func peerUID(fd int) (uint32, error) {
	cred, err := unix.GetsockoptUcred(fd, unix.SOL_SOCKET, unix.SO_PEERCRED)
	if err != nil {
		return 0, err
	}
	return cred.Uid, nil
}
