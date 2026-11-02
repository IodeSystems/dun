package main

import (
	"strings"
	"testing"
)

// The picks lead, in order; the aliases follow; the rest are after.
func TestClaudeRank(t *testing.T) {
	ids := []string{"claude-opus-5-5", "claude-sonnet-5-5", "claude-haiku-5-5", "opus-latest", "fable-latest", "claude-opus-4-8"}
	for i := 1; i < len(ids); i++ {
		if claudeRank(ids[i-1]) >= claudeRank(ids[i]) {
			t.Errorf("%s should come before %s", ids[i-1], ids[i])
		}
	}
}

func TestSuggestedModel_OpusFive(t *testing.T) {
	rows := []modelRow{{"a", "claude-haiku-5-5"}, {"a", "chat"}, {"a", claudeDefault}}
	if got := suggestedModel(rows); got != 2 {
		t.Errorf("row %d", got)
	}
}

func TestClaudeNote(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	if got := claudeNote(claudeDefault); !strings.Contains(got, "recommended") || !strings.Contains(got, "~$4 / $20") {
		t.Errorf("%q", got)
	}
	if got := claudeNote("opus-latest"); got != "the newest opus" {
		t.Errorf("%q", got)
	}
	if got := claudeNote("deepseek/x"); got != "" {
		t.Errorf("%q", got)
	}
}

// Over ssh the browser would open on the wrong machine.
func TestOpenKeysPage_NotOverSSH(t *testing.T) {
	t.Setenv("SSH_CONNECTION", "1.2.3.4 1 5.6.7.8 22")
	if openKeysPage("https://platform.claude.com/settings/keys") {
		t.Error("tried to open a browser over ssh")
	}
}
