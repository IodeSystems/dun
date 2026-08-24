package dun

// Two defects the live run surfaced, both of the same kind this whole area is
// about: a number or a diagnosis that LOOKS measured and is not.

import (
	"context"
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/agent"
)

// Observed live 2026-08-24: two overflow folds in one turn on a 9,824-token
// prompt against a 178,354-token budget, reported as "the context budget cannot
// fit one turn's floor. Raise DUN_CONTEXT_TOKENS." The budget was nowhere near
// full; the folds came from the endpoint cutting replies. Every word of the
// advice was wrong.
func TestFoldCause_OverflowFoldsDoNotIndictTheBudget(t *testing.T) {
	h := newNoteHarness(t)
	var notes []CompactionNote
	h.cfg.OnCompaction = func(n CompactionNote) { notes = append(notes, n) }

	h.noteCompactionCause(agent.CompactionInfo{SubsumedCount: 20}, foldByOverflow)
	h.noteCompactionCause(agent.CompactionInfo{SubsumedCount: 11}, foldByOverflow)

	h.compactMu.Lock()
	turn, overflow := h.compactTurn, h.compactOverflow
	h.compactMu.Unlock()
	if turn != 2 || overflow != 2 {
		t.Fatalf("compactTurn=%d compactOverflow=%d, want 2 and 2", turn, overflow)
	}
	if len(notes) != 2 {
		t.Errorf("the UI should still see both folds, got %d", len(notes))
	}
}

// The Shaper's own folds are still the case the thrash warning was written for,
// and splitting the counter must not have disarmed it.
func TestFoldCause_ShaperFoldsStillCountAsThrash(t *testing.T) {
	h := newNoteHarness(t)
	h.noteCompaction(agent.CompactionInfo{SubsumedCount: 5})
	h.noteCompaction(agent.CompactionInfo{SubsumedCount: 5})

	h.compactMu.Lock()
	turn, overflow := h.compactTurn, h.compactOverflow
	h.compactMu.Unlock()
	if turn != compactionThrashTurns {
		t.Errorf("compactTurn=%d, want %d (the thrash threshold)", turn, compactionThrashTurns)
	}
	if overflow != 0 {
		t.Errorf("compactOverflow=%d, want 0 — a Shaper fold is not an overflow fold", overflow)
	}
}

// Every rescue used to log "0 → 0 tokens (saved 0)": a measurement-shaped number
// that measured nothing, in the one place a reader goes to find out whether the
// fold was worth its LLM call.
func TestFoldHistory_ReportsWhatItActuallySaved(t *testing.T) {
	h := newNoteHarness(t)
	h.client = &summaryRunner{t: t}
	seedHistory(t, h, 8)

	var note CompactionNote
	h.cfg.OnCompaction = func(n CompactionNote) { note = n }

	n, err := h.foldHistory(context.Background(), foldByOverflow)
	if err != nil || n == 0 {
		t.Fatalf("fold: n=%d err=%v", n, err)
	}
	if note.TokensBefore <= 0 {
		t.Errorf("TokensBefore=%d — the folded entries cost SOMETHING", note.TokensBefore)
	}
	if note.TokensAfter <= 0 {
		t.Errorf("TokensAfter=%d — the summary is not free either", note.TokensAfter)
	}
	if note.TokensAfter >= note.TokensBefore {
		t.Errorf("a fold that saves nothing is not worth its LLM call: %d → %d",
			note.TokensBefore, note.TokensAfter)
	}
	if s := note.String(); strings.Contains(s, "saved 0)") {
		t.Errorf("still reporting a zero saving: %s", s)
	}
}
