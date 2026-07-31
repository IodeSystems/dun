package main

import (
	"fmt"
	"log"
	"net/http"
	_ "net/http/pprof" // registers /debug/pprof on the handler below
	"os"
	"sort"
	"sync"
	"time"
)

// Perf instrumentation, added after a session spent so long rendering that it
// dropped keystrokes while tmux stayed responsive — and nothing in dun could
// say so. There were no metrics of any kind: no frame timing, no profile
// endpoint, no counters. The diagnosis took a benchmark written after the fact.
//
// Two levels, because they answer different questions:
//
//   - frameStats: always on, ~free (one time.Since per frame), and readable
//     from inside the session with /perf. Answers "is the UI the problem?"
//   - DUN_PPROF: opt-in pprof on a local port. Answers "which function?"
//
// The always-on half matters more. A profile you have to know to enable is a
// profile nobody has when the thing actually happens.

// slowFrame is the point where a redraw is visible as a stutter: at 30Hz a
// frame has 33ms, and anything past half of that is eating the input loop's
// share of it.
const slowFrame = 16 * time.Millisecond

// frameStats accumulates redraw timings. Cheap enough to leave on: an append
// to a bounded ring and a couple of comparisons.
type frameStats struct {
	mu      sync.Mutex
	n       int
	slow    int
	total   time.Duration
	max     time.Duration
	recent  []time.Duration // ring of the last frames, for percentiles
	nextIdx int
}

const frameRing = 256

var frames frameStats

// observe records one frame.
func (f *frameStats) observe(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	f.total += d
	if d > f.max {
		f.max = d
	}
	if d >= slowFrame {
		f.slow++
	}
	if len(f.recent) < frameRing {
		f.recent = append(f.recent, d)
		return
	}
	f.recent[f.nextIdx] = d
	f.nextIdx = (f.nextIdx + 1) % frameRing
}

// report renders the stats for /perf. Percentiles come from the recent ring —
// the last few hundred frames are what the user is complaining about, not the
// average since startup.
func (f *frameStats) report() string {
	f.mu.Lock()
	n, slow, total, mx := f.n, f.slow, f.total, f.max
	recent := append([]time.Duration(nil), f.recent...)
	f.mu.Unlock()

	if n == 0 {
		return "no frames rendered yet"
	}
	sort.Slice(recent, func(i, j int) bool { return recent[i] < recent[j] })
	pct := func(p int) time.Duration {
		if len(recent) == 0 {
			return 0
		}
		i := len(recent) * p / 100
		if i >= len(recent) {
			i = len(recent) - 1
		}
		return recent[i]
	}
	s := fmt.Sprintf("frames %d · mean %s · p50 %s · p95 %s · max %s",
		n, round(total/time.Duration(n)), round(pct(50)), round(pct(95)), round(mx))
	if slow > 0 {
		s += fmt.Sprintf("\nslow frames (≥%s): %d (%.1f%%) — these are what drops keystrokes",
			slowFrame, slow, float64(slow)*100/float64(n))
	}
	s += fmt.Sprintf("\nredraws are capped at %dHz while streaming; DUN_PPROF=127.0.0.1:6060 for a profile", renderHz)
	return s
}

func round(d time.Duration) time.Duration {
	if d < time.Millisecond {
		return d.Round(10 * time.Microsecond)
	}
	return d.Round(100 * time.Microsecond)
}

// startPprof serves net/http/pprof when DUN_PPROF is set to an address. Opt-in
// and local: it exposes the process's internals, so it must never be on by
// default or bound anywhere but a loopback address the user chose.
func startPprof() {
	addr := os.Getenv("DUN_PPROF")
	if addr == "" {
		return
	}
	go func() {
		log.Printf("dun: pprof on http://%s/debug/pprof/", addr)
		srv := &http.Server{Addr: addr, ReadHeaderTimeout: 5 * time.Second}
		if err := srv.ListenAndServe(); err != nil {
			log.Printf("dun: pprof: %v", err)
		}
	}()
}
