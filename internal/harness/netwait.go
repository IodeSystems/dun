package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/dun/internal/fsutil"
)

// netwait.go — continue when the network returns (USER 2026-11-18: a laptop
// loses and regains network while dun works).
//
// The provider retries (retry.go) cover an outage up to their budget; past
// it the turn ends with an error and, before this, nothing ran again when
// the network came back. Now:
//
//   - A turn that ends on a NETWORK fault (NetworkDown: refused, reset, DNS,
//     timeout, no route; not a 4xx, a 402 budget, a 5xx or a model error)
//     with an ask open (todo or doing, asks.go) puts the session in
//     `waiting for network`. Only where asks are kept (asksOn) and with a
//     provider URL (Config.Endpoint.URL).
//   - The wait probes the provider with an HTTP GET of <base>/v1/models: any
//     response, whatever its status, is the network back. No tokens. Backoff
//     5s doubling to 60s. No OS network-change hint: not portable cheaply.
//   - Probe success starts ONE automatic turn: an "[automatic] the network
//     was down from 14:02 to 14:19; continue: <ask>" notification and a
//     wake. It is ask work, so the automatic-turn budget allows it
//     (autoturn.go). A resumed turn that fails the same way waits again;
//     each open ask is resumed at most netResumeCap times in 24 hours, then
//     the wait is not started and a give-up note says so.
//   - The person's message cancels the wait (it runs as usual); so does any
//     turn that succeeds (the network works).
//   - State is in <session>.netwait.json: a restarted engine with the flag
//     set and an ask still open waits again. dund's restart does not touch
//     it: the engine keeps running (engine adoption).
//   - Shown through turn_state's net_wait (NetWait), the TUI status line,
//     web/app and the `dun ps` row.
//
// /retry settings still apply: with an unbounded budget a turn never gives
// up, so this path is not reached.

const netResumeCap = 12 // resumes per ask in a rolling 24 hours

// The probe schedule. Vars so a test can run it in milliseconds.
var (
	netWaitFirst = 5 * time.Second
	netWaitMax   = 60 * time.Second
)

const netProbeTime = 5 * time.Second // one probe's timeout

// NetWaitInfo is the wait in progress; the zero value is none.
type NetWaitInfo struct {
	Since time.Time // when the network went down
	Next  time.Time // the next probe
}

// Waiting reports whether a wait is in progress.
func (w NetWaitInfo) Waiting() bool { return !w.Since.IsZero() }

type netWaitState struct {
	mu      sync.Mutex
	on      bool
	since   time.Time
	next    time.Time
	gen     uint64
	cancel  context.CancelFunc
	downAt  time.Time              // first network retry since the last success
	resumes map[string][]time.Time // ask node -> resume times, 24h
	loaded  bool
	closed  bool
}

type netWaitFile struct {
	Waiting bool                   `json:"waiting,omitempty"`
	Since   time.Time              `json:"since,omitzero"`
	Resumes map[string][]time.Time `json:"resumes,omitempty"`
}

// NetworkDown reports whether err is the network (or the provider's host)
// not answering: what waiting for the network can fix. Narrower than
// llm.TransientUpstream: a 429, a 5xx, a gateway with no backend and a
// spent budget are the provider's state, not the network's.
func NetworkDown(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	var spent *llm.BudgetSpentError
	if errors.As(err, &spent) {
		return false
	}
	for _, e := range []error{syscall.ECONNREFUSED, syscall.ECONNRESET, syscall.ENETUNREACH,
		syscall.EHOSTUNREACH, syscall.ETIMEDOUT, syscall.ENETDOWN, syscall.EPIPE} {
		if errors.Is(err, e) {
			return true
		}
	}
	var dns *net.DNSError
	var op *net.OpError
	if errors.As(err, &dns) || errors.As(err, &op) {
		return true
	}
	// A turn's own deadline (--timeout) is not the network; a dial's or a
	// read's timeout inside the client is, and reaches here as text.
	if errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	s := strings.ToLower(err.Error())
	// The client's give-up wraps the last error with %v: only text is left.
	if strings.Contains(s, "of transport failures") {
		return true
	}
	if gatewayStatus(s) != "" || strings.Contains(s, "status ") {
		return false // the provider answered
	}
	for _, sig := range []string{"connection refused", "connection reset", "no such host",
		"network is unreachable", "no route to host", "i/o timeout", "tls handshake timeout",
		"network is down", "broken pipe", "unexpected eof", "dial tcp", "server misbehaving"} {
		if strings.Contains(s, sig) {
			return true
		}
	}
	return false
}

