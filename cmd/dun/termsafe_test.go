package main

import (
	"os"
	"testing"
)

// A pipe or file gets the text as written; only a terminal is filtered
// (bubbleteandmd.Sanitize is tested in its own module).
func TestSafeFor_PipeIsUnchanged(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	in := "a\x1b]52;c;SGVsbG8=\x07b\x1b[2J‮\t😀\n"
	if got := safeFor(w)(in); got != in {
		t.Errorf("a pipe's text changed: %q", got)
	}
}
