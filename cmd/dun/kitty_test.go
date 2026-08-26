package main

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

func TestProbeKittyNotTTY(t *testing.T) {
	f, err := os.Open("/dev/null")
	if err != nil {
		t.Skip(err.Error())
	}
	defer f.Close()
	if probeKitty(f) {
		t.Fatal("/dev/null is not a tty; probe must report unsupported")
	}
}

// The probe talks to a real pty in these tests, because everything that was
// wrong with the previous one was invisible without a terminal on the other
// end: it read a canonical-mode tty (so read(2) blocked until the user pressed
// enter) with ECHO on (so the terminal's own reply was painted on the screen as
// garbage), and it treated ANY byte — that enter included — as proof of kitty
// support.
func kittyPTY(t *testing.T) (master, slave *os.File) {
	t.Helper()
	m, s, err := pty.Open()
	if err != nil {
		t.Skip(err.Error())
	}
	t.Cleanup(func() { m.Close(); s.Close() })
	return m, s
}

// readQuery drains the probe's query off the master end so the test can reply
// to it in order.
func readQuery(t *testing.T, master *os.File) string {
	t.Helper()
	_ = master.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 64)
	n, err := master.Read(buf)
	if err != nil {
		t.Fatalf("reading probe query: %v", err)
	}
	return string(buf[:n])
}

func TestProbeKittySupported(t *testing.T) {
	master, slave := kittyPTY(t)
	done := make(chan bool, 1)
	go func() { done <- probeKitty(slave) }()

	if q := readQuery(t, master); !strings.Contains(q, kittyProbe) || !strings.Contains(q, daProbe) {
		t.Fatalf("probe must ask the kitty query AND primary DA; got %q", q)
	}
	// A kitty-capable terminal answers the keyboard query, then DA.
	if _, err := master.WriteString("\x1b[?1u\x1b[?62;1;6c"); err != nil {
		t.Fatal(err)
	}
	if !<-done {
		t.Fatal("terminal answered CSI ? 1 u; probe must report supported")
	}
	if got := readQuery(t, master); got != kittyEnable+kittyAppMark {
		t.Errorf("on support the probe enables the protocol; wrote %q, want %q", got, kittyEnable+kittyAppMark)
	}
	kittyFD = -1 // probeKitty armed disableKitty; the test owns no real terminal
}

func TestProbeKittyDAOnlyIsUnsupported(t *testing.T) {
	master, slave := kittyPTY(t)
	done := make(chan bool, 1)
	start := time.Now()
	go func() { done <- probeKitty(slave) }()
	readQuery(t, master)
	// A terminal that answers DA but not the keyboard query: the old probe
	// called this kitty support, which is how every key became a dead key.
	if _, err := master.WriteString("\x1b[?62;1;6c"); err != nil {
		t.Fatal(err)
	}
	if <-done {
		t.Fatal("DA alone is not kitty support")
	}
	if el := time.Since(start); el >= kittyProbeTimeout {
		t.Errorf("DA is the terminator: the probe must return on it, not wait out the timeout (took %v)", el)
	}
}

// A terminal that answers nothing must cost the timeout and no more — and in
// particular must not need a keypress to get past, which is what a canonical
// -mode read would have required.
func TestProbeKittySilentTerminalDoesNotNeedAKeypress(t *testing.T) {
	_, slave := kittyPTY(t)
	done := make(chan bool, 1)
	go func() { done <- probeKitty(slave) }()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("a silent terminal is not kitty-capable")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("probe hung on a silent terminal — it is reading in canonical mode again")
	}
}

// The probe leaves the terminal exactly as it found it: bubbletea sets its own
// modes afterwards and a probe that leaked raw mode would silently change what
// it inherits.
func TestProbeKittyRestoresTermios(t *testing.T) {
	master, slave := kittyPTY(t)
	fd := int(slave.Fd())
	before, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		t.Skip(err.Error())
	}
	done := make(chan bool, 1)
	go func() { done <- probeKitty(slave) }()
	readQuery(t, master)
	master.WriteString("\x1b[?62;1;6c")
	<-done
	after, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	if *after != *before {
		t.Errorf("probe must restore termios; lflag %v -> %v", before.Lflag, after.Lflag)
	}
}
