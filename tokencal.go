package dun

import (
	"log"
	"math"
	"sync"

	"github.com/iodesystems/agentkit/agent"
)

// tokencal.go — measuring chars-per-token instead of guessing it.
//
// Every token number dun produced used to be agentkit's CharsByFour: bytes/4.
// That is the Shaper's budget arithmetic, the `Active` window shown in the UI,
// and the threshold the recap nudge fires on — one guess, three consumers.
//
// Measured against this session's own bytes (llama.cpp /tokenize,
// local-Qwen3.8-27B, a yscr coding session): 60,000 chars → 21,636 tokens, i.e.
// 2.77 chars per token, not 4. So a prompt logged as "301,203 chars (~75,300
// tokens)" was really ~108,700 — the estimate was 31% low.
//
// The consequence is not a cosmetic error in a status line, it is that the
// context ladder never runs. With a 188,160-token window the budget came out at
// 169,344 "tokens", and the shape target at 159,344 — which at 2.77 chars/token
// is ~230,000 REAL tokens, more than the whole window. LOD, compaction and the
// rescue fold cannot fire before the endpoint does, so the first thing that
// notices the wall is the model, mid-generation, with finish_reason=length.
//
// The fix does not need a tokenizer round-trip. The provider reports
// prompt_tokens on every single round and we already know how many chars we
// sent it, so the ratio is a division. This is that division, remembered.

// fallbackCharsPerToken is what to assume before the provider has answered
// once. Kept at agentkit's 4 deliberately: it is the number every previous
// session ran on, so an uncalibrated dun behaves exactly as it did before rather
// than jumping to some new guess that is differently wrong.
const fallbackCharsPerToken = 4.0

// ratioFloor and ratioCeil bound what a measurement is allowed to claim.
//
// Not paranoia about arithmetic — a bound on a number derived from TWO sources
// that can disagree. The chars are ours (the built message list); the tokens are
// the provider's, and a provider that reports prompt_tokens for a cached prefix
// it did not re-evaluate, or a proxy that rewrites the body, makes the division
// meaningless. 1.0 is the densest real text measured (one token per character);
// 8.0 is looser than any natural-language corpus. Outside that, keep the last
// good ratio rather than shape the context to a typo.
const (
	ratioFloor = 1.0
	ratioCeil  = 8.0
)

// promptMeter is an agent.TokenEstimator fitted to the provider's own usage
// reports.
//
// The model is AFFINE, not a single ratio, and that is a correction the first
// live run forced. A round's prompt_tokens covers things our character count
// does not: the tool schemas (sent as a separate field, so not in any message),
// the chat template's per-message scaffolding, a BOS. Those are a fixed cost per
// REQUEST, and dividing them into the character count spreads a constant across
// a variable.
//
// Measured on six consecutive rounds against local-Qwen3.8-27B, a single ratio
// swung 1.26 → 1.66 → 1.31 → 1.56 chars/token as the prompt grew, purely from
// that constant being amortised over more text. Extrapolated to a real coding
// session's 301,203 characters, the ratio in force at the end of that swing
// predicts 193,079 tokens against a 188,160-token window — over budget, on every
// turn, forever. The failure mode is documented in contextBudget: two sessions
// that folded 45 times in 29 minutes and 38 in 7.
//
// Fitted as two terms instead, the same six rounds give 2.30 chars/token
// marginal plus 1,617 tokens of overhead, and predict 132,699 for that prompt.
// The marginal figure is also the one that agrees with an independent
// measurement: 2.77 chars/token counted by llama.cpp's /tokenize over the same
// session's raw content, which carries no per-request cost at all.
//
// So: Estimate reports the MARGINAL cost of text, which is what the Shaper is
// deciding about when it decides what to keep. The fixed term is published
// separately (Overhead) and charged once, against the budget.
type promptMeter struct {
	mu sync.RWMutex
	// ratio is chars per token from the LAST observation alone — the one-point
	// model, still used until the fit has enough spread to beat it.
	ratio float64
	// slope (tokens per char) and intercept (tokens per request) are the fit.
	// slope is 0 until it is trustworthy; see fitLocked for what that means.
	slope     float64
	intercept float64
	// Accumulators for the least-squares fit over (chars, tokens).
	n, sx, sy, sxx, sxy float64
	// built is the size of the most recently BUILT prompt, in chars. Set by
	// noteBuild immediately before the round it belongs to.
	built int
	// prompted is the cumulative prompt-token count at the last observation, so
	// a per-round figure can be differenced out of agent.TokenUsage's running
	// totals.
	prompted int
	obs      int
}

