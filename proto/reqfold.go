package proto

import (
	"math"
	"sync"
)

// RequestFold is the session's request figures, folded by the engine so
// every client reads them from the request event instead of adding the
// events up (docs/session-events.md, "Request stats"). Before, the TUI and
// the web client each summed the events they saw, so a reconnect started
// the averages again. Safe for concurrent use.
type RequestFold struct {
	mu  sync.Mutex
	s   RequestSession
	pct int
	// Token-weighted sums for the averages: Σ rate·tokens and Σ tokens.
	ppSum, ppN, tgSum, tgN float64
}

// Note folds r in and sets r.Session to the session's figures after it.
func (f *RequestFold) Note(r *Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.CacheReported {
		f.s.CacheSeen = true
	}
	if r.TTFTMS > 0 {
		f.s.TTFTMS = r.TTFTMS
	}
	if r.PPS > 0 {
		f.s.PPS = r.PPS
	}
	if r.TGS > 0 {
		f.s.TGS = r.TGS
	}
	if r.Phase == "done" {
		if r.CacheReported && r.Prompt > 0 {
			f.pct = int(math.Round(float64(r.Cached) * 100 / float64(r.Prompt)))
			f.s.CachePct = &f.pct
		}
		if n := float64(r.Prompt - r.Cached); r.PPS > 0 && n > 0 {
			f.ppSum, f.ppN = f.ppSum+r.PPS*n, f.ppN+n
			f.s.PPAvg = f.ppSum / f.ppN
		}
		if n := float64(r.Gen); r.TGS > 0 && n > 0 {
			f.tgSum, f.tgN = f.tgSum+r.TGS*n, f.tgN+n
			f.s.TGAvg = f.tgSum / f.tgN
		}
	}
	out := f.s
	if out.CachePct != nil {
		p := *out.CachePct // each event its own copy
		out.CachePct = &p
	}
	r.Session = &out
}

// RequestFoldState is a fold's whole state: the session's figures and the
// token-weighted sums behind its averages. An engine keeps it beside the
// session (cmd/dun/tally.go) and seeds the next engine's fold with it, so an
// engine restart or a model switch does not start the figures again.
type RequestFoldState struct {
	Session RequestSession `json:"session"`
	PPSum   float64        `json:"pp_sum,omitempty"`
	PPN     float64        `json:"pp_n,omitempty"`
	TGSum   float64        `json:"tg_sum,omitempty"`
	TGN     float64        `json:"tg_n,omitempty"`
}

// State is f's state now.
func (f *RequestFold) State() RequestFoldState {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := RequestFoldState{Session: f.s, PPSum: f.ppSum, PPN: f.ppN, TGSum: f.tgSum, TGN: f.tgN}
	if f.s.CachePct != nil {
		p := *f.s.CachePct
		st.Session.CachePct = &p
	}
	return st
}

// Seed sets f to st, before its first request.
func (f *RequestFold) Seed(st RequestFoldState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.s = st.Session
	f.ppSum, f.ppN, f.tgSum, f.tgN = st.PPSum, st.PPN, st.TGSum, st.TGN
	f.s.CachePct = nil
	if st.Session.CachePct != nil {
		f.pct = *st.Session.CachePct
		f.s.CachePct = &f.pct
	}
}
