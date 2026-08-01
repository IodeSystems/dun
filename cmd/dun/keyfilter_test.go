package main

import (
	"io"
	"strings"
	"testing"
)

// readAll drains the filter the way bubbletea does — many small reads.
func readAll(t *testing.T, r io.Reader) string {
	t.Helper()
	var out []byte
	buf := make([]byte, 8)
	for {
		n, err := r.Read(buf)
		out = append(out, buf[:n]...)
		if err == io.EOF {
			return string(out)
		}
		if err != nil {
			t.Fatalf("read: %v", err)
		}
	}
}

// Every spelling of shift+enter becomes LF, which bubbletea reports as ctrl+j —
// the composer's existing "insert a newline".
func TestKeyFilter_RewritesShiftEnter(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"SS3 M", "a\x1bOMb", "a\nb"},
		{"CSI-u", "a\x1b[13;2ub", "a\nb"},
		{"modifyOtherKeys", "a\x1b[27;2;13~b", "a\nb"},
		{"twice in one read", "\x1bOM\x1bOM", "\n\n"},
		{"nothing to do", "hello world", "hello world"},
	}
	for _, c := range cases {
		if got := readAll(t, newKeyFilter(strings.NewReader(c.in))); got != c.want {
			t.Errorf("%s: want %q, got %q", c.name, c.want, got)
		}
	}
}

// The letter M must stay the letter M. This is the whole reason the rewrite
// happens here rather than as a key binding: after bubbletea's parser the two
// are the same event.
func TestKeyFilter_LeavesPlainTextAlone(t *testing.T) {
	const in = "MOM sent OM and \x1bOA" // …including a real SS3 (up arrow)
	if got := readAll(t, newKeyFilter(strings.NewReader(in))); got != in {
		t.Fatalf("text was rewritten: want %q, got %q", in, got)
	}
}

// A lone ESC is the Escape key. Holding it back to see whether a sequence
// follows would make esc do nothing until the NEXT keystroke arrived.
func TestKeyFilter_LoneEscPassesStraightThrough(t *testing.T) {
	r, w := io.Pipe()
	k := newKeyFilter(r)
	go func() {
		w.Write([]byte{0x1b})
	}()
	buf := make([]byte, 8)
	n, err := k.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf[:n]) != "\x1b" {
		t.Fatalf("esc should arrive on its own, got %q", buf[:n])
	}
}

// A sequence split across reads is still recognised: the partial is held once
// two bytes of it have arrived.
func TestKeyFilter_ReassemblesASplitSequence(t *testing.T) {
	r, w := io.Pipe()
	k := newKeyFilter(r)
	go func() {
		w.Write([]byte("x\x1bO"))
		w.Write([]byte("My"))
		w.Close()
	}()
	if got := readAll(t, k); got != "x\ny" {
		t.Fatalf("split sequence lost: got %q", got)
	}
}

// A truncated sequence at EOF is not a keystroke — it must be flushed, not
// waited on.
func TestKeyFilter_FlushesATruncatedSequence(t *testing.T) {
	if got := readAll(t, newKeyFilter(strings.NewReader("x\x1bO"))); got != "x\x1bO" {
		t.Fatalf("truncated tail should be flushed as-is, got %q", got)
	}
}