// minFitPoints is how many rounds the fit needs before it is preferred to the
// one-point ratio. Two determine a line and three are the first that can
// disagree with them, which is the cheapest evidence that the points are not
// collinear noise.
const minFitPoints = 3

// minFitSpread is the character range the observations must cover. Fitting a
// line through points that are all the same size recovers the noise, not the
// slope: the denominator goes to zero and the intercept absorbs everything.
const minFitSpread = 2000.0

// maxOverhead caps a believable fixed cost. Tool schemas and a chat template
// are thousands of tokens, not tens of thousands; a fit claiming more has
// found something other than a per-request constant.
const maxOverhead = 20000.0

// Estimate implements agent.TokenEstimator: the MARGINAL cost of this text.
//
// Deliberately not the whole cost of a request containing it. The interface is
// called per message and summed, so a per-request constant added here would be
// charged once per message — on a 400-message prompt, four hundred times. See
// Overhead for where the constant is charged instead.
func (m *promptMeter) Estimate(s string) int {
	if len(s) == 0 {
		return 0
	}
	return int(math.Ceil(float64(len(s)) / m.CharsPerToken()))
}

// CharsPerToken is the marginal ratio in force: the fit's slope once it is
// trustworthy, the last single observation before that, and 4 before anything
// has been measured at all.
func (m *promptMeter) CharsPerToken() float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.slope > 0 {
		return 1 / m.slope
	}
	if m.ratio <= 0 {
		return fallbackCharsPerToken
	}
	return m.ratio
}

// Overhead is the fitted per-request cost in tokens: everything the provider
// counts that our characters do not — the tool schemas, which travel in their
// own field and appear in no message, plus the chat template's scaffolding.
//
// 0 until the fit is trustworthy, which is the honest answer rather than a
// conservative one: a guessed overhead subtracted from the budget shrinks the
// context for a reason nobody measured.
func (m *promptMeter) Overhead() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.slope <= 0 || m.intercept <= 0 {
		return 0
	}
	return int(m.intercept)
}

// fitLocked recomputes the least-squares line through the observations and
// accepts it only if it is believable: enough points, spread over enough
// character range to have a slope at all, a marginal ratio inside the same band
// a single observation must satisfy, and a non-negative overhead under the cap.
//
// A rejected fit is not an error. It leaves the one-point ratio in charge, which
// is what was in charge before the fit existed.
func (m *promptMeter) fitLocked() {
	if m.n < minFitPoints {
		return
	}
	den := m.n*m.sxx - m.sx*m.sx
	if den <= 0 || math.Sqrt(m.sxx/m.n-(m.sx/m.n)*(m.sx/m.n)) < minFitSpread/2 {
		return
	}
	slope := (m.n*m.sxy - m.sx*m.sy) / den
	intercept := (m.sy - slope*m.sx) / m.n
	if slope <= 0 {
		return
	}
	if cpt := 1 / slope; cpt < ratioFloor || cpt > ratioCeil {
		return
	}
	if intercept < 0 || intercept > maxOverhead {
		return
	}
	m.slope, m.intercept = slope, intercept
}

// Measured reports whether the ratio came from the provider rather than from the
// fallback. Callers that PUBLISH a token number use this to say which it is —
// the same discipline SystemBreakdown.Exact enforces for /context.
func (m *promptMeter) Measured() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.ratio > 0
}

// noteBuild records the size of the prompt just built. Called from the context
// builder, which runs immediately before the chat round whose usage will price
// it — that adjacency is what lets the two numbers be divided at all.
func (m *promptMeter) noteBuild(chars int) {
	m.mu.Lock()
	m.built = chars
	m.mu.Unlock()
}

// observation is one round's worth of the comparison, for logging.
type observation struct {
	Chars        int
	PromptTokens int     // what the provider counted for those chars
	Estimated    int     // what the ratio in force at BUILD time predicted
	Ratio        float64 // this round's chars-per-token, on its own
	Slope        float64 // the fit's tokens-per-char, 0 while it is not trusted
	Overhead     float64 // the fit's per-request constant, in tokens
	First        bool    // the calibration round: nothing measured before it
}

