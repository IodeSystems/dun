package main

import (
	"strings"
	"testing"
)

// The block exists because /context could itemise the pre-conversation cost in
// five rows and could not say how big the window was — so a reader could see
// what was expensive without ever seeing whether it was near the wall. These
// tests hold it to the two things that makes it worth having: the numbers are
// there, and a measured one never renders like a default one.

func TestWindowBlock_ReportsTheDivision(t *testing.T) {
	s := &contextStats{
		window: 188160, windowReserved: 34816, windowCap: 32768,
		promptBudget: 151727, promptTokens: 108738, promptOverhead: 1617,
		charsPerToken: 2.30, ratioMeasured: true, ratioRounds: 42,
	}
	out := windowBlock(s)
	for _, want := range []string{"188160", "108738", "151727", "1617", "34816", "2.30 chars/token", "measured over 42 rounds"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// room now = window - prompt
	if !strings.Contains(out, "79422") {
		t.Errorf("room left should be shown as 79422:\n%s", out)
	}
	if strings.Contains(out, "below the reservation") {
		t.Errorf("79422 of room is above the 34816 reservation; no warning is due:\n%s", out)
	}
}

// The state a cut comes out of. A reader should not have to do the subtraction
// that the whole session's work was about.
func TestWindowBlock_WarnsWhenThePromptEatsTheReservation(t *testing.T) {
	s := &contextStats{
		window: 188160, windowReserved: 34816, windowCap: 32768,
		promptBudget: 151727, promptTokens: 170000,
		charsPerToken: 2.30, ratioMeasured: true, ratioRounds: 9,
	}
	if out := windowBlock(s); !strings.Contains(out, "below the reservation") {
		t.Errorf("a prompt inside the reservation must be called out:\n%s", out)
	}
}

// A default and a measurement must never render alike: that equivalence is what
// let a 31%-low estimate pass for a measurement until the endpoint refused to
// generate.
func TestWindowBlock_DistinguishesADefaultFromAMeasurement(t *testing.T) {
	measured := windowBlock(&contextStats{window: 188160, promptBudget: 151727,
		charsPerToken: 2.30, ratioMeasured: true, ratioRounds: 5})
	guessed := windowBlock(&contextStats{window: 188160, promptBudget: 151727,
		charsPerToken: 4.0, ratioMeasured: false})
	if !strings.Contains(measured, "measured over") {
		t.Errorf("a measurement should say so:\n%s", measured)
	}
	if !strings.Contains(guessed, "default") || strings.Contains(guessed, "measured over") {
		t.Errorf("a default must not read as a measurement:\n%s", guessed)
	}
}

// No window is a STATE, not missing data — and it is the state the yscr session
// ran in: nothing to shape against, no cap to send, the model generating until
// the endpoint stops it.
func TestWindowBlock_SaysWhenThereIsNoWindow(t *testing.T) {
	out := windowBlock(&contextStats{})
	for _, want := range []string{"not stated", "no shaping", "DUN_CONTEXT_TOKENS"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// A cut is the budget having been wrong, so it is reported with the budget.
func TestWindowBlock_ReportsCuts(t *testing.T) {
	out := windowBlock(&contextStats{window: 188160, promptBudget: 151727,
		promptTokens: 100000, charsPerToken: 2.3, ratioMeasured: true,
		windowCuts: 3, windowFolds: 1})
	if !strings.Contains(out, "cut short") || !strings.Contains(out, "1 folded") {
		t.Errorf("cuts should be reported here:\n%s", out)
	}
}

// The stats are fed by events; a field that decodes wrong renders a confident
// zero, which is worse than a blank.
func TestReadWindow_DecodesTheEvent(t *testing.T) {
	var s contextStats
	s.readWindow(map[string]any{
		"window": 188160.0, "window_reserved": 34816.0, "window_cap": 32768.0,
		"prompt_budget": 151727.0, "prompt_tokens": 108738.0, "prompt_overhead": 1617.0,
		"chars_per_token": 2.3, "ratio_measured": true, "ratio_rounds": 42.0,
		"window_cuts": 2.0, "window_folds": 1.0,
	})
	if s.window != 188160 || s.promptTokens != 108738 || s.promptOverhead != 1617 ||
		s.windowCap != 32768 || s.ratioRounds != 42 || s.windowCuts != 2 || s.windowFolds != 1 {
		t.Errorf("decoded wrong: %+v", s)
	}
	if !s.ratioMeasured || s.charsPerToken != 2.3 {
		t.Errorf("ratio decoded wrong: %v %v", s.ratioMeasured, s.charsPerToken)
	}
	// An event from a path that does not carry the window must not zero what is
	// already known.
	s.readWindow(map[string]any{"total": 5.0})
	if s.window != 188160 {
		t.Error("an event without the window cleared it")
	}
}
