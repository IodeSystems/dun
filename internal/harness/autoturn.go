package harness

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/iodesystems/agentkit/taskgraph"
)

// autoturn.go — turns nobody asked for (USER 2026-11-18: "don't stop work
// on asks; stop senseless unrequested work").
//
// Every turn has an origin: the person's (a message they typed, or one
// they typed mid-turn that a Continue carries), user-set (a [loop] or
// [scheduled] firing: the person set it earlier), or automatic (anything
// else that starts a turn: a job's finish or report, a heartbeat that
// escalated, a child's report, a server's or a peer's wake, the settle, an
// upgrade's "[automatic] continue").
//
//   - The budget. While an ask (asks.go) is open — todo or doing; a blocked
//     ask is not open — automatic turns run as before. With none open, a
//     session starts at most autoTurnsPerHour automatic turns in a rolling
//     hour; past that a wake does not start a turn: what it brought is
//     written into the conversation as a note (no inbox arrival), counted,
//     and shown (`idle: N notes waiting`: turn_state's notes_waiting). The
//     person's next message resets the count; the notes are in its context.
//     A user-set firing is not counted and is never held.
//   - No questions in a round dun added (autoRound): the rounds after the
//     turn-end check (idlecheck.go), the round after a plan nudge or stall
//     (plannudge.go), and every round of an automatic turn with no open
//     ask. ask_user there is refused with a tool error that says to block
//     the ask with the question instead (the tool stays in the set: a tool
//     set that changes per round re-reads the prompt).
//
// Only where asks are kept (asksOn): a sub-agent, a --non-interactive run
// and Config.NoAsks run every wake as before.

const (
	autoTurnsPerHour = 3
	autoTurnWindow   = time.Hour
)

// turnOrigin is who started a turn.
type turnOrigin int

const (
	originPerson  turnOrigin = iota // the person typed it (zero: a direct call)
	originUserSet                   // a [loop] or [scheduled] firing
	originAuto                      // dun's or dund's own
)

// ErrTurnHeld is a turn not started: no ask is open and the hour's
// automatic turns are used. What it would have read waits as notes.
var ErrTurnHeld = errors.New("not started: no ask is open and this hour's automatic turns are used; the news waits for the person's next message")

// autoState is the session's automatic-turn bookkeeping.
type autoState struct {
	mu     sync.Mutex
	origin turnOrigin  // the turn running now, or the last one
	starts []time.Time // automatic turns started with no ask open, in the window
	held   int         // notes written without a turn since the person's last message
	// gen counts turns begun; calls is the tool calls as the turn began.
	gen   uint64
	calls uint64
	// presence: the person was seen (control "presence") since the last
	// turn began.
	presence bool
	now      func() time.Time // tests
}

func (a *autoState) clock() time.Time {
	if a.now != nil {
		return a.now()
	}
	return time.Now()
}

// userSetText is a [loop] or [scheduled] firing (dund schedule.go).
func userSetText(t string) bool {
	return strings.HasPrefix(t, "[loop ") || strings.HasPrefix(t, "[scheduled ")
}

// PersonText reports whether text is a message the person typed: not one
// dun or dund sent in their place (a bracketed lowercase tag) and not a
// loop's or a schedule's firing.
func PersonText(text string) bool {
	t := strings.TrimSpace(text)
	return t != "" && !autoTag.MatchString(t) && !userSetText(t)
}

// textOrigin is the origin of a turn a message starts.
func textOrigin(text string) turnOrigin {
	t := strings.TrimSpace(text)
	switch {
	case userSetText(t):
		return originUserSet
	case PersonText(t):
		return originPerson
	}
	return originAuto
}

// asksOn: this session keeps asks (asks.go), so the budget applies.
func (h *Harness) asksOn() bool {
	return !h.isChild() && !h.cfg.NonInteractive && !h.cfg.NoAsks && h.hasTool(taskgraph.ToolAdd)
}

