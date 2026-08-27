package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// Session replay: record the engine's event stream, then feed it back to the
// TUI at the original pacing.
//
// Everything about the render path had to be inferred from benchmarks written
// after the fact — a 5s stall was found by attributing frames to message types,
// not by reproducing it. A trace closes that gap: the exact events, in the
// exact order, with the exact gaps, driving the exact UI, with no LLM and no
// luck. `dun --replay t.jsonl` then `/perf` is a measurement anyone can repeat.
//
// It is also the only honest way to test a UI against a REAL session. The
// fixtures in the tests are what someone imagined a conversation looks like;
// a trace is one that happened.

// traceEntry is one recorded engine event. The offset is from the first event,
// not wall-clock, so a trace replays the same in a year.
type traceEntry struct {
	MS int64           `json:"ms"`
	Ev json.RawMessage `json:"ev"`
}

// traceWriter appends events to a trace file. Recording must never break the
// session it is recording: every error disables the writer and is otherwise
// ignored.
type traceWriter struct {
	mu    sync.Mutex
	f     *os.File
	start time.Time
	dead  bool
}

// newTraceWriter opens the trace named by DUN_TRACE, or returns nil.
func newTraceWriter() *traceWriter {
	path := os.Getenv("DUN_TRACE")
	if path == "" {
		return nil
	}
	f, err := os.Create(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dun: trace: %v\n", err)
		return nil
	}
	return &traceWriter{f: f, start: time.Now()}
}

func (w *traceWriter) write(line []byte) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.dead {
		return
	}
	e := traceEntry{MS: time.Since(w.start).Milliseconds(), Ev: append(json.RawMessage(nil), line...)}
	b, err := json.Marshal(e)
	if err != nil {
		w.dead = true
		return
	}
	if _, err := w.f.Write(append(b, '\n')); err != nil {
		w.dead = true
	}
}

func (w *traceWriter) close() {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.f.Close()
	w.dead = true
}

// replayPacing decides the gap between replayed events.
//
// Recorded offsets are the default because the WHOLE POINT of a trace is that
// the timing is part of the bug: a burst of 60 tokens in a second is a
// different load from the same 60 spread over a minute, and only one of them
// reproduces the stutter. But recorded pacing is wrong for the other two uses —
// a CI check does not want to sit through a ten-minute session, and hunting for
// a load ceiling wants events faster than they ever really arrived.
//
// Delay < 0 means "use what was recorded"; 0 means no delay at all
// (fast-forward); anything else is a fixed gap that overrides the recording.
//
// maxGap is the refinement that makes recorded timing usable: only the SHORT
// gaps carry load. Tokens arriving 16ms apart are the thing that stutters; the
// four minutes while someone read the reply reproduce nothing but four minutes.
// Clamping long gaps keeps every burst exactly as it happened and throws away
// the dead air between them — which is what makes a real session replayable in
// seconds instead of in real time.
type replayPacing struct {
	speed  float64       // proportional: 2 = twice as fast. Ignored when delay >= 0.
	delay  time.Duration // fixed gap; negative = use the recorded offsets
	maxGap time.Duration // clamp idle gaps to this; 0 = verbatim
}

// gap returns how long to wait before the event at offset ms, given the time
// already elapsed since the replay started.
func (p replayPacing) gap(ms int64, elapsed time.Duration) time.Duration {
	if p.delay >= 0 {
		return p.delay
	}
	if p.speed <= 0 {
		return 0
	}
	// Sleep to the event's OWN offset rather than the gap, so scheduling jitter
	// cannot accumulate across a long trace.
	return time.Duration(float64(ms)*float64(time.Millisecond)/p.speed) - elapsed
}

// replayProc is a dunProc fed from a trace instead of a subprocess. It has no
// cmd and a discarding stdin: replay is a rerun of what the engine SAID, so
// anything typed has nowhere to go (the TUI says so — see replaying).
func replayProc(path string, pacing replayPacing) (*dunProc, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	var entries []traceEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		var e traceEntry
		if json.Unmarshal(sc.Bytes(), &e) == nil && len(e.Ev) > 0 {
			entries = append(entries, e)
		}
	}
	_ = f.Close()
	if len(entries) == 0 {
		return nil, fmt.Errorf("dun: %s: no events in trace", path)
	}
	stats := squeezeIdle(entries, pacing)

	ch := make(chan tea.Msg, 256)
	p := &dunProc{stdin: nopCloser{io.Discard}, ch: ch, replay: stats}
	go func() {
		start := time.Now()
		for _, e := range entries {
			if due := pacing.gap(e.MS, time.Since(start)); due > 0 {
				time.Sleep(due)
			}
			var ev map[string]any
			if json.Unmarshal(e.Ev, &ev) == nil {
				ch <- evMsg(ev)
			}
		}
		ch <- eofMsg{proc: p}
	}()
	return p, nil
}

// nopCloser makes an io.Writer a WriteCloser (replay has no engine to write to).
type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }

// runReplay drives the real TUI from a trace. Same model, same render path,
// same everything — only the event source differs, which is the point: a
// measurement taken here is a measurement of the thing users run.
func runReplay(path string, pacing replayPacing, o tuiOpts) error {
	initMarkdownStyle()
	loadScriptRenderers()
	proc, err := replayProc(path, pacing)
	if err != nil {
		return err
	}
	m := newTUIModel(proc, o.workspace)
	m.opts = o
	m.replaying = true
	m.model, m.url = o.model, o.url
	// Mouse + kitty keyboard: same wiring as runTUI (see tui.go) — all-motion,
	// so a replay pane behaves like a session pane under tmux on Termux.
	if probeKitty(os.Stdin) {
		defer disableKitty()
	}
	_, err = tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseAllMotion()).Run()
	proc.close()
	return err
}

// replayStats is what a replay did to the recording, so the UI can say so. A
// replay that silently rewrites time is a replay you cannot trust as evidence.
type replayStats struct {
	Events   int
	Span     time.Duration // the recording's own length
	Played   time.Duration // how long the replay will take
	Squeezed int           // idle gaps clamped
}

func (s *replayStats) String() string {
	if s == nil {
		return ""
	}
	out := fmt.Sprintf("%d events over %s", s.Events, s.Span.Round(time.Second))
	if s.Squeezed > 0 {
		out += fmt.Sprintf(" · %d idle gap(s) compressed, replayed in %s",
			s.Squeezed, s.Played.Round(time.Second))
	}
	return out
}

// squeezeIdle rewrites each entry's offset so gaps longer than pacing.maxGap
// collapse to it, and reports what it changed.
//
// The offsets stay ABSOLUTE (rewritten, not turned into gaps) because the
// player sleeps to each event's own offset — that is what stops scheduling
// jitter accumulating across a long trace, and it has to survive this.
func squeezeIdle(entries []traceEntry, pacing replayPacing) *replayStats {
	st := &replayStats{Events: len(entries)}
	if len(entries) == 0 {
		return st
	}
	st.Span = time.Duration(entries[len(entries)-1].MS) * time.Millisecond
	if pacing.maxGap <= 0 || pacing.delay >= 0 {
		st.Played = st.Span
		return st
	}
	capMS := pacing.maxGap.Milliseconds()
	var prevRaw, adj int64
	for i := range entries {
		gap := entries[i].MS - prevRaw
		prevRaw = entries[i].MS
		if gap > capMS {
			st.Squeezed++
			gap = capMS
		}
		adj += gap
		entries[i].MS = adj
	}
	st.Played = time.Duration(adj) * time.Millisecond
	return st
}
