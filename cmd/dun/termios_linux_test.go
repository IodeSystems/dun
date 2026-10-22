package main

import "golang.org/x/sys/unix"

// getTermios is the ioctl that reads a terminal's modes.
const getTermios = unix.TCGETS
