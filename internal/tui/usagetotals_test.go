package tui

import "testing"

// usage carries the session's totals, cumulative: the TUI sets them, so two
// turns read as the second's figures, and a reconnect's snapshot (only the
// last usage) reads the same.
func TestUsage_TotalsAreTheLast(t *testing.T) {
	first := evMsg{"type": "usage", "total": 5000.0, "cached": 3000.0, "processed": 1500.0, "generated": 500.0, "turns": 1.0, "active": 4000.0}
	second := evMsg{"type": "usage", "total": 12000.0, "cached": 8000.0, "processed": 3000.0, "generated": 1000.0, "turns": 3.0, "active": 9000.0}
	m := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	m = m.feed(first)
	m = m.feed(second)
	again := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	again = again.feed(second)
	for _, s := range []contextStats{m.ctxStats, again.ctxStats} {
		if s.totalTokens != 12000 || s.cachedTokens != 8000 || s.processedTokens != 3000 || s.generatedTokens != 1000 || s.turns != 3 {
			t.Errorf("totals %d/%d/%d/%d turns %d, want the last usage's", s.totalTokens, s.cachedTokens, s.processedTokens, s.generatedTokens, s.turns)
		}
	}
}
