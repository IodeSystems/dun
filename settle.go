package dun

import (
	"encoding/json"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/iodesystems/dun/internal/fsutil"
)

// settle.go — the delayed end-of-turn report (docs/attention.md, "Delayed
// settle"). Interactive mode has no end-of-turn check: the person reads the
// last message. TB2.1 at GLM-5.3 low effort (2026-11-07) showed models in dun
// and pi declaring victory after a few steps; dun's immediate check turned
// some failing trials into passes, but only with no one to ask.
//
// So: when a turn the person started has used tools and ended, a timer
// starts. The person replying (Say, a new turn) cancels it: they are the
// check. The TUI's presence ping (control "presence") restarts it: someone
// who types, scrolls or opens a pane is reading. If it runs out, ONE wake
// carries the settle prompt; the model compares what it did with what was
// asked, finishes what needs no one, and ends with a status.
//
// Decisions (USER 2026-11-07): the delay is 2 minutes, --settle-delay or
// DUN_SETTLE_DELAY (0 is off); a settle is a dun-originated wake and may
// start a turn; its report stays in the context like any entry; its PROMPT
// is dropped when the next turn starts (it is about the turn that ran it);
// only the main agent, only interactive.
//
// Rules the code keeps:
//   - Only a user turn (Ask) arms. The settle turn is a Continue, so it can
//     never arm another: once per user message, by construction.
//   - The timer never starts a turn on top of a running one. If it runs out
//     during a turn (a job's wake turn), it starts again: the turn that ends
//     is when the person could first have read, so the delay is counted anew.
//   - "Used tools" is counted where the model's tool calls arrive
//     (mergeForcedToolCalls), not guessed from text.
//   - Owed state is a sidecar written when the timer arms and removed when
//     the settle is answered, so a crash is covered as well as a clean exit.

const (
	// settleTag marks the prompt's history entry, so dropTagged finds it.
	settleTag = "settle"

	defaultSettleDelay = 2 * time.Minute

	settlePrompt = "[settle] The user has been away since your last reply. Compare what you did with what they asked; " +
		"finish what you can without them; then end with a short status: done (what was done), remaining (what is left, " +
		"and that you continue), needs you (a decision only the user can make), or waiting on (which job or agent, and " +
		"what for; set its reports with jobs({job, every_secs}) to the rate you want, then end the turn: it wakes you)."

	// settleResumeNote rides the first user turn of a resumed session whose
	// settle was owed. No request is sent for it.
	settleResumeNote = "[settle] The last turn's check did not finish: before the new request, say briefly what was done and what remains."
)

// settleState is the timer and what hangs on it. The zero value is ready.
type settleState struct {
	mu    sync.Mutex
	timer *time.Timer
	gen   uint64 // a firing timer whose gen is not current is stale
	armed bool   // the timer runs
	fired bool   // the prompt is queued, or its turn has not finished
	turns int    // turns running now (Ask, Continue)
	// resumeOwed: the sidecar said a settle was owed at start. resumeNow: the
	// user turn being prepared carries the note.
	resumeOwed, resumeNow bool
	closed                bool
	tools                 atomic.Int64 // tool calls the model made this turn
}

// settleFile is the session's owed-settle sidecar.
func settleFile(sessionFile string) string {
	return strings.TrimSuffix(sessionFile, ".jsonl") + ".settle.json"
}

// settleDelay is the wait, 0 when settle is off: Config.SettleDelay
// (negative: off), else DUN_SETTLE_DELAY, else 2 minutes.
func (h *Harness) settleDelay() time.Duration {
	switch d := h.cfg.SettleDelay; {
	case d < 0:
		return 0
	case d > 0:
		return d
	}
	if v := os.Getenv("DUN_SETTLE_DELAY"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= 0 {
			return d
		}
	}
	return defaultSettleDelay
}

// settleOn: only the main agent, only where a person may come back.
func (h *Harness) settleOn() bool {
	return h != nil && !h.isChild() && !h.cfg.NonInteractive && h.settleDelay() > 0
}

// settleBegin runs where a turn starts, before prepareTurn. user is a turn
// the person started (Ask): it cancels the timer, answers the owed settle and
// drops the old settle prompt. It reports whether this turn is the settle
// turn (a Continue with the prompt in it).
func (h *Harness) settleBegin(user bool) (settleTurn bool) {
	if !h.settleOn() {
		return false
	}
	s := &h.settle
	if user {
		h.settleCancel()
	}
	s.mu.Lock()
	s.turns++
	s.tools.Store(0)
	if user {
		s.resumeNow, s.resumeOwed = s.resumeOwed, false
	} else {
		settleTurn = s.fired
	}
	s.mu.Unlock()
	if !settleTurn {
		// The prompt described the turn it started; no later turn needs it.
		h.store.dropTagged(settleTag)
	}
	if user {
		h.settleOwe(false)
	}
	return settleTurn
}

