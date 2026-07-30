package dun

// Compaction is the one operation that destroys conversation. Two sessions in
// this workspace lost 90% and 84% of their entries to it in July 2026 —
// silently, because nothing reported it. These tests cover both halves of that
// failure: the trigger, and the invisibility.

import (
	"os"
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/agent"
)

// The default (DUN_CONTEXT_TOKENS unset) must mean NO shaping. It reads as 0,
// and a 0 that anything downstream treats as "a budget of zero tokens" makes
// every turn over budget.
func TestContextBudget_UnsetMeansUnbudgeted(t *testing.T) {
	t.Setenv("DUN_CONTEXT_TOKENS", "")
	if got := contextBudget(); got != 0 {
		t.Fatalf("unset should be 0 (unbudgeted), got %d", got)
	}
	t.Setenv("DUN_CONTEXT_TOKENS", "not-a-number")
	if got := contextBudget(); got != 0 {
		t.Errorf("garbage should fall back to unbudgeted, got %d", got)
	}
	t.Setenv("DUN_CONTEXT_TOKENS", "100000")
	if got := contextBudget(); got != 90000 {
		t.Errorf("a real window should shape to 90%%: got %d", got)
	}
	_ = os.Unsetenv("DUN_CONTEXT_TOKENS")
}

// A fold has to be reviewable after the fact: how much was destroyed, what it
// cost, and whether it is happening more than once a turn.
func TestCompactionNote_ReportsTheDamage(t *testing.T) {
	n := CompactionNote{Subsumed: 12, TokensBefore: 30000, TokensAfter: 8000}
	s := n.String()
	for _, want := range []string{"12 entries", "30000", "8000", "saved 22000"} {
		if !strings.Contains(s, want) {
			t.Errorf("note is missing %q: %s", want, s)
		}
	}
	if strings.Contains(s, "this turn") {
		t.Errorf("a single fold is not thrashing: %s", s)
	}
	// The second fold in ONE turn is the signal that the budget is wrong.
	n.Turn, n.SinceLastSecs = 2, 7
	s = n.String()
	if !strings.Contains(s, "2× this turn") || !strings.Contains(s, "7s since") {
		t.Errorf("thrash markers missing: %s", s)
	}
}

// The harness must hand every fold to the UI, with the per-turn count attached.
func TestNoteCompaction_FiresTheHook(t *testing.T) {
	var got []CompactionNote
	h := newNoteHarness(t)
	h.cfg.OnCompaction = func(n CompactionNote) { got = append(got, n) }

	h.noteCompaction(agentCompaction(5, 100, 40))
	h.noteCompaction(agentCompaction(3, 90, 30))
	if len(got) != 2 {
		t.Fatalf("want 2 notes, got %d", len(got))
	}
	if got[0].Turn != 1 || got[1].Turn != 2 {
		t.Errorf("per-turn counter not attached: %d then %d", got[0].Turn, got[1].Turn)
	}
	if got[0].Subsumed != 5 || got[1].TokensAfter != 30 {
		t.Errorf("numbers not carried through: %+v", got)
	}
}

func agentCompaction(subsumed, before, after int) agent.CompactionInfo {
	return agent.CompactionInfo{SubsumedCount: subsumed, TokensBefore: before, TokensAfter: after, Summary: "s"}
}
