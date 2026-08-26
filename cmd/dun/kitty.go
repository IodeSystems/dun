package main

import (
	"os"
	"regexp"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// Kitty keyboard protocol: probe and enable.
//
// Why dun cares: on Termux a terminal that reports keys at the keysym level
// hands taps to the text-entry path instead of eating them, so the composer
// behaves the way Claude Code's does.
//
// Measured against claude cli 2.1.246 in a pty, its startup writes:
//
//	ESC[?2004h ESC[?1004h ESC[?2031h   bracketed paste, focus, theme change
//	ESC[<u ESC[>1u                     kitty: pop flags, push flags=1
//	ESC[>4;2m                          xterm modifyOtherKeys=2 — NOT kitty
//
// (plus 1000/1002/1003/1006 once past its trust prompt — see tui.go). Two
// things that capture corrects about the earlier version of this file:
// `CSI > 0 q` is XTVERSION, not a kitty query (claude sends it once at the END
// of startup, to identify the terminal, not to gate anything), and
// `CSI > 4 ; 2 m` is xterm's modifyOtherKeys rather than a kitty mode 4 — so
// we push flags=1 alone.
//
// Unlike claude we still probe rather than enabling blind, because bubbletea
// v1 has no table for `CSI <code> u`: on a terminal that answers the query but
// encodes keys we cannot decode, enabling blind turns every key into a dead
// key. The query is the real one (`CSI ? u`, answered `CSI ? flags u`), paired
// with a primary DA (`CSI c`) that every terminal answers — DA is the
// terminator, so a kitty-less terminal costs one round trip instead of the
// whole timeout, and no reply at all leaves the terminal exactly as found.

const (
	kittyProbe   = "\x1b[?u"  // kitty keyboard query; reply is CSI ? flags u
	daProbe      = "\x1b[c"   // primary DA; its reply terminates the read
	kittyEnable  = "\x1b[>1u" // push flags=1 (disambiguate escape codes)
	kittyAppMark = "\x1b[>4;2m"
	kittyDisable = "\x1b[<u" // pop the flags we pushed
	// A real terminal answers both queries in milliseconds; a pipe or a
	// kitty-less terminal that also swallows DA answers never, so the timeout is
	// where "unsupported" lives. Long enough not to race a slow pty, short
	// enough that dun's start does not feel it.
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
// support, enables it. It must run BEFORE bubbletea starts, and it puts the
// tty in raw mode for its own duration: in canonical mode read(2) does not
// return until a newline — the probe would hang until the user pressed enter —
// and ECHO paints the terminal's replies on the screen as garbage. The
// terminal is restored before returning, so bubbletea finds it as it was. Not
// ok for non-tty stdin (tests, `dun -serve` before the pty is wired) — the
// caller leaves input to bubbletea.
func probeKitty(f *os.File) bool {
	if !isTTY(f) {
		return false
	}
	fd := int(f.Fd())
	st, err := term.MakeRaw(fd)
	if err != nil {
		return false
	}
	defer func() { _ = term.Restore(fd, st) }()
	// Clear O_NONBLOCK if it is set, so a ready fd reads rather than EAGAINs.
	if flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0); err == nil {
		_, _ = unix.FcntlInt(uintptr(fd), unix.F_SETFL, flags&^unix.O_NONBLOCK)
	}
	if _, err := f.Write([]byte(kittyProbe + daProbe)); err != nil {
		return false
	}
	// Read until DA answers (or the deadline), then decide on what arrived.
	// Everything read here is consumed deliberately: it is the terminal talking
	// to us, and letting it reach bubbletea's input loop would parse as spurious
	// keys. Anything the user typed during the probe goes with it — a keystroke
	// in the first few ms of startup, which is cheaper than a garbled composer.
	deadline := time.Now().Add(kittyProbeTimeout)
	var reply []byte
	buf := make([]byte, 256)
	for {
		wait := time.Until(deadline)
		if wait <= 0 {
			break
		}
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, int(wait.Milliseconds())+1)
		if err == unix.EINTR {
			continue
		}
		if err != nil || n == 0 {
			break
		}
		n, err = f.Read(buf)
		if err != nil {
			if err == syscall.EAGAIN || err == syscall.EWOULDBLOCK {
				continue
			}
			break
		}
		reply = append(reply, buf[:n]...)
		if daReply.Match(reply) {
			break
		}
	}
	if !kittyReply.Match(reply) {
		return false
	}
	if _, err := f.Write([]byte(kittyEnable + kittyAppMark)); err != nil {
		return false
	}
	kittyFD = fd
	return true
}

// kittyReply is the answer to `CSI ? u`; daReply is the primary DA answer,
// which every terminal sends and which therefore ends the read. Both are
// matched anywhere in the buffer — the two replies arrive in one read as often
// as not, and other unsolicited reports can be interleaved with them.
var (
	kittyReply = regexp.MustCompile(`\x1b\[\?[0-9;]*u`)
	daReply    = regexp.MustCompile(`\x1b\[\?[0-9;]*c`)
)

// disableKitty puts the terminal back: pop the flags we pushed, so the shell
// the user lands in stops expecting kitty-encoded keys.
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
