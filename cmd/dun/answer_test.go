package main

import "testing"

func TestAnswerOf(t *testing.T) {
	for in, want := range map[string]string{
		"I looked at go.mod.\n# RESPONSE BELOW\n1.26\n":            "1.26",
		"no delimiter, just this":                                  "no delimiter, just this",
		"a\n# RESPONSE BELOW\nfirst\n# RESPONSE BELOW\n  second  ": "second",
		"": "",
	} {
		if got := answerOf(in); got != want {
			t.Errorf("answerOf(%q) = %q, want %q", in, got, want)
		}
	}
}
