package dun

// safeCut and safeCutFront trim partial UTF-8 runes from the edges of a string
// so that a byte-boundary cut in exec.go cannot produce an invalid string.

import (
	"testing"
	"unicode/utf8"
)

func TestSafeCut(t *testing.T) {
	// Already valid: unchanged.
	if got := safeCut("hello"); got != "hello" {
		t.Errorf("valid string: got %q", got)
	}

	// Empty string.
	if got := safeCut(""); got != "" {
		t.Errorf("empty: got %q", got)
	}

	// Trailing partial rune (multi-byte char cut in the middle).
	// "é" is 2 bytes: 0xC3 0xA9. Cut after the first byte.
	bad := "hé"[:2] // "h" + first byte of é
	got := safeCut(bad)
	if !utf8.ValidString(got) {
		t.Errorf("result should be valid UTF-8: %q", got)
	}
	if got != "h" {
		t.Errorf("should trim the partial rune: got %q, want %q", got, "h")
	}

	// A valid multi-byte char at the end: unchanged.
	got = safeCut("héllo")
	if got != "héllo" {
		t.Errorf("valid multi-byte: got %q", got)
	}
}

func TestSafeCutFront(t *testing.T) {
	// Already valid: unchanged.
	if got := safeCutFront("hello"); got != "hello" {
		t.Errorf("valid string: got %q", got)
	}

	// Empty string.
	if got := safeCutFront(""); got != "" {
		t.Errorf("empty: got %q", got)
	}

	// Leading partial rune (second half of a multi-byte char).
	// "é" = 0xC3 0xA9. Take just the second byte.
	bad := []byte{0xA9, 'h', 'i'}
	got := safeCutFront(string(bad))
	if !utf8.ValidString(got) {
		t.Errorf("result should be valid UTF-8: %q", got)
	}
	if got != "hi" {
		t.Errorf("should trim the leading partial rune: got %q, want %q", got, "hi")
	}

	// Valid multi-byte char at the start: unchanged.
	got = safeCutFront("héllo")
	if got != "héllo" {
		t.Errorf("valid multi-byte: got %q", got)
	}
}
