package dun

// The meter is the foundation the rest of the context work stands on: every
// budget below is denominated in its ratio, so a meter that quietly falls back
// to chars/4 puts the shape target above the window again and nothing else
// fires. These tests hold it to the two properties that matter — it uses the
// provider's number when there is one, and it refuses a number that cannot be
// the provider's.

import (
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/agent"
)

// Until the provider has answered, the meter behaves exactly as agentkit's
// default did. A new estimator that starts somewhere else changes every existing
// session's shaping on upgrade for no measured reason.
func TestMeter_FallsBackToCharsByFour(t *testing.T) {
	var m promptMeter
	if m.Measured() {
		t.Error("a fresh meter must not claim to have measured anything")
	}
	if got, want := m.Estimate(strings.Repeat("x", 400)), 100; got != want {
		t.Errorf("uncalibrated estimate = %d, want %d (chars/4)", got, want)
	}
	if got := m.Estimate(""); got != 0 {
		t.Errorf("empty string = %d, want 0", got)
	}
}

// One round is enough. The provider counted 21,636 tokens for 60,000 chars of a
// real coding session (2.77 chars/token, measured against llama.cpp /tokenize),
// and chars/4 called the same text 15,000 — the 31% error that let a 188k window
// fill without anything noticing.
func TestMeter_CalibratesFromTheProvider(t *testing.T) {
	var m promptMeter
	m.noteBuild(60000, 0)
	obs, ok := m.noteUsage(agent.TokenUsage{Processed: 21636})
	if !ok {
		t.Fatalf("a plain report should calibrate; got %+v", obs)
	}
	if !obs.First {
		t.Error("the first observation must be marked as the calibration round")
	}
	if got := m.CharsPerToken(); got < 2.76 || got > 2.78 {
		t.Errorf("chars/token = %.3f, want ~2.77", got)
	}
	if !m.Measured() {
		t.Error("Measured must be true once the provider has answered")
	}
	// The number the shaper now works in, versus the one it used to.
	if got, want := m.Estimate(strings.Repeat("x", 60000)), 21636; got < want-2 || got > want+2 {
		t.Errorf("calibrated estimate = %d, want ~%d", got, want)
	}
	if obs.Estimated != 15000 {
		t.Errorf("the round should report what chars/4 HAD predicted: got %d, want 15000", obs.Estimated)
	}
	if d := obs.Drift(); d > -0.30 || d < -0.32 {
		t.Errorf("drift = %.3f, want ~-0.31 (the estimate was 31%% low)", d)
	}
}

// Usage arrives as running totals, so a round's prompt is a DIFFERENCE. Reading
// the cumulative number as if it were this round's would make the ratio grow
// without bound: round 5 of a stable conversation would look five times denser
// than round 1 and the budget would collapse.
func TestMeter_DifferencesCumulativeUsage(t *testing.T) {
	var m promptMeter
	m.noteBuild(40000, 0)
	if _, ok := m.noteUsage(agent.TokenUsage{Cached: 5000, Processed: 5000}); !ok {
		t.Fatal("round 1 should calibrate")
	}
	if got := m.CharsPerToken(); got < 3.99 || got > 4.01 {
		t.Fatalf("round 1 chars/token = %.3f, want 4.0", got)
	}
	// Round 2: the prompt grew to 44k chars, so the running total is 21k — of
	// which 11k is this round.
	m.noteBuild(44000, 0)
	if _, ok := m.noteUsage(agent.TokenUsage{Cached: 15000, Processed: 6000}); !ok {
		t.Fatal("round 2 should calibrate")
	}
	if got := m.CharsPerToken(); got < 3.99 || got > 4.01 {
		t.Errorf("round 2 chars/token = %.3f — cumulative usage was read as one round's", got)
	}
}

// A ratio outside the plausible band means the two sides are not describing the
// same request — a proxy that rewrote the body, a cached prefix billed oddly.
// Shaping the context to that is worse than shaping it to the last good number.
func TestMeter_RefusesAnImplausibleRatio(t *testing.T) {
	var m promptMeter
	m.noteBuild(40000, 0)
	m.noteUsage(agent.TokenUsage{Processed: 10000}) // 4.0, good
	// Round 2 bills 10 tokens for the same 40,000 chars: 4000 chars/token, far
	// outside the ceiling.
	m.noteBuild(40000, 0)
	obs, ok := m.noteUsage(agent.TokenUsage{Processed: 10010})
	if ok {
		t.Errorf("ratio %.1f should have been refused", obs.Ratio)
	}
	if got := m.CharsPerToken(); got < 3.0 || got > 5.0 {
		t.Errorf("a refused round must leave the previous ratio in force; got %.2f", got)
	}
}

// A provider that reports no usage teaches nothing, and must not be mistaken for
// one reporting zero tokens.
func TestMeter_IgnoresASilentProvider(t *testing.T) {
	var m promptMeter
	m.noteBuild(40000, 0)
	if _, ok := m.noteUsage(agent.TokenUsage{}); ok {
		t.Error("no reported prompt tokens is not a measurement")
	}
	if m.Measured() {
		t.Error("silence must not calibrate")
	}
}

