package main

import (
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Kitty keyboard protocol: probe and enable, the way Claude Code does it.
//
// What this changes on Termux (measured: Claude Code enables full mouse mode
// AND this, so the IME popping there is not a mouse-mode story — it is the
// input-classification story): a terminal that speaks the kitty protocol
// reports keys at the keysym level, and tap → text-entry hand-off follows the
// terminal app's path rather than being eaten as a pointer event.
//
// We probe first (`CSI > 0 q`) and enable only on a reply: enabling on a
// terminal that does not speak it would turn every key into a
// `CSI <code> u` sequence that bubbletea v1 does not decode — we would break
// every key for every user. No reply within the timeout, and the terminal is
// left exactly as found.
//
// Modes enabled on support: 1 (disambiguate escape codes) and 4 (report
// alternative keys) — the same pair as Claude Code (`CSI > 1 u`, `CSI > 4;2m`).
// Mode 2 (application keypad) is deliberately OFF: it re-codes arrow keys and
// enter into `u` sequences bubbletea has no table for, which would turn them
// into dead keys.

const (
	kittyProbe     = "\x1b[>0q"
	kittyEnable    = "\x1b[>1;4u" // disambiguate + alternate keys
	kittyAppMark   = "\x1b[>4;2m" // expect kitty-style key reports
	kittyDisable   = "\x1b[>0u"
	// A real terminal answers the capability query in milliseconds; a pipe or
	// a kitty-less terminal answers never, so the timeout is where "unsupported"
	// lives. Long enough not to race a slow pty, short enough that dun's start
	// does not feel it.
	kittyProbeTimeout = 150 * time.Millisecond
)

// kittyFD is the tty the protocol was enabled on, so disableKitty can put it
// back. -1 when probing failed and nothing was changed.
var kittyFD = -1

func isTTY(f *os.File) bool {
	var st syscall.Stat_t
	return syscall.Fstat(int(f.Fd()), &st) == nil && st.Mode&syscall.S_IFMT == syscall.S_IFCHR
}

// probeKitty queries the terminal for kitty keyboard protocol support and, on
// support, enables it. It must run BEFORE the terminal is put in raw mode:
// raw mode sets O_NONBLOCK, and with it set we would swallow the terminal's
// reply as if it were a keypress. Not ok for non-tty stdin (tests, `dun
// -serve` before the pty is wired) — the caller leaves input to bubbletea.
func probeKitty(f *os.File) bool {
	if !isTTY(f) {
		return false
	}
	fd := int(f.Fd())
	// Clear O_NONBLOCK if it is set, so the reply is readable at all.
	if flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0); err == nil {
		_, _ = unix.FcntlInt(uintptr(fd), unix.F_SETFL, flags&^unix.O_NONBLOCK)
	}
	if _, err := f.Write([]byte(kittyProbe)); err != nil {
		return false
	}
	// The reply is `CSI > flags ; versions ST`. Presence is the test — the
	// baseline modes we enable (1 and 4) are what every conforming
	// kitty-capable terminal implements, so parsing the flags adds nothing.
	deadline := time.Now().Add(kittyProbeTimeout)
	buf := make([]byte, 64)
	for time.Now().Before(deadline) {
		n, err := f.Read(buf)
		if err != nil {
			if err == syscall.EAGAIN || err == syscall.EWOULDBLOCK || err == os.ErrDeadlineExceeded {
				continue
			}
			return false
		}
		if n > 0 {
			// We already decided on that byte; do not let it reach bubbletea's
			// input loop, where it would parse as a spurious key.
			if _, err := f.Write([]byte(kittyEnable + kittyAppMark)); err != nil {
				return false
			}
			kittyFD = fd
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return false
}

// disableKitty puts the terminal back: kitty protocol off, so the shell the
// user lands in stops expecting kitty-encoded keys.
func disableKitty() {
	if kittyFD < 0 {
		return
	}
	// O_NONBLOCK before the write so it cannot block on a slow tty.
	if flags, err := unix.FcntlInt(uintptr(kittyFD), unix.F_GETFL, 0); err == nil {
		_, _ = unix.FcntlInt(uintptr(kittyFD), unix.F_SETFL, flags|unix.O_NONBLOCK)
	}
	_, _ = os.Stderr.WriteString(kittyDisable)
	kittyFD = -1
}