// Fitted reports whether the affine fit is in force rather than the one-point
// ratio.
func (o observation) Fitted() bool { return o.Slope > 0 }

// Drift reports how far the prediction was from the count, as a signed fraction
// (-0.31 = the estimate was 31% low). Zero when there is nothing to compare.
func (o observation) Drift() float64 {
	if o.PromptTokens <= 0 {
		return 0
	}
	return float64(o.Estimated-o.PromptTokens) / float64(o.PromptTokens)
}

// noteUsage folds a round's reported usage into the ratio and returns what the
// round showed. ok is false when there is nothing to learn from — no build to
// pair with, or a provider that reports no prompt tokens.
//
// The per-round prompt count is DIFFERENCED out of agent.TokenUsage's running
// totals: Cached + Processed is the whole prompt (a provider with no cache
// breakdown puts all of it in Processed), and both accumulate across the
// session. Subtracting the previous observation is what turns them back into
// this round's number.
func (m *promptMeter) noteUsage(u agent.TokenUsage) (observation, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	cumulative := u.Cached + u.Processed
	round := cumulative - m.prompted
	m.prompted = cumulative
	if round <= 0 || m.built <= 0 {
		return observation{}, false
	}

	prior := m.ratio
	if prior <= 0 {
		prior = fallbackCharsPerToken
	}
	obs := observation{
		Chars:        m.built,
		PromptTokens: round,
		Estimated:    int(math.Ceil(float64(m.built) / prior)),
		Ratio:        float64(m.built) / float64(round),
		First:        m.ratio <= 0,
	}
	if obs.Ratio < ratioFloor || obs.Ratio > ratioCeil {
		// Keep the last good ratio, and keep the point OUT of the fit: a round
		// the two sides disagree about is not evidence of anything. See
		// ratioFloor.
		return obs, false
	}
	m.ratio = obs.Ratio
	m.obs++
	x, y := float64(m.built), float64(round)
	m.n++
	m.sx += x
	m.sy += y
	m.sxx += x * x
	m.sxy += x * y
	m.fitLocked()
	obs.Slope, obs.Overhead = m.slope, m.intercept
	return obs, true
}

// lastBuiltChars is the size of the prompt most recently built, for a caller
// sizing the room left in the window against it.
func (m *promptMeter) lastBuiltChars() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.built
}

// logObservation says, once per round, what the estimate claimed and what the
// provider counted.
//
// One line, on every round, because the failure this exists to catch is silent
// by construction: an estimate that is 31% low produces no error, no warning and
// no wrong-looking output — it produces a context ladder that never runs and a
// generation that dies at the wall twenty minutes later. The only way that shows
// up in a log is if the log prints both numbers side by side.
func logObservation(o observation, ok bool) {
	switch {
	case !ok && o.PromptTokens == 0:
		return // nothing reported; not worth a line every round
	case !ok:
		log.Printf("dun: prompt %d chars → provider counted %d tokens (%.2f chars/token) — "+
			"OUTSIDE [%.1f, %.1f], keeping the previous ratio; the two sides are not measuring "+
			"the same request", o.Chars, o.PromptTokens, o.Ratio, ratioFloor, ratioCeil)
	case o.First:
		log.Printf("dun: calibrated: prompt %d chars → %d tokens (%.2f chars/token); the %.0f "+
			"chars/token default had estimated %d (%+.0f%%). Shaping now uses the measured ratio.",
			o.Chars, o.PromptTokens, o.Ratio, fallbackCharsPerToken, o.Estimated, o.Drift()*100)
	case o.Fitted():
		log.Printf("dun: prompt %d chars → %d tokens; estimate %d (%+.0f%%); fit: %.2f chars/token "+
			"marginal + %.0f tokens per request",
			o.Chars, o.PromptTokens, o.Estimated, o.Drift()*100, 1/o.Slope, o.Overhead)
	default:
		log.Printf("dun: prompt %d chars → %d tokens (%.2f chars/token); estimate %d (%+.0f%%)",
			o.Chars, o.PromptTokens, o.Ratio, o.Estimated, o.Drift()*100)
	}
}