// gatewayStatus mirrors llm's: a 429/502/503/504 in the error's text.
func gatewayStatus(s string) string {
	for _, c := range []string{"429", "502", "503", "504"} {
		for _, p := range []string{"status " + c, "status:" + c, "returned " + c, "http " + c} {
			if strings.Contains(s, p) {
				return c
			}
		}
	}
	return ""
}

// netWaitOn: asks are kept and there is a provider to probe.
func (h *Harness) netWaitOn() bool {
	return h.asksOn() && h.cfg.Endpoint.URL != ""
}

func netWaitPath(sessionFile string) string {
	return strings.TrimSuffix(sessionFile, ".jsonl") + ".netwait.json"
}

// netLoadLocked reads the sidecar once. Under netWait.mu.
func (h *Harness) netLoadLocked() netWaitFile {
	w := &h.netWait
	var f netWaitFile
	if h.cfg.SessionFile != "" {
		if b, err := os.ReadFile(netWaitPath(h.cfg.SessionFile)); err == nil {
			_ = json.Unmarshal(b, &f)
		}
	}
	if !w.loaded {
		w.loaded, w.resumes = true, f.Resumes
	}
	return f
}

// netSaveLocked writes the sidecar. Under netWait.mu.
func (h *Harness) netSaveLocked() {
	if h.cfg.SessionFile == "" {
		return
	}
	w := &h.netWait
	f := netWaitFile{Waiting: w.on, Resumes: w.resumes}
	if w.on {
		f.Since = w.since
	}
	p := netWaitPath(h.cfg.SessionFile)
	if !f.Waiting && len(f.Resumes) == 0 {
		_ = os.Remove(p)
		return
	}
	b, _ := json.Marshal(f)
	_ = fsutil.WriteAtomic(p, append(b, '\n'), 0o600)
}

// netNote watches the retry notes for when the network went down.
func (h *Harness) netNote(n RetryNote) {
	w := &h.netWait
	w.mu.Lock()
	defer w.mu.Unlock()
	switch n.Kind {
	case "transport", "interrupted":
		if w.downAt.IsZero() {
			w.downAt = time.Now().Add(-n.Elapsed)
		}
	case "recovered":
		w.downAt = time.Time{}
	}
}

// noteRetry is every retry note: what the wait needs, then the UI.
func (h *Harness) noteRetry(n RetryNote) {
	h.netNote(n)
	if h.onRetry != nil {
		h.onRetry(n)
	}
}

// openAsks is what the network wait resumes: the intake, one id per
// intake, and the open asks not blocked (asktool.go asksWaiting).
func (h *Harness) openAsks() (ids, titles []string) { return h.asksWaiting() }

