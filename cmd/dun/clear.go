package main

import (
	"sync"
	"time"

	"github.com/iodesystems/dun/internal/harness"
	"github.com/iodesystems/dun/proto"
)

// /clear while a turn runs (USER 2026-11-24: "/clear takes a while").
//
// Before, the clear was done at once on the reader goroutine and the turn
// ran on: the screen cleared, but the session stayed busy until the turn
// ended (16s for an exec still in its grace, measured), a message typed
// after the clear rode the OLD turn, and that turn's tool calls and reply
// were written into the cleared conversation.
//
// Now a clear with a turn running interrupts it and says so (proto.Clearing,
// naming what the turn was doing); the turn loop clears the conversation the
// moment the turn is over (clearing.take), before it runs anything else. A
// message sent meanwhile is queued as any message sent mid-turn is (the
// gate), is not lifted into the interrupted turn (liftQueued), survives the
// clear (Reset keeps the queue) and runs in the cleared conversation. With
// no turn running the clear is done at once, as before. reset carries how
// long it took (proto.Timing).

// clearing is a /clear waiting for the running turn to stop.
type clearing struct {
	mu      sync.Mutex
	pending *harness.PhaseClock // the clear's clock; nil: none waits
	waiting string              // what the turn was doing, the clock's phase
}

// begin starts a clear: when stop reports a turn it stopped, the clear
// waits for the loop (true); otherwise there is nothing to wait for
// (false), and the caller clears now. Under the lock, so the loop's take
// sees either no clear or one whose turn was told to stop.
func (c *clearing) begin(clk *harness.PhaseClock, waiting string, stop func() bool) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pending != nil {
		return true // a second /clear while the first waits: the same clear
	}
	if !stop() {
		return false
	}
	c.pending, c.waiting = clk, waiting
	return true
}

// take returns the clear that waits, once, and the phase it waited in.
func (c *clearing) take() (*harness.PhaseClock, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	clk, w := c.pending, c.waiting
	c.pending, c.waiting = nil, ""
	return clk, w
}

// waitingFor is what the running turn is doing, in words, for the clearing
// event: its tool, a question it waits on, a provider wait, or the reply.
func (t *turnTracker) waitingFor() string {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	switch {
	case t.st.Tool != "":
		return t.st.Tool
	case t.st.Phase == proto.PhaseWaitingAnswer:
		return "the open question"
	case t.st.Phase == proto.PhaseRetrying:
		return "a provider retry"
	}
	return "the model's reply"
}

// clearNow clears the conversation and says so, with the clear's timing.
func clearNow(h *harness.Harness, em *emitter, clk *harness.PhaseClock) {
	h.Reset()
	clk.Mark("clear")
	em.emit(&proto.ResetEvent{Timing: clk.Timing()})
}

// newClear is a clear's clock, started now.
func newClear() *harness.PhaseClock { return harness.NewPhaseClock(time.Now()) }
