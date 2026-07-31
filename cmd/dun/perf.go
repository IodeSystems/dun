package main

import (
	"fmt"
	"log"
	"net/http"
	_ "net/http/pprof" // registers /debug/pprof on the handler below
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Perf instrumentation, added after a session spent so long rendering that it
// dropped keystrokes while tmux stayed responsive — and nothing in dun could
// say so. There were no metrics of any kind: no frame timing, no profile
// endpoint, no counters. The diagnosis took a benchmark written after the fact.
//
// Three levels, because they answer different questions:
//
//   - stage timings: always on, ~free, readable in-session with /perf.
//     "Is the UI the problem, and which half of it?"
//   - slow-frame detection: notices on its own and writes a dump, because the
//     user who hits this is busy working, not watching a metric.
//   - DUN_PPROF: opt-in pprof. "Which function?"
//
// The always-on half matters most. A profile you have to know to enable is a
// profile nobody has when the thing actually happens.

// slowFrame is the point where a trip through the loop is visible as a stutter.
// At 30Hz a frame's budget is 33ms; past half of that it is eating the input
// loop's share.
const slowFrame = 16 * time.Millisecond

// stage names the thing being timed. A "frame" is one trip through the Bubble
// Tea loop — Update plus the View the runtime calls after it — because that is
// what a keystroke waits behind. Timing only the redraw hid half the cost: View
// runs per message even when nothing was redrawn (191µs of it inside bubbles'
// viewport, until that was memoized).
type stage int

const (
	stageUpdate stage = iota
	stageView
	stageRefresh
	numStages
)

var stageName = [numStages]string{"update", "view", "refresh"}

const frameRing = 256

// stageStats is one stage's timings: totals plus a bounded ring for
// percentiles, because the last few hundred frames are what the user is
// complaining about, not the average since startup.
type stageStats struct {
	n      int
	slow   int
	total  time.Duration
	max    time.Duration
	recent []time.Duration
	next   int
}

func (s *stageStats) add(d time.Duration) {
	s.n++
	s.total += d
	if d > s.max {
		s.max = d
	}
	if d >= slowFrame {
		s.slow++
	}
	if len(s.recent) < frameRing {
		s.recent = append(s.recent, d)
		return
	}
	s.recent[s.next] = d
	s.next = (s.next + 1) % frameRing
}

func (s *stageStats) pct(p int) time.Duration {
	if len(s.recent) == 0 {
		return 0
	}
	r := append([]time.Duration(nil), s.recent...)
	sort.Slice(r, func(i, j int) bool { return r[i] < r[j] })
	i := len(r) * p / 100
	if i >= len(r) {
		i = len(r) - 1
	}
	return r[i]
}

type frameStats struct {
	mu     sync.Mutex
	st     [numStages]stageStats
	warned bool
	// worst names the message type behind the slowest Update seen. "A frame was
	// slow" is not actionable; "the frame handling evMsg:history was slow" is.
	worst     time.Duration
	worstKind string
	slowKinds map[string]int
}

var frames frameStats

// observe records one stage, and reports whether this sample is the one that
// crosses into "something is wrong" — see slowWarning.
func (f *frameStats) observe(s stage, d time.Duration) (warn bool) {
	return f.observeMsg(s, d, "")
}

// observeMsg is observe with the message type that caused it, for Update.
func (f *frameStats) observeMsg(s stage, d time.Duration, kind string) (warn bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.st[s].add(d)
	if s == stageUpdate && kind != "" {
		if d > f.worst {
			f.worst, f.worstKind = d, kind
		}
		if d >= slowFrame {
			if f.slowKinds == nil {
				f.slowKinds = map[string]int{}
			}
			f.slowKinds[kind]++
		}
	}
	if s != stageUpdate || f.warned {
		return false
	}
	// Enough samples to mean something, and a tenth of them stuttering.
	u := &f.st[stageUpdate]
	if u.n >= 50 && u.slow*10 >= u.n {
		f.warned = true
		return true
	}
	return false
}

// report renders the stats for /perf.
func (f *frameStats) report() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.st[stageUpdate].n == 0 && f.st[stageRefresh].n == 0 {
		return "nothing rendered yet"
	}
	var b strings.Builder
	for i := range f.st {
		s := &f.st[i]
		if s.n == 0 {
			continue
		}
		fmt.Fprintf(&b, "%-8s n=%-6d mean %-8s p50 %-8s p95 %-8s max %s",
			stageName[i], s.n, round(s.total/time.Duration(s.n)),
			round(s.pct(50)), round(s.pct(95)), round(s.max))
		if s.slow > 0 {
			fmt.Fprintf(&b, "  · %d slow (≥%s)", s.slow, slowFrame)
		}
		b.WriteByte('\n')
	}
	if f.worstKind != "" {
		fmt.Fprintf(&b, "slowest message: %s (%s)\n", f.worstKind, round(f.worst))
	}
	if len(f.slowKinds) > 0 {
		kinds := make([]string, 0, len(f.slowKinds))
		for k := range f.slowKinds {
			kinds = append(kinds, fmt.Sprintf("%s×%d", k, f.slowKinds[k]))
		}
		sort.Strings(kinds)
		fmt.Fprintf(&b, "slow by message: %s\n", strings.Join(kinds, " "))
	}
	fmt.Fprintf(&b, "redraws capped at %dHz while streaming · DUN_PPROF=127.0.0.1:6060 for a profile", renderHz)
	return b.String()
}

// slowWarning is what the user sees when dun notices its own stutter, and the
// path of the dump written alongside it. Emitted ONCE per session: a warning
// that repeats while the UI is already struggling is just more work.
func (f *frameStats) slowWarning() string {
	path := filepath.Join(os.TempDir(), fmt.Sprintf("dun-perf-%d.txt", os.Getpid()))
	body := "dun perf dump\n\n" + f.report() + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return "⚠ rendering is stuttering — /perf for the numbers"
	}
	return "⚠ rendering is stuttering and may be dropping input — /perf for the numbers, details in " + path
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

// msgKind labels a Bubble Tea message for the timing map. Engine events carry
// their own type, which is the distinction that matters — "evMsg" alone would
// lump a 20-byte token in with a full history replay.
func msgKind(msg any) string {
	switch m := msg.(type) {
	case evMsg:
		if t, ok := m["type"].(string); ok {
			return "ev:" + t
		}
		return "ev:?"
	default:
		return fmt.Sprintf("%T", msg)
	}
}