// resumesLocked is ask id's resumes in the last 24 hours, pruned.
func (h *Harness) resumesLocked(id string, now time.Time) int {
	w := &h.netWait
	var kept []time.Time
	for _, t := range w.resumes[id] {
		if now.Sub(t) < 24*time.Hour {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(w.resumes, id)
	} else {
		w.resumes[id] = kept
	}
	return len(kept)
}

// netWaitAfter runs where a turn that ran ends: a success ends a wait, a
// network fault with an ask open starts one.
func (h *Harness) netWaitAfter(err error) {
	if err == nil {
		h.netWait.mu.Lock()
		h.netWait.downAt = time.Time{}
		h.netWait.mu.Unlock()
		h.netWaitCancel()
		return
	}
	if !h.netWaitOn() || !NetworkDown(err) {
		return
	}
	ids, _ := h.openAsks()
	if len(ids) == 0 {
		return
	}
	w := &h.netWait
	w.mu.Lock()
	if w.on || w.closed {
		w.mu.Unlock()
		return
	}
	h.netLoadLocked()
	now := time.Now()
	under := 0
	for _, id := range ids {
		if h.resumesLocked(id, now) < netResumeCap {
			under++
		}
	}
	if under == 0 {
		w.mu.Unlock()
		h.retry(RetryNote{Scope: "turn", Kind: "giveup", Reason: fmt.Sprintf(
			"the network is down; the open ask was resumed %d times in 24 hours, so dun does not wait again — send a message to continue",
			netResumeCap)})
		return
	}
	since := w.downAt
	if since.IsZero() {
		since = now
	}
	h.netStartLocked(since)
	w.mu.Unlock()
	h.netChanged()
}

// netStartLocked starts the probe loop. Under netWait.mu.
func (h *Harness) netStartLocked(since time.Time) {
	w := &h.netWait
	w.on, w.since = true, since
	w.gen++
	ctx, cancel := context.WithCancel(context.Background())
	w.cancel = cancel
	w.next = time.Now().Add(netWaitFirst)
	h.netSaveLocked()
	go h.netLoop(ctx, w.gen, netWaitFirst, netWaitMax) // read here: the loop touches no package var
}

// netLoop probes until the network answers, then starts the continue.
func (h *Harness) netLoop(ctx context.Context, gen uint64, delay, max time.Duration) {
	for {
		w := &h.netWait
		w.mu.Lock()
		if w.gen != gen || !w.on {
			w.mu.Unlock()
			return
		}
		w.next = time.Now().Add(delay)
		w.mu.Unlock()
		h.netChanged()
		if !sleepOrCancel(ctx, delay) {
			return
		}
		if h.netProbe(ctx) {
			h.netResume(gen)
			return
		}
		delay *= 2
		if delay > max {
			delay = max
		}
	}
}

// netProbe reports whether the provider's host answers at all: any HTTP
// response is the network back (a 401 or 404 included). No tokens.
func (h *Harness) netProbe(ctx context.Context) bool {
	u := strings.TrimSuffix(h.cfg.Endpoint.URL, "/")
	if !strings.HasSuffix(u, "/v1") {
		u += "/v1"
	}
	ctx, cancel := context.WithTimeout(ctx, netProbeTime)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u+"/models", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}

// netResume ends the wait gen and wakes the session with the continue.
func (h *Harness) netResume(gen uint64) {
	ids, titles := h.openAsks()
	w := &h.netWait
	w.mu.Lock()
	if w.gen != gen || !w.on || w.closed {
		w.mu.Unlock()
		return
	}
	since := w.since
	w.on = false
	w.cancel()
	now := time.Now()
	var named []string
	for i, id := range ids {
		if h.resumesLocked(id, now) >= netResumeCap {
			continue
		}
		if w.resumes == nil {
			w.resumes = map[string][]time.Time{}
		}
		w.resumes[id] = append(w.resumes[id], now)
		named = append(named, titles[i])
	}
	h.netSaveLocked()
	w.mu.Unlock()
	h.netChanged()
	if len(named) == 0 {
		return // closed or answered meanwhile: nothing to continue
	}
	h.Notify(netResumeText(since, now, named))
	select {
	case h.wake <- struct{}{}:
	default:
	}
}

// netResumeText is the continue's message.
func netResumeText(since, now time.Time, asks []string) string {
	return fmt.Sprintf("[automatic] the network was down from %s to %s; continue: %s",
		since.Local().Format("15:04"), now.Local().Format("15:04"), strings.Join(asks, "; "))
}

// netWaitCancel ends a wait without a turn: the person wrote, or a turn
// got through.
func (h *Harness) netWaitCancel() {
	w := &h.netWait
	w.mu.Lock()
	if !w.on {
		w.mu.Unlock()
		return
	}
	w.on = false
	w.gen++
	w.cancel()
	h.netSaveLocked()
	w.mu.Unlock()
	h.netChanged()
}

// netWaitResume continues a wait the sidecar says was on when the engine
// stopped, while an ask is still open.
func (h *Harness) netWaitResume() {
	if !h.netWaitOn() || h.cfg.SessionFile == "" {
		return
	}
	w := &h.netWait
	w.mu.Lock()
	f := h.netLoadLocked()
	if !f.Waiting || w.on {
		w.mu.Unlock()
		return
	}
	if ids, _ := h.openAsks(); len(ids) == 0 {
		h.netSaveLocked() // waiting off: nothing to continue
		w.mu.Unlock()
		return
	}
	since := f.Since
	if since.IsZero() {
		since = time.Now()
	}
	h.netStartLocked(since)
	w.mu.Unlock()
	h.netChanged()
}

// netWaitClose stops the probes with the harness. The sidecar stays.
func (h *Harness) netWaitClose() {
	w := &h.netWait
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	w.gen++
	if w.cancel != nil {
		w.cancel()
	}
}

// NetWait is the wait in progress (turn_state's net_wait).
func (h *Harness) NetWait() NetWaitInfo {
	w := &h.netWait
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.on {
		return NetWaitInfo{}
	}
	return NetWaitInfo{Since: w.since, Next: w.next}
}

func (h *Harness) netChanged() {
	if h.cfg.OnNetWait != nil {
		h.cfg.OnNetWait()
	}
}
