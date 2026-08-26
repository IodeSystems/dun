package dun

// tailOf keeps the end of a log: a failure is at the bottom.

import (
	"strings"
	"testing"
)

func TestTailOf_Short(t *testing.T) {
	if got := tailOf("hello", 100); got != "hello" {
		t.Errorf("short string: got %q", got)
	}
}

func TestTailOf_Empty(t *testing.T) {
	if got := tailOf("", 100); got != "(no output)" {
		t.Errorf("empty: got %q", got)
	}
	if got := tailOf("\n\n", 100); got != "(no output)" {
		t.Errorf("trailing newlines only: got %q", got)
	}
}

func TestTailOf_Exact(t *testing.T) {
	s := "0123456789"
	if got := tailOf(s, len(s)); got != s {
		t.Errorf("exact fit: got %q, want %q", got, s)
	}
}

func TestTailOf_Truncates(t *testing.T) {
	s := "line1\nline2\nline3\nline4\nline5\n"
	got := tailOf(s, 20)
	if !strings.Contains(got, "…(truncated") {
		t.Errorf("should have truncation marker: %q", got)
	}
	// Should end with the last line (after a newline cut).
	if !strings.HasSuffix(got, "line5") {
		t.Errorf("should end with line5: %q", got)
	}
}

func TestTailOf_CutsAtNewline(t *testing.T) {
	s := "aaaaabbbbb\ncccc"
	got := tailOf(s, 7)
	// cut = s[len(s)-7:] = "bbbb\nccc"
	// IndexByte finds '\n' at index 4, cut = cut[5:] = "ccc"
	if !strings.Contains(got, "ccc") {
		t.Errorf("should cut at newline: %q", got)
	}
}

func TestTailOf_NoNewlineInCut(t *testing.T) {
	s := "xxxxx" + strings.Repeat("a", 100)
	got := tailOf(s, 10)
	// cut = last 10 chars, all 'a', no newline
	if !strings.HasSuffix(got, strings.Repeat("a", 10)) {
		t.Errorf("no newline in cut: got %q", got)
	}
}
