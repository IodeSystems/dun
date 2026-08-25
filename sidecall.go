package dun

import (
	"sync"
	"time"

	"github.com/iodesystems/agentkit/llm"
)

// Side-call accounting — the LLM calls that are NOT the conversation turn.
//
// A session has more than one reader of the model: the turn itself, plus the
// helpers that fire around it — /suggest predictions, /rephrase rewrites,
// commit messages, rescue compactions. Each is one throwaway round-trip, but
// a session that idles with suggestions on, or rephrases every message, or
// compacts its way down a long conversation, spends real money and real
// seconds on calls that no view used to see: the turn's TokenUsage counts the
// turn, and the side calls threw their usage off the stream's final chunk
// without reading it.
//
// This is that reading, in one place. Every side-call site (commit.go,
// suggest.go, rephrase.go, rescue.go) calls noteSideCall with the wall clock
// and whatever Usage the final chunk carried (nil when the provider did not
// report, or the call failed before one). The numbers are per-session
// running totals, per KIND: counts, latency sums (for averages), and the
// token columns a turn gets — processed, cached, generated. They ride the
// existing `usage` event, so /context shows them alongside the turn's.
//
// Latency: the chunk's Usage.LatencyMS is the client-measured round-trip the
// agentkit client stamps on the same call; the wall clock here is the same
// span (start just before ChatStream, stop just after the Done chunk), so the
// two agree and the wall clock is the fallback for providers that do not
// report usage at all.

// sideCallKind names a helper call. Stable strings — they are the keys in the
// stats map and show up verbatim in /context.
type sideCallKind string

// Per-call record, one row per kind after aggregation.
type sideCallRecord struct {
	Calls         int
	LatencyMS     int64
	Processed     int   // prompt tokens re-evaluated
	Cached        int   // prompt tokens served from cache
	Generated     int   // completion tokens
	LastLatencyMS int64 // the most recent call, for the "last" display
}

type sideCallStats struct {
	mu    sync.Mutex
	kinds map[sideCallKind]*sideCallRecord
}

// noteSideCall folds one finished side call into the per-kind totals.
// u may be nil (failed call, or a provider with no usage report) — the call
// and its latency are still counted, so a session full of FAILED suggestions
// shows up as such instead of as nothing.
func (h *Harness) noteSideCall(kind string, start time.Time, u *llm.Usage) {
	if h.sideCalls == nil {
		return
	}
	lat := time.Since(start).Milliseconds()
	proc, cached, gen := 0, 0, 0
	if u != nil {
		proc = u.NewPromptTokens()
		cached = u.CachedPromptTokens()
		gen = u.CompletionTokens
		if u.LatencyMS > 0 {
			lat = u.LatencyMS // the client's own measurement of this round-trip
		}
	}
	h.sideCalls.record(sideCallKind(kind), lat, proc, cached, gen)
}

func (s *sideCallStats) record(kind sideCallKind, latMS int64, proc, cached, gen int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.kinds == nil {
		s.kinds = make(map[sideCallKind]*sideCallRecord)
	}
	r := s.kinds[kind]
	if r == nil {
		r = &sideCallRecord{}
		s.kinds[kind] = r
	}
	r.Calls++
	r.LatencyMS += latMS
	r.LastLatencyMS = latMS
	r.Processed += proc
	r.Cached += cached
	r.Generated += gen
}

// SideCalls exposes the per-kind totals, JSON-serializable for the usage event.
func (h *Harness) SideCalls() map[string]map[string]any {
	if h.sideCalls == nil {
		return nil
	}
	return h.sideCalls.summary()
}

func (s *sideCallStats) summary() map[string]map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]map[string]any, len(s.kinds))
	for k, r := range s.kinds {
		m := map[string]any{
			"calls":      r.Calls,
			"latency_ms": r.LatencyMS,
			"last_ms":    r.LastLatencyMS,
			"processed":  r.Processed,
			"cached":     r.Cached,
			"generated":  r.Generated,
		}
		if r.Calls > 0 {
			m["avg_ms"] = r.LatencyMS / int64(r.Calls)
		}
		if r.LatencyMS > 0 && r.Generated > 0 {
			m["tok_per_s"] = float64(r.Generated) / (float64(r.LatencyMS) / 1000.0)
		}
		out[string(k)] = m
	}
	return out
}
