package dun

import "testing"

func TestParseSuggestions(t *testing.T) {
	// Wrapped in prose (small models do this), unsorted, one empty (dropped).
	s := `sure: {"suggestions":[{"text":"run the tests","prob":0.2},{"text":"  ","prob":0.9},{"text":"commit it","prob":0.6}]} ok`
	got := parseSuggestions(s)
	if len(got) != 2 {
		t.Fatalf("want 2 (empty dropped), got %d: %+v", len(got), got)
	}
	if got[0].Text != "commit it" { // 0.6 sorts above 0.2
		t.Fatalf("should sort by prob desc, got %+v", got)
	}

	// Probabilities clamp to [0,1].
	if c := parseSuggestions(`{"suggestions":[{"text":"x","prob":1.5}]}`); len(c) != 1 || c[0].Prob != 1 {
		t.Fatalf("clamp failed: %+v", c)
	}

	// No JSON → nil.
	if parseSuggestions("no json here") != nil {
		t.Fatal("non-JSON should yield nil")
	}
}
