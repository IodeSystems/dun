package main

import (
	"strings"
	"testing"

	"github.com/iodesystems/dun/internal/dunconfig"
)

// Headless, /login cannot be typed: the text names the command-line ways in
// (--setup, the env vars that exist), never /login.
func TestHeadlessMissing_SaysTheCommandLine(t *testing.T) {
	for _, err := range []error{dunconfig.ProviderMissing("", ""), dunconfig.ProviderMissing("http://x", "")} {
		s := headlessMissing(err)
		if strings.Contains(s, "/login") || !strings.Contains(s, "dun --setup") {
			t.Errorf("%v:\n%s", err, s)
		}
	}
	s := headlessMissing(dunconfig.ErrNoLogin)
	for _, want := range []string{"DUN_URL=", "DUN_MODEL=", "DUN_LLM_KEY="} {
		if !strings.Contains(s, want) {
			t.Errorf("lacks %s:\n%s", want, s)
		}
	}
	if !strings.Contains(setupNoTTY, "DUN_URL=") || strings.Contains(setupNoTTY, "bubbletea") {
		t.Errorf("--setup without a terminal:\n%s", setupNoTTY)
	}
}
