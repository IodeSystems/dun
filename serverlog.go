package dun

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// A server's activity log: what dun did with it (spawned it, connected,
// waited for its tools, stopped it), how long each step took, and what the
// server wrote on stderr, as it happens. USER 2026-10-25: a server that was
// timing out said nothing until the timeout, and then only the error; the
// log says where a start is and that it is still alive.
//
// Every line calls Config.OnServerActivity, so a UI can show it while the
// start is still in flight (the CLI's /mcp table refreshes on it); Servers()
// carries each server's last serverLogShown lines.

// ServerLogLine is one entry: a step of dun's, or a line of the server's
// stderr (Stderr).
type ServerLogLine struct {
	At     time.Time `json:"at"`
	Text   string    `json:"text"`
	Stderr bool      `json:"stderr,omitempty"`
}

// stillEvery is how often a start in flight says it is still at it (a var
// for the tests).
var stillEvery = 10 * time.Second

const (
	serverLogCap   = 300 // lines kept per server
	serverLogShown = 60  // lines Servers() carries per server
)

type serverLogs struct {
	mu    sync.Mutex
	lines map[string][]ServerLogLine
}

// noteServer adds a line of dun's to id's log.
func (h *Harness) noteServer(id, format string, args ...any) {
	h.addServerLines(id, []string{fmt.Sprintf(format, args...)}, false)
}

func (h *Harness) addServerLines(id string, texts []string, stderr bool) {
	if len(texts) == 0 {
		return
	}
	now := time.Now()
	l := &h.srvLogs
	l.mu.Lock()
	if l.lines == nil {
		l.lines = map[string][]ServerLogLine{}
	}
	lines := l.lines[id]
	for _, t := range texts {
		lines = append(lines, ServerLogLine{At: now, Text: t, Stderr: stderr})
	}
	if over := len(lines) - serverLogCap; over > 0 {
		lines = append(lines[:0:0], lines[over:]...)
	}
	l.lines[id] = lines
	l.mu.Unlock()
	if cb := h.cfg.OnServerActivity; cb != nil {
		cb(id)
	}
}

// ServerLog is id's log, oldest first.
func (h *Harness) ServerLog(id string) []ServerLogLine {
	return h.serverLogTail(id, serverLogCap)
}

func (h *Harness) serverLogTail(id string, n int) []ServerLogLine {
	l := &h.srvLogs
	l.mu.Lock()
	defer l.mu.Unlock()
	lines := l.lines[id]
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return append([]ServerLogLine(nil), lines...)
}

// startPhase is where a start is, for its "still …" lines.
type startPhase struct {
	mu    sync.Mutex
	what  string
	since time.Time
}

func (p *startPhase) set(what string) {
	p.mu.Lock()
	p.what, p.since = what, time.Now()
	p.mu.Unlock()
}

func (p *startPhase) get() (string, time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.what, p.since
}

// watchStart says, every stillEvery until done is closed, that a start in
// flight is still at it. Its stderr comes on its own (MCPConfig.OnStderr).
func (h *Harness) watchStart(id string, phase *startPhase, timeout time.Duration, done <-chan struct{}) {
	t0 := time.Now()
	still := time.NewTicker(stillEvery)
	defer still.Stop()
	for {
		select {
		case <-done:
			return
		case <-h.closing():
			return
		case <-still.C:
			what, since := phase.get()
			h.noteServer(id, "still %s · %s (%s of the %s timeout)", what, secs(time.Since(since)), secs(time.Since(t0)), secs(timeout))
		}
	}
}

// closing is closed by Close; nil (never ready) for a harness not built by
// Start.
func (h *Harness) closing() <-chan struct{} { return h.closed }

// markClosed closes the closing channel, once.
func (h *Harness) markClosed() {
	if h.closed != nil {
		h.closeOnce.Do(func() { close(h.closed) })
	}
}

// secs is a duration as "1.4s", "12s" or "2m05s".
func secs(d time.Duration) string {
	switch {
	case d < 10*time.Second && d%time.Second != 0:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
}

// secretArg is a flag whose value is a credential: logged as "…".
var secretArg = regexp.MustCompile(`(?i)(key|token|secret|passw|auth|bearer|cookie|credential)`)

// commandLine is a command and its args for the log, credentials hidden:
// the value after a flag that names one, or after its "=".
func commandLine(command string, args []string) string {
	out := []string{command}
	hide := false
	for _, a := range args {
		switch {
		case hide:
			out, hide = append(out, "…"), false
		case strings.HasPrefix(a, "-") && secretArg.MatchString(a):
			if name, _, ok := strings.Cut(a, "="); ok {
				out = append(out, name+"=…")
			} else {
				out, hide = append(out, a), true
			}
		default:
			out = append(out, a)
		}
	}
	return strings.Join(out, " ")
}

// logURL is u for the log: no user info, no query.
func logURL(u string) string {
	p, err := url.Parse(u)
	if err != nil {
		return "(an address that does not parse)"
	}
	p.User, p.RawQuery, p.Fragment = nil, "", ""
	return p.String()
}

// noteStartDone ends a start's log: how it went and how long it took.
func (h *Harness) noteStartDone(ctx context.Context, id string, t0 time.Time, err error) {
	switch {
	case err == nil:
		n := 0
		for _, t := range h.mgr.GetTools() {
			if t.ServerID == id {
				n++
			}
		}
		h.noteServer(id, "ready · %d tool%s · %s", n, map[bool]string{true: "", false: "s"}[n == 1], secs(time.Since(t0)))
	case ctx.Err() != nil:
		h.noteServer(id, "start cancelled after %s", secs(time.Since(t0)))
	default:
		h.noteServer(id, "did not start after %s: %s", secs(time.Since(t0)), oneLine(err.Error(), 400))
	}
}
