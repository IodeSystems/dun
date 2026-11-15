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