// liveRounds are six consecutive rounds measured against local-Qwen3.8-27B on
// 2026-08-24 — the run that showed a single ratio cannot work here. Chars are
// what dun built; tokens are what the provider charged, which also covers the
// tool schemas (a separate request field, in no message) and the chat
// template's scaffolding.
var liveRounds = [][2]int{
	{4208, 3340}, {4506, 3536}, {5075, 3972},
	{9258, 5577}, {5012, 3816}, {8152, 5221},
}

// feed replays rounds of (chars, tokens). noteBuild's second argument is what
// the build was ESTIMATED at, which is carried for /context to display and plays
// no part in the ratio — these tests are about the ratio, so it is left at 0.
func feed(m *promptMeter, rounds [][2]int) {
	cumulative := 0
	for _, r := range rounds {
		m.noteBuild(r[0], 0)
		cumulative += r[1]
		m.noteUsage(agent.TokenUsage{Processed: cumulative})
	}
}

// The defect the live run exposed: a one-point ratio tracks the amortisation of
// a CONSTANT, not the cost of text. Over these six rounds it swung 1.26 → 1.66,
// and the ratio in force at the end predicts 193,079 tokens for a real session's
// 301,203 characters — over a 188,160-token window, on every turn, forever.
// Fitted as two terms it predicts 132,699, and the marginal ratio lands next to
// an independent /tokenize measurement of the same corpus (2.77).
func TestMeter_SeparatesPerRequestOverheadFromText(t *testing.T) {
	var m promptMeter
	feed(&m, liveRounds)

	if got := m.Overhead(); got < 1400 || got > 1900 {
		t.Errorf("overhead = %d tokens, want ~1617 (the tool schemas and the template)", got)
	}
	if got := m.CharsPerToken(); got < 2.1 || got > 2.5 {
		t.Errorf("marginal = %.2f chars/token, want ~2.30", got)
	}

	// The number that decides whether the ladder ever runs.
	const yscrChars = 301203
	whole := m.Estimate(strings.Repeat("x", yscrChars)) + m.Overhead()
	if whole < 125000 || whole > 140000 {
		t.Errorf("a %d-char prompt estimates at %d tokens, want ~132,700", yscrChars, whole)
	}
	if whole > 188160 {
		t.Errorf("estimate %d exceeds the measured window — this is the shape that "+
			"compacts on every turn and never converges", whole)
	}
}

// The constant is charged ONCE per request, so it must not be inside Estimate:
// the Shaper calls that per message and sums, and a 400-message prompt would pay
// the tool schemas four hundred times.
func TestMeter_OverheadIsNotChargedPerMessage(t *testing.T) {
	var m promptMeter
	feed(&m, liveRounds)

	whole := m.Estimate(strings.Repeat("x", 30000))
	split := 0
	for i := 0; i < 10; i++ {
		split += m.Estimate(strings.Repeat("x", 3000))
	}
	if diff := split - whole; diff < -10 || diff > 10 {
		t.Errorf("10 messages estimate %d, one of the same size estimates %d — the "+
			"per-request constant leaked into the per-message cost", split, whole)
	}
}

// Until the fit has enough points spread over enough sizes, the one-point ratio
// stays in charge and no overhead is claimed. A fitted constant nobody measured
// shrinks the context for no reason.
func TestMeter_WithholdsAnUnearnedFit(t *testing.T) {
	var m promptMeter
	feed(&m, liveRounds[:2])
	if got := m.Overhead(); got != 0 {
		t.Errorf("overhead = %d after 2 rounds; want 0 until the fit is earned", got)
	}
	// Points all the same size have no slope to recover: the fit would put
	// everything in the intercept.
	var flat promptMeter
	feed(&flat, [][2]int{{5000, 4000}, {5000, 4000}, {5000, 4000}, {5000, 4000}})
	if got := flat.Overhead(); got != 0 {
		t.Errorf("overhead = %d from collinear points; want 0", got)
	}
	if got := flat.CharsPerToken(); got < 1.2 || got > 1.3 {
		t.Errorf("the one-point ratio should still be in force; got %.2f", got)
	}
}

// Fitted and Drift are the two accessors on observation that report whether
// the affine fit is in force and how far off the estimate was.

func TestObservation_Fitted(t *testing.T) {
	if (observation{Slope: 0}).Fitted() {
		t.Error("slope 0 should not be fitted")
	}
	if (observation{Slope: 0.001}).Fitted() != true {
		t.Error("positive slope should be fitted")
	}
}

func TestObservation_Drift(t *testing.T) {
	// No count: no drift.
	if got := (observation{PromptTokens: 0}).Drift(); got != 0 {
		t.Errorf("no count: got %f, want 0", got)
	}
	// Perfect estimate: zero drift.
	if got := (observation{PromptTokens: 1000, Estimated: 1000}).Drift(); got != 0 {
		t.Errorf("perfect: got %f, want 0", got)
	}
	// Estimate was 30% low: drift = (700-1000)/1000 = -0.30
	if got := (observation{PromptTokens: 1000, Estimated: 700}).Drift(); got != -0.30 {
		t.Errorf("30%% low: got %f, want -0.30", got)
	}
	// Estimate was 50% high: drift = (1500-1000)/1000 = +0.50
	if got := (observation{PromptTokens: 1000, Estimated: 1500}).Drift(); got != 0.50 {
		t.Errorf("50%% high: got %f, want 0.50", got)
	}
}
