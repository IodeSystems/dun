package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// The turn budget bounds DUN's work, not the human's.
//
// ask_user blocks inside the turn while a person decides. Under a plain
// context.WithTimeout that thinking time is spent from the same budget as the
// model's, so a question left open long enough kills the turn that asked it —
// punishing the user for being asked. A context's deadline cannot be moved, so
// the budget is enforced by a timer that can be STOPPED instead: pause it while
// an ask is outstanding, resume it with whatever was left.
//
// Everything else about it behaves like a deadline context: it cancels on its
// parent, and cancelling is one-way.
type turnClock struct {
	ctx    context.Context
	cancel context.CancelCauseFunc

	budget time.Duration // the whole budget; for the expiry message

	mu    sync.Mutex
	left  time.Duration // budget remaining; meaningful while paused
	due   time.Time     // when it expires; meaningful while running
	timer *time.Timer   // nil when paused, stopped, or unbudgeted
}

// errTurnBudget is why a turn ended when its budget ran out.
//
// It is carried as the context's cancellation CAUSE rather than returned,
// because nothing between the timer and the error site touches the clock: the
// turn fails somewhere inside the agent loop, and what arrives there is the
// plain "context canceled" that a cancelled context always produces. Ctrl-C
// produces exactly the same two words, so before this the two were
// indistinguishable at every place that reports a failed turn — which is how a
// 30-minute clock running out read as an unexplained cancellation, and sent
// anyone debugging one looking for a crash that never happened.
type errTurnBudget struct{ budget time.Duration }

func (e errTurnBudget) Error() string {
	return fmt.Sprintf("the turn ran past its %s budget (--timeout) and was cut off", e.budget)
}

// newTurnClock starts a clock over parent. A budget of 0 means no budget: the
// result is an ordinary cancelable context that only ends with its parent.
func newTurnClock(parent context.Context, budget time.Duration) *turnClock {
	ctx, cancel := context.WithCancelCause(parent)
	c := &turnClock{ctx: ctx, cancel: cancel, budget: budget, left: budget}
	c.mu.Lock()
	c.startLocked()
	c.mu.Unlock()
	return c
}

func (c *turnClock) startLocked() {
	if c.left <= 0 {
		return
	}
	c.due = time.Now().Add(c.left)
	c.timer = time.AfterFunc(c.left, func() { c.cancel(errTurnBudget{c.budget}) })
}

// Pause stops the budget burning. Safe to call when unbudgeted, already paused,
// or already expired — in the last case the context is on its way down and
// resuming will not revive it.
func (c *turnClock) Pause() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.timer == nil {
		return
	}
	if !c.timer.Stop() {
		c.timer = nil // already fired; the cancel has happened
		c.left = 0
		return
	}
	c.timer = nil
	c.left = time.Until(c.due)
	if c.left < 0 {
		c.left = 0
	}
}

// Resume restarts the budget with whatever was left when it was paused.
func (c *turnClock) Resume() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.timer != nil {
		return
	}
	c.startLocked()
}

// Stop releases the timer and cancels the context. Idempotent.
func (c *turnClock) Stop() {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	c.mu.Unlock()
	// The ordinary end of a turn: no cause beyond "it is over". A cause set
	// here would overwrite nothing (the first cancel wins) but would claim a
	// reason for the deferred Stop that follows every turn, successful or not.
	c.cancel(context.Canceled)
}

// curClock is the clock an ask should pause: the running turn's in interactive
// mode, or the whole run's in one-shot mode. A pointer swap rather than a
// parameter because the ask hook is wired once, at Start, and cannot see the
// turn that is running when it fires.
var curClock atomic.Pointer[turnClock]

// beginTurn bounds one turn and makes its clock the one an ask pauses.
// The returned func ends the turn; it must be called.
//
// With no per-turn budget (one-shot mode) the run's clock is already installed
// and stays installed — a turn must not shadow it, or an ask would pause
// nothing.
func beginTurn(ctx context.Context) (context.Context, func()) {
	if turnTimeout <= 0 {
		return ctx, func() {}
	}
	c := newTurnClock(ctx, turnTimeout)
	prev := curClock.Swap(c)
	return c.ctx, func() {
		curClock.Store(prev)
		c.Stop()
	}
}

// withoutClock runs fn with the turn budget paused — for time spent waiting on
// a human. Returns whatever fn returns.
func withoutClock[T any](fn func() (T, error)) (T, error) {
	c := curClock.Load()
	c.Pause()
	defer c.Resume()
	return fn()
}

// turnErr replaces a bare cancellation with the reason the turn actually ended.
//
// The retry loop does not retry a cancelled context (see Harness.runTurn): a
// cancellation is a decision, not a transient fault, so "context canceled" is
// reported after ZERO attempts. That is correct, and it is also why the two
// words have to say which decision it was — a reader who sees a retryable-
// looking error with no retries behind it reasonably concludes retries ran out.
func turnErr(tctx context.Context, err error) error {
	if err == nil || tctx.Err() == nil {
		return err
	}
	// A real failure that merely landed on a turn already cut off keeps its own
	// message: the cause explains the cancellation, not an unrelated 500.
	if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	// A cause of context.Canceled is the absence of a reason — ctrl-C, or the
	// parent going down — and has nothing to add over err.
	if cause := context.Cause(tctx); cause != nil && !errors.Is(cause, context.Canceled) {
		return cause
	}
	return err
}
