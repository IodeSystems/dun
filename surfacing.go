package dun

import (
	"math"
	"sync"
	"time"
)

// Surfacing accounting — what proactive RAG costs the conversation.
//
// A surfaced entry is not a one-time cost. docsPreparer APPENDS it to history,
// so every chat round after that re-sends it until compaction drops it. A note
// found on round 2 has been paid for many times by round 20, and nothing showed
// that: DocsNote carried counts (found/surfaced) and no size, and /context did
// not break the docs entries out of the conversation.
//
// So there are two numbers, and the second is the one that buries a session:
//
//   - the entry itself, once: bytes and tokens of what was appended, per pass;
//   - the carried cost: on every round, the tokens of the docs entries still
//     in history, summed across rounds — what surfacing has actually cost in
//     prompt tokens so far.
//
// Latency is counted per PASS, including passes that found nothing: a search
// that returns no hits still holds the turn for its round trip.
//
// Tokens are the calibrated meter's estimate (promptMeter.Estimate), not a
// provider count — the entry is one part of a prompt the provider only ever
// counts as a whole. Appended tokens are re-derived from bytes at the CURRENT
// ratio when reported, so "appended" and "in history" are in the same unit:
// measured live, the meter moved from 4 to 1.02 chars/token on the first
// round, and an entry stamped at append read 112 tok beside 438 for itself.
// The early ratio is itself unsettled (it has not yet split the tool-schema
// overhead from the per-char slope — see promptMeter.Overhead), so first-round
// token numbers run high; bytes are exact.

type surfacingStats struct {
	mu sync.Mutex

	Passes   int   // preparer searches run
	Found    int   // candidate hits after threshold/dedup, summed
	Surfaced int   // hits actually appended, summed
	Entries  int   // docs entries appended (one per pass that surfaced)
	Bytes    int   // bytes appended, summed
	Tokens   int   // estimated tokens appended, summed
	LatMS    int64 // finder round-trip, summed over passes
	LastMS   int64 // the most recent pass's round-trip
	Rounds   int   // chat rounds observed by noteRound
	Carried  int   // tokens of in-history docs entries, summed over rounds
	InHist   int   // tokens of docs entries in history at the last round
	InHistN  int   // docs entries in history at the last round
	LastSize int   // tokens of the last entry appended
}

// notePass records one preparer search. bytes/tokens are zero when the pass
// surfaced nothing.
func (s *surfacingStats) notePass(lat time.Duration, found, surfaced, bytes, tokens int) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ms := lat.Milliseconds()
	s.Passes++
	s.LatMS += ms
	s.LastMS = ms
	s.Found += found
	if surfaced > 0 {
		s.Surfaced += surfaced
		s.Entries++
		s.Bytes += bytes
		s.Tokens += tokens
		s.LastSize = tokens
	}
}

// noteRound records what one chat round re-sent: n docs entries in history,
// totalling tokens.
func (s *surfacingStats) noteRound(n, tokens int) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Rounds++
	s.Carried += tokens
	s.InHist = tokens
	s.InHistN = n
}

// summary is the JSON shape for the usage event; nil before anything ran, so
// a session without RAG carries no empty section.
func (s *surfacingStats) summary(charsPerToken float64) map[string]any {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Passes == 0 && s.Rounds == 0 {
		return nil
	}
	m := map[string]any{
		"passes": s.Passes, "found": s.Found, "surfaced": s.Surfaced,
		"entries": s.Entries, "bytes": s.Bytes, "tokens": s.Tokens,
		"latency_ms": s.LatMS, "last_ms": s.LastMS,
		"rounds": s.Rounds, "carried": s.Carried,
		"in_history": s.InHist, "in_history_entries": s.InHistN,
	}
	if charsPerToken > 0 {
		m["tokens"] = int(math.Ceil(float64(s.Bytes) / charsPerToken))
	}
	if s.Passes > 0 {
		m["avg_ms"] = s.LatMS / int64(s.Passes)
	}
	return m
}

// noteSurfacingRound measures the docs entries in history for this round.
// Called from OnUsage, which fires once per chat round — the unit at which a
// history entry is re-sent.
func (h *Harness) noteSurfacingRound() {
	if h.surfacing == nil || h.store == nil {
		return
	}
	h.surfacing.mu.Lock()
	active := h.surfacing.Passes > 0
	h.surfacing.mu.Unlock()
	if !active {
		return
	}
	n, tok := 0, 0
	for _, e := range h.store.docsEntries() {
		n++
		tok += h.meter.Estimate(e)
	}
	h.surfacing.noteRound(n, tok)
}

// Surfacing exposes the totals, JSON-serializable for the usage event.
func (h *Harness) Surfacing() map[string]any {
	return h.surfacing.summary(h.meter.CharsPerToken())
}
