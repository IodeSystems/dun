package main

import (
	"regexp"
	"strings"
	"testing"
)

var ansiRE = regexp.MustCompile("\x1b\\[[0-9;]*[a-zA-Z]")

func visible(s string) string { return ansiRE.ReplaceAllString(s, "") }

// The only thing that must never change is what the user sees.
func TestPackSGRKeepsVisibleText(t *testing.T) {
	cases := []string{
		"plain text, no escapes at all",
		"\x1b[38;5;252mword\x1b[0m\x1b[38;5;252m 26\x1b[0m\x1b[38;5;252m \x1b[0m   ",
		"\x1b[1m\x1b[31mred bold\x1b[0m then \x1b[32mgreen\x1b[0m",
		"multi\nline\n\x1b[36mcyan\x1b[0m",
		"\x1b[2;36r\x1b[36;Hcursor sequences must survive\x1b[0m",
		"truncated escape at end \x1b[38;5;",
		"\x1b]8;;https://example.com\x07link\x1b]8;;\x07",
	}
	for _, in := range cases {
		got := packSGR(in)
		if visible(got) != visible(in) {
			t.Errorf("visible text changed\n in: %q\nout: %q", in, got)
		}
	}
}

// The point of the exercise: the per-word colour churn collapses.
func TestPackSGRShrinksPerWordStyling(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 12; i++ {
		b.WriteString("\x1b[38;5;252mword\x1b[0m\x1b[38;5;252m \x1b[0m")
	}
	in := b.String()
	got := packSGR(in)
	if len(got) >= len(in)/2 {
		t.Errorf("expected the run to at least halve: %d -> %d bytes", len(in), len(got))
	}
	if visible(got) != visible(in) {
		t.Fatalf("visible text changed: %q", got)
	}
	if !strings.Contains(got, "\x1b[38;5;252m") {
		t.Errorf("the colour itself must survive: %q", got)
	}
}

// Non-SGR sequences are structure, not style: cursor moves, margins and erases
// have to come through in order and unchanged, or the screen desyncs.
func TestPackSGRLeavesControlSequencesAlone(t *testing.T) {
	in := "\x1b[2;36r\x1b[2;H\x1b[35L\x1b[38;5;252mtext\x1b[0m\x1b[K"
	got := packSGR(in)
	for _, seq := range []string{"\x1b[2;36r", "\x1b[2;H", "\x1b[35L", "\x1b[K"} {
		if !strings.Contains(got, seq) {
			t.Errorf("dropped %q from %q", seq, got)
		}
	}
	if strings.Index(got, "\x1b[2;36r") > strings.Index(got, "\x1b[35L") {
		t.Errorf("resequenced control codes: %q", got)
	}
}
