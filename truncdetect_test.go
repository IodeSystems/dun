package dun

import "testing"

// Truncation and bad syntax need DIFFERENT advice to the model: "retry smaller"
// vs "fix your syntax". Reporting both as "unexpected end of JSON input" is what
// made a provider-side cut look like a model mistake.
func TestIsTruncatedJSON(t *testing.T) {
	truncated := []string{
		`{"node":"a.java","newText":"package com.termux.app;`, // the real failure shape
		`{"a":`,
		`{"a":[1,2`,
		`{`,
	}
	for _, s := range truncated {
		if !isTruncatedJSON(s) {
			t.Errorf("should be detected as truncated: %q", s)
		}
	}
	malformed := []string{
		`{"a":,}`,  // stray comma
		`{a:1}`,    // unquoted key
		`{"a":1}}`, // trailing brace
		`not json`, // not even close
	}
	for _, s := range malformed {
		if isTruncatedJSON(s) {
			t.Errorf("should NOT be called truncation (it is bad syntax): %q", s)
		}
	}
	// complete documents are neither
	if isTruncatedJSON(`{"a":1}`) {
		t.Error("valid JSON reported as truncated")
	}
}
