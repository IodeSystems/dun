package main

import (
	"bytes"
	"io"
	"os"

	"golang.org/x/term"
)

// rawMode puts a terminal into raw mode and returns the restore func. Not ok
// when the file is not a terminal (a test, a pipe, `dun -serve`'s pty before it
// is wired) — the caller then leaves bubbletea to manage its own input, which
// costs the shift+enter rewrite and nothing else.
func rawMode(f *os.File) (restore func(), ok bool) {
	fd := int(f.Fd())
	if !term.IsTerminal(fd) {
		return nil, false
	}
	st, err := term.MakeRaw(fd)
	if err != nil {
		return nil, false
	}
	return func() { _ = term.Restore(fd, st) }, true
}

// shift+enter, rescued before bubbletea's parser gets it.
//
// Terminals report shift+enter three different ways and bubbletea v1 loses all
// three. Measured against the real parser:
//
//	ESC O M        → KeyRunes "M", alt=false  — byte-identical to typing M.
//	                 ESC O parses as SS3, "M" is not in the SS3 table, and the
//	                 final byte falls through as a bare rune.
//	ESC [ 13;2u    → nothing at all; the sequence is swallowed.
//	ESC [ 27;2;13~ → likewise.
//
// So it cannot be bound in Update: a binding on "M" would make the letter M
// insert a newline. It CAN be fixed one layer down, by rewriting the bytes
// before they are parsed. Each sequence becomes a single LF (0x0a), which
// bubbletea reports as ctrl+j — already the composer's "insert a newline" key,
// so this adds a spelling rather than a meaning.
//
// LF is safe to synthesise: enter sends CR (0x0d), and a pasted newline arrives
// inside a bracketed paste, so a bare LF is not otherwise produced by a
// terminal in raw mode.
type keyFilter struct {
	r    io.Reader
	pend []byte // rewritten, waiting for the caller
	hold []byte // raw bytes that may be the start of a sequence
}

// shiftEnterSeqs are the encodings rewritten to LF.
var shiftEnterSeqs = [][]byte{
	[]byte("\x1bOM"),        // SS3 M — the common one
	[]byte("\x1b[13;2u"),    // CSI-u (kitty, foot, wezterm)
	[]byte("\x1b[27;2;13~"), // xterm modifyOtherKeys
}

// maxSeq is the longest sequence, and so the most bytes that can be held back
// waiting to find out whether they are one.
var maxSeq = func() int {
	n := 0
	for _, s := range shiftEnterSeqs {
		if len(s) > n {
			n = len(s)
		}
	}
	return n
}()

func newKeyFilter(r io.Reader) *keyFilter { return &keyFilter{r: r} }

func (k *keyFilter) Read(p []byte) (int, error) {
	for len(k.pend) == 0 {
		tmp := make([]byte, 4096)
		n, err := k.r.Read(tmp)
		if n > 0 {
			k.hold = append(k.hold, tmp[:n]...)
			emit, hold := rewriteKeys(k.hold, false)
			k.pend, k.hold = append(k.pend, emit...), hold
		}
		if err != nil {
			// Flush whatever is held, unrewritten: a truncated sequence at EOF is
			// not a keystroke and must not be waited on forever.
			if len(k.hold) > 0 {
				emit, _ := rewriteKeys(k.hold, true)
				k.pend, k.hold = append(k.pend, emit...), nil
			}
			if len(k.pend) == 0 {
				return 0, err
			}
			break
		}
	}
	n := copy(p, k.pend)
	k.pend = append(k.pend[:0], k.pend[n:]...)
	return n, nil
}

// rewriteKeys replaces every complete sequence with LF, returning what to emit
// and what to hold for the next read.
//
// A trailing PARTIAL sequence is held only once at least two bytes of it have
// arrived. A lone trailing ESC is passed straight through, because holding it
// would make the Escape key do nothing until the NEXT keystroke arrived — and
// terminals write an escape sequence in one burst, so the split-read case this
// gives up on is the rare one, degrading to the behaviour without the filter.
func rewriteKeys(b []byte, flush bool) (emit, hold []byte) {
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); {
		if b[i] != 0x1b {
			out = append(out, b[i])
			i++
			continue
		}
		rest := b[i:]
		if m := matchSeq(rest); m > 0 {
			out = append(out, '\n')
			i += m
			continue
		}
		if !flush && len(rest) >= 2 && isSeqPrefix(rest) {
			return out, append([]byte(nil), rest...)
		}
		out = append(out, b[i])
		i++
	}
	return out, nil
}

// matchSeq returns the length of the sequence at the front of b, or 0.
func matchSeq(b []byte) int {
	for _, s := range shiftEnterSeqs {
		if bytes.HasPrefix(b, s) {
			return len(s)
		}
	}
	return 0
}

// isSeqPrefix reports whether b could still grow into a sequence.
func isSeqPrefix(b []byte) bool {
	if len(b) >= maxSeq {
		return false
	}
	for _, s := range shiftEnterSeqs {
		if bytes.HasPrefix(s, b) {
			return true
		}
	}
	return false
}
