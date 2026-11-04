package tui

import (
	"strings"
	"testing"
)

// /provider names the one in use and its model, and says for every other
// one what it lists or why it lists nothing.
func TestProviderListing(t *testing.T) {
	rows := []providerStatus{
		{name: "corp", url: "https://gw.corp.example/v1/", key: true, models: 142},
		{name: "corrallm", url: "https://llm.example/v1", key: true, models: 1},
		{name: "local", url: "http://127.0.0.1:8080/v1", err: "connection refused"},
	}
	got := stripANSI(providerListing(rows, "https://gw.corp.example/v1", "gpt-x"))
	for _, want := range []string{
		"● corp — in use: gpt-x",
		"https://gw.corp.example/v1/ · key · 142 models",
		"  corrallm\n    https://llm.example/v1 · key · 1 model",
		"  local\n    http://127.0.0.1:8080/v1 · no key · ✗ connection refused",
		"/provider login [name]",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("listing lacks %q:\n%s", want, got)
		}
	}
	if strings.Count(got, "●") != 1 {
		t.Errorf("want exactly one provider in use:\n%s", got)
	}
}
