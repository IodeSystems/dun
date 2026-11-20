package harness

import (
	"strings"
	"testing"
)

// A short commit hash (git's default abbreviation is 7) panicked the engine
// on every fold: recordFacts cut each hash to 8 bytes blindly.
func TestRecordFacts_ShortCommitHash(t *testing.T) {
	h := testHarness(t)
	h.ledger.mu.Lock()
	h.ledger.open = true
	h.ledger.facts.Commits = []LedgerCommit{
		{Hash: "abc1234", Subject: "seven"},
		{Hash: "0123456789abcdef", Subject: "long"},
		{Hash: "", Subject: "empty"},
	}
	h.ledger.mu.Unlock()

	got := h.recordFacts()
	for _, want := range []string{"- abc1234 seven", "- 01234567 long", "-  empty"} {
		if !strings.Contains(got, want) {
			t.Errorf("recordFacts missing %q:\n%s", want, got)
		}
	}
}
