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

func TestParseSuggestions_FillerFilter(t *testing.T) {
	// Filler phrases are dropped.
	for _, phrase := range []string{"looks good", "thanks", "ok", "done", "great", "awesome"} {
		s := `{"suggestions":[{"text":"` + phrase + `","prob":0.9}]}`
		if got := parseSuggestions(s); len(got) != 0 {
			t.Errorf("filler %q should be dropped, got %+v", phrase, got)
		}
	}

	// Case-insensitive.
	for _, phrase := range []string{"Looks Good", "THANKS", "Ok", "Done"} {
		s := `{"suggestions":[{"text":"` + phrase + `","prob":0.9}]}`
		if got := parseSuggestions(s); len(got) != 0 {
			t.Errorf("filler %q should be dropped (case-insensitive), got %+v", phrase, got)
		}
	}

	// Action-bearing suggestions survive.
	for _, phrase := range []string{"looks good, commit it", "thanks, now run tests", "ok, ship it"} {
		s := `{"suggestions":[{"text":"` + phrase + `","prob":0.5}]}`
		if got := parseSuggestions(s); len(got) != 1 {
			t.Errorf("action phrase %q should survive, got %+v", phrase, got)
		}
	}
}