// settleEnd runs where a turn ends. A user turn that used tools and did not
// fail arms the timer. The settle turn, finished, clears the owed state; one
// that failed stays owed.
func (h *Harness) settleEnd(user, settleTurn bool, err error) {
	if !h.settleOn() {
		return
	}
	s := &h.settle
	s.mu.Lock()
	defer s.mu.Unlock()
	s.turns--
	if user && err == nil && s.tools.Load() > 0 && !s.closed {
		h.settleStartLocked()
		h.settleOwe(true)
	}
	if settleTurn && err == nil {
		s.fired = false
		h.settleOwe(false)
	}
}

// settleStartLocked (re)starts the timer. s.mu is held.
func (h *Harness) settleStartLocked() {
	s := &h.settle
	if s.timer != nil {
		s.timer.Stop()
	}
	s.gen++
	g := s.gen
	s.armed = true
	s.timer = time.AfterFunc(h.settleDelay(), func() { h.settleFire(g) })
}

// settleCancel stops the timer and takes back a queued prompt: the user
// answered. Safe on any goroutine, before the harness is a harness.
func (h *Harness) settleCancel() {
	s := &h.settle
	s.mu.Lock()
	was := s.armed || s.fired
	if s.timer != nil {
		s.timer.Stop()
	}
	s.gen++
	s.armed, s.fired = false, false
	s.mu.Unlock()
	if was {
		h.drain(func(q queued) bool { return q.tag == settleTag })
		h.settleOwe(false)
	}
}

// SettlePresence is the TUI's word that the person is here (control
// "presence"): a running timer starts again. After the timer has fired, or
// during a turn, it does nothing: typing is not a reply, and a started settle
// turn is cut only by a sent message.
func (h *Harness) SettlePresence() {
	s := &h.settle
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.armed && !s.closed && s.turns == 0 {
		h.settleStartLocked()
	}
}

// settleFire queues the prompt and wakes the driver.
func (h *Harness) settleFire(gen uint64) {
	s := &h.settle
	s.mu.Lock()
	defer s.mu.Unlock()
	if gen != s.gen || !s.armed || s.closed {
		return
	}
	if s.turns > 0 {
		h.settleStartLocked() // never on top of a turn: count the delay anew
		return
	}
	s.armed, s.fired = false, true
	h.noteMu.Lock()
	h.queue = append(h.queue, queued{kind: queuedNotification, text: settlePrompt, tag: settleTag})
	h.noteMu.Unlock()
	select {
	case h.wake <- struct{}{}:
	default: // a turn is already due
	}
}

// settleClose stops the timer with the harness. The sidecar stays: the settle
// is still owed.
func (h *Harness) settleClose() {
	s := &h.settle
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if s.timer != nil {
		s.timer.Stop()
	}
}

// settleOwe records in the sidecar whether a settle is owed.
func (h *Harness) settleOwe(owed bool) {
	if h.cfg.SessionFile == "" {
		return
	}
	f := settleFile(h.cfg.SessionFile)
	if !owed {
		_ = os.Remove(f)
		return
	}
	b, _ := json.Marshal(map[string]bool{"owed": true})
	_ = fsutil.WriteAtomic(f, append(b, '\n'), 0o600)
}

// settleResume reads the sidecar at start.
func (h *Harness) settleResume() {
	if !h.settleOn() || h.cfg.SessionFile == "" {
		return
	}
	if b, err := os.ReadFile(settleFile(h.cfg.SessionFile)); err == nil {
		var d struct {
			Owed bool `json:"owed"`
		}
		if json.Unmarshal(b, &d) == nil && d.Owed {
			h.settle.resumeOwed = true
		}
	}
}

// settleResumeAside puts the resume note on the user turn being prepared.
// Called from prepareTurn after dropTurnScoped, which would drop it earlier.
func (h *Harness) settleResumeAside() {
	s := &h.settle
	s.mu.Lock()
	now := s.resumeNow
	s.resumeNow = false
	s.mu.Unlock()
	if now {
		h.AsideWith(settleResumeNote, AsideOpts{Key: "settle-resume", TurnScoped: true})
	}
}