// openAskNow reports whether an ask node is todo or doing, or the intake
// has messages (asktool.go). Blocked is not open: it waits on the person.
func (h *Harness) openAskNow() bool {
	if h.intakeOpen() {
		return true
	}
	g, err := planStore{h}.View(context.Background())
	if err != nil {
		return false
	}
	for _, n := range g.Nodes {
		if (n.Status == taskgraph.Todo || n.Status == taskgraph.Doing) && h.isAskNode(n.ID) {
			return true
		}
	}
	return false
}

// autoAllow reports whether an automatic turn may start now, and counts it
// when it does and no ask is open.
func (h *Harness) autoAllow() bool {
	if !h.asksOn() || h.openAskNow() {
		return true
	}
	a := &h.auto
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.clock()
	var kept []time.Time
	for _, t := range a.starts {
		if now.Sub(t) < autoTurnWindow {
			kept = append(kept, t)
		}
	}
	if len(kept) >= autoTurnsPerHour {
		a.starts = kept
		return false
	}
	a.starts = append(kept, now)
	return true
}

// autoHold counts n notes written without a turn.
func (h *Harness) autoHold(n int) {
	h.auto.mu.Lock()
	h.auto.held += n
	h.auto.mu.Unlock()
}

// autoPersonReset: the person wrote. The count starts again, and the notes
// are in the context the message is read with.
func (h *Harness) autoPersonReset() {
	h.auto.mu.Lock()
	h.auto.starts, h.auto.held = nil, 0
	h.auto.mu.Unlock()
}

// NotesWaiting is how many notes arrived without starting a turn since the
// person's last message: turn_state's notes_waiting.
func (h *Harness) NotesWaiting() int {
	h.auto.mu.Lock()
	defer h.auto.mu.Unlock()
	return h.auto.held
}

// turnBegin notes a turn starting: its origin, the tool calls so far, and
// the turn-end check's state (once per turn, not per attempt: a retried
// turn keeps its checks, retry.go).
func (h *Harness) turnBegin(o turnOrigin) {
	var calls uint64
	if h.store != nil {
		calls = h.store.toolCallsNow()
	}
	h.auto.mu.Lock()
	h.auto.origin, h.auto.calls, h.auto.presence = o, calls, false
	h.auto.gen++
	h.auto.mu.Unlock()
	h.idle.reset()
	if h.store != nil { // a reply from before the turn is not this turn's (textcall.go)
		if e, ok := h.store.lastRoundEntry(); ok {
			h.idle.textCallSeen(e.ID)
		}
	}
	if o == originPerson {
		h.autoPersonReset()
	}
}

// turnOriginNow is the running (or last) turn's origin and its tool calls
// so far.
func (h *Harness) turnOriginNow() (turnOrigin, uint64) {
	h.auto.mu.Lock()
	o, c := h.auto.origin, h.auto.calls
	h.auto.mu.Unlock()
	var now uint64
	if h.store != nil {
		now = h.store.toolCallsNow()
	}
	return o, now - c
}

// notePresence: the person is here (control "presence").
func (h *Harness) notePresence() {
	h.auto.mu.Lock()
	h.auto.presence = true
	h.auto.mu.Unlock()
}

// queuedPerson reports whether the buffer holds a message the person wrote
// (typed mid-turn, or a `dun msg` from a shell).
func (h *Harness) queuedPerson() bool {
	h.noteMu.Lock()
	defer h.noteMu.Unlock()
	for _, q := range h.queue {
		if q.kind == queuedUser && PersonText(q.text) || q.person {
			return true
		}
	}
	return false
}

// holdPending writes what is pending into the conversation without a turn:
// the inbox arrivals are claimed, so nothing runs for them; the next turn
// reads them. Returns how many.
func (h *Harness) holdPending() int {
	n, _ := h.store.ClaimPending(context.Background(), "", 0)
	if n > 0 {
		h.autoHold(n)
	}
	return n
}
