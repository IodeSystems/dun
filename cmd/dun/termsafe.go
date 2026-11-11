package main

import (
	"os"

	"github.com/iodesystems/bubbleteandmd"
	"golang.org/x/term"
)

// safeFor returns the filter for text bound for f: when f is a terminal,
// bubbleteandmd.Sanitize (model and tool text must not carry escapes such as
// OSC 52 clipboard writes, titles, cursor moves or screen clears); when f is
// a pipe or file, the text unchanged, so scripts get the bytes as written.
func safeFor(f *os.File) func(string) string {
	if term.IsTerminal(int(f.Fd())) {
		return bubbleteandmd.Sanitize
	}
	return func(s string) string { return s }
}

// Filters for the two standard streams, set once at start.
var (
	safeOut = safeFor(os.Stdout)
	safeErr = safeFor(os.Stderr)
)
