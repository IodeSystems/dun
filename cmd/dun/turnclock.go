package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/iodesystems/dun"
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
// The budget is SOFT (USER, 2026-10-23). When it runs out the turn is not
// cancelled: the "spent" mark tells the model (at its next call) to checkpoint
// and end the turn, and a grace period starts on the same pausable timer. Only
// when the grace runs out too is the turn cut — the "cut" mark, then the
// cancel (see budgetMarks). A cut
// with no warning lost the turn's last step and left the model no way to know
// why its work had stopped.
//
// Everything else about it behaves like a deadline context: it cancels on its
// parent, and cancelling is one-way.
type turnClock struct {
	ctx    context.Context
	cancel context.CancelCauseFunc

	budget time.Duration // the whole budget; for the messages
	grace  time.Duration // after the budget, before the cut; 0 = cut at the budget

	mu      sync.Mutex
	inGrace bool          // the budget is spent; the timer is counting the grace
	over    bool          // cut or stopped: never starts again
	gen     int           // bumped when the timer is let go; a stale fire is ignored
	left    time.Duration // time remaining; meaningful while paused
	due     time.Time     // when the running phase ends; meaningful while running
	timer   *time.Timer   // nil when paused, stopped, or unbudgeted
}

// budgetMarks is what the clock does at its two marks: spent (the budget ran
// out; tell the model) and cut (the grace ran out too). Either may be nil.
// Called from the timer's goroutine, never under the clock's lock.
type budgetMarks struct {
	spent, cut func(budget, grace time.Duration)
}

// budgetHooks is set once the harness exists (main). Read when a mark is
// reached, not when a clock is made: the one-shot run's clock starts before
// the harness does.
var budgetHooks atomic.Pointer[budgetMarks]

func (c *turnClock) mark(cut bool) {
	m := budgetHooks.Load()
	if m == nil {
		return
	}
	if f := m.spent; !cut && f != nil {
		f(c.budget, c.grace)
	}
	if f := m.cut; cut && f != nil {
		f(c.budget, c.grace)
	}
}

// turnGrace is --timeout-grace: 0 = auto (graceFor), negative = no grace.
var turnGrace time.Duration

// graceFor is the grace a budget gets: --timeout-grace when set, else 20% of
// the budget, at least 2m and at most 10m. Enough to commit and summarize; not
// so much that a turn that ignores the notice runs on for long.
func graceFor(budget time.Duration) time.Duration {
	switch {
	case budget <= 0 || turnGrace < 0:
		return 0
	case turnGrace > 0:
		return turnGrace
	}
	return min(max(budget/5, 2*time.Minute), 10*time.Minute)
}

// newTurnClock starts a clock over parent. A budget of 0 means no budget: the
// result is an ordinary cancelable context that only ends with its parent.
func newTurnClock(parent context.Context, budget, grace time.Duration) *turnClock {
	ctx, cancel := context.WithCancelCause(parent)
	c := &turnClock{ctx: ctx, cancel: cancel, budget: budget, grace: grace, left: budget}
	c.mu.Lock()
	c.startLocked()
	c.mu.Unlock()
	return c
}

func (c *turnClock) startLocked() {
	if c.budget <= 0 || c.over {
		return
	}
	// left may be 0: a pause that raced the timer firing. Fire at once rather
	// than never.
	c.left = max(c.left, 0)
	c.due = time.Now().Add(c.left)
	gen := c.gen
	c.timer = time.AfterFunc(c.left, func() { c.fire(gen) })
}

// fire is the timer reaching the end of its phase: the budget (warn, start the
// grace) or the grace (cut).
func (c *turnClock) fire(gen int) {
	c.mu.Lock()
	if gen != c.gen || c.timer == nil {
		c.mu.Unlock()
		return // paused or stopped after this fire was scheduled
	}
	c.timer = nil
	if !c.inGrace && c.grace > 0 {
		c.inGrace, c.left = true, c.grace
		c.startLocked()
		c.mu.Unlock()
		c.mark(false)
		return
	}
	c.over = true
	c.mu.Unlock()
	// The mark first, while what the turn is doing can still be read; then the
	// cancel, whose cause (dun.BudgetCut) the harness takes as an interrupt.
	c.mark(true)
	c.cancel(dun.BudgetCut{Budget: c.budget, Grace: c.grace})
}

// letGoLocked stops the timer and makes any fire already scheduled stale.
func (c *turnClock) letGoLocked() {
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	c.gen++
}

// Pause stops the clock burning, in the budget or the grace. Safe to call when
// unbudgeted, already paused, or already cut — in the last case the context is
// on its way down and resuming will not revive it.
func (c *turnClock) Pause() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.timer == nil {
		return
	}
	c.letGoLocked()
	c.left = max(time.Until(c.due), 0)
}

// Resume restarts the clock with whatever was left when it was paused.
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
	c.letGoLocked()
	c.over = true
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
	c := newTurnClock(ctx, turnTimeout, graceFor(turnTimeout))
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

// pauseClock stops the running turn's budget clock and returns the resume func.
// It is the acquire/release form of withoutClock, for a host callback that
// spans a call the clock cannot wrap inline (the Shaper's compaction LLM call
// runs in agentkit, outside withoutClock's reach). nil-safe: when no turn is
// running, or the turn is unbudgeted, the clock is already inert and the
// resume func is a no-op.
func pauseClock() (resume func()) {
	c := curClock.Load()
	c.Pause()
	return c.Resume
}

// turnErr replaces a bare cancellation with the reason the turn actually ended.
//
// The retry loop does not retry a cancelled context (see Harness.runTurn): a
// cancellation is a decision, not a transient fault, so "context canceled" is
// reported after ZERO attempts. That is correct, and it is also why the two
// words have to say which decision it was — a reader who sees a retryable-
// looking error with no retries behind it reasonably concludes retries ran out.
//
// A budget cut reaches here as dun.ErrInterrupted (the harness recorded it
// through its interrupt path) and is reported as the budget: it was not the
// user.
func turnErr(tctx context.Context, err error) error {
	if err == nil || tctx.Err() == nil {
		return err
	}
	var budget dun.BudgetCut
	if errors.Is(err, dun.ErrInterrupted) && errors.As(context.Cause(tctx), &budget) {
		return budget
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
