package main

import "golang.org/x/sys/unix"

// getTermios is the ioctl that reads a terminal's modes (TCGETS on Linux).
const getTermios = unix.TIOCGETA
