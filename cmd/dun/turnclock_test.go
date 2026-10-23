package main

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iodesystems/dun"
)

// pauseClock is nil-safe when no turn is running: it must not panic and must
// return a callable no-op resume.
func TestPauseClock_NoTurnRunning(t *testing.T) {
	curClock.Store(nil)
	defer curClock.Store(nil)
	resume := pauseClock()
	if resume == nil {
		t.Fatal("resume func must not be nil")
	}
	resume() // must not panic
}

// pauseClock stops the running turn's budget clock for the duration of the
// fold and resumes it after. A fold that takes longer than the remaining
// budget must NOT cancel the turn's context while paused.
func TestPauseClock_StopsBudgetForFold(t *testing.T) {
	parent := context.Background()
	c := newTurnClock(parent, 100*time.Millisecond, 0)
	curClock.Store(c)
	defer curClock.Store(nil)
	defer c.Stop()

	resume := pauseClock()
	// While paused, burning well past the (100ms) budget must not cancel the ctx.
	time.Sleep(150 * time.Millisecond)
	if c.ctx.Err() != nil {
		t.Fatalf("turn clock canceled while the fold was paused: %v", c.ctx.Err())
	}
	resume()
	// After resume, the leftover budget is gone; it should fire promptly.
	deadline := time.Now().Add(200 * time.Millisecond)
	for c.ctx.Err() == nil {
		if time.Now().After(deadline) {
			t.Fatal("turn clock did not resume burning its budget after the fold")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if c.ctx.Err() != context.Canceled {
		t.Fatalf("expected context.Canceled after budget expiry, got %v", c.ctx.Err())
	}
}

// shortBudget sets a per-turn budget with no grace, so a test that wants the
// cut gets it at the budget.
func shortBudget(t *testing.T, budget time.Duration) {
	t.Helper()
	turnTimeout, turnGrace = budget, -1
	t.Cleanup(func() { turnTimeout, turnGrace = 0, 0 })
}

// markLog counts the clock's marks.
type markLog struct{ spent, cut atomic.Int32 }

func watchMarks(t *testing.T) *markLog {
	t.Helper()
	m := &markLog{}
	budgetHooks.Store(&budgetMarks{
		spent: func(time.Duration, time.Duration) { m.spent.Add(1) },
		cut:   func(time.Duration, time.Duration) { m.cut.Add(1) },
	})
	t.Cleanup(func() { budgetHooks.Store(nil) })
	return m
}

// The budget running out does not cancel: it marks "spent" once, and the turn
// runs on through the grace. Only the end of the grace cuts it, with the
// budget as the cause.
func TestTurnClock_SoftThenCut(t *testing.T) {
	m := watchMarks(t)
	c := newTurnClock(context.Background(), 30*time.Millisecond, 150*time.Millisecond)
	defer c.Stop()

	waitUntil(t, func() bool { return m.spent.Load() == 1 }, "the spent mark")
	if c.ctx.Err() != nil {
		t.Fatal("the budget running out cancelled the turn; it must only warn")
	}
	if m.cut.Load() != 0 {
		t.Fatal("cut before the grace ran out")
	}
	<-c.ctx.Done()
	var cut dun.BudgetCut
	if !errors.As(context.Cause(c.ctx), &cut) || cut.Budget != 30*time.Millisecond || cut.Grace != 150*time.Millisecond {
		t.Fatalf("cause = %v, want the budget cut", context.Cause(c.ctx))
	}
	if m.spent.Load() != 1 || m.cut.Load() != 1 {
		t.Fatalf("marks: spent %d, cut %d; want one each", m.spent.Load(), m.cut.Load())
	}
}

// A paused clock (ask_user, compaction) reaches neither mark, in the budget or
// in the grace; resumed, it reaches them.
func TestTurnClock_PausedDoesNotExpire(t *testing.T) {
	m := watchMarks(t)
	c := newTurnClock(context.Background(), 30*time.Millisecond, 300*time.Millisecond)
	defer c.Stop()

	c.Pause()
	time.Sleep(120 * time.Millisecond)
	if m.spent.Load() != 0 || c.ctx.Err() != nil {
		t.Fatal("a paused budget expired")
	}
	c.Resume()
	waitUntil(t, func() bool { return m.spent.Load() == 1 }, "the spent mark")

	c.Pause() // now in the grace
	time.Sleep(120 * time.Millisecond)
	if m.cut.Load() != 0 || c.ctx.Err() != nil {
		t.Fatal("a paused grace cut the turn")
	}
	c.Resume()
	<-c.ctx.Done()
	if m.spent.Load() != 1 || m.cut.Load() != 1 {
		t.Fatalf("marks: spent %d, cut %d; want one each", m.spent.Load(), m.cut.Load())
	}
}

// A turn that ends before its budget reaches no mark.
func TestTurnClock_StopBeforeBudget(t *testing.T) {
	m := watchMarks(t)
	c := newTurnClock(context.Background(), 20*time.Millisecond, 20*time.Millisecond)
	c.Stop()
	time.Sleep(80 * time.Millisecond)
	if m.spent.Load() != 0 || m.cut.Load() != 0 {
		t.Fatal("a stopped clock reached a mark")
	}
}

func TestGraceFor(t *testing.T) {
	defer func() { turnGrace = 0 }()
	for _, tc := range []struct {
		budget, flag, want time.Duration
	}{
		{30 * time.Minute, 0, 6 * time.Minute},
		{5 * time.Minute, 0, 2 * time.Minute}, // floor
		{2 * time.Hour, 0, 10 * time.Minute},  // ceiling
		{30 * time.Minute, 45 * time.Second, 45 * time.Second},
		{30 * time.Minute, -1, 0}, // none
		{0, 0, 0},                 // no budget, no grace
	} {
		turnGrace = tc.flag
		if got := graceFor(tc.budget); got != tc.want {
			t.Errorf("graceFor(%s) with --timeout-grace %s = %s, want %s", tc.budget, tc.flag, got, tc.want)
		}
	}
}

// A budget cut arrives from the harness as ErrInterrupted, and is reported as
// the budget; a user's interrupt on a live turn stays an interrupt.
func TestTurnErr_BudgetCutIsNotTheUser(t *testing.T) {
	tctx, cancel := context.WithCancelCause(context.Background())
	cancel(dun.BudgetCut{Budget: 30 * time.Minute, Grace: 6 * time.Minute})
	got := turnErr(tctx, dun.ErrInterrupted)
	if errors.Is(got, dun.ErrInterrupted) || !strings.Contains(got.Error(), "30m budget plus 6m grace") {
		t.Fatalf("got %v, want the budget named", got)
	}
	if got := turnErr(context.Background(), dun.ErrInterrupted); !errors.Is(got, dun.ErrInterrupted) {
		t.Fatalf("a user's interrupt became %v", got)
	}
}

// The grace starting is one dim line in the TUI, not a bell.
func TestTUI_BudgetSpentLine(t *testing.T) {
	m := newTUIModel(&dunProc{}, "/ws")
	m = m.handleEvent(evMsg{"type": "notification", "kind": "budget",
		"text": "turn budget 30m spent — the model is told to wrap up; cut in 6m"})
	var all strings.Builder
	for _, e := range m.convo {
		all.WriteString(e.view())
	}
	if got := all.String(); !strings.Contains(got, "⏱") || !strings.Contains(got, "cut in 6m") || strings.Contains(got, "🔔") {
		t.Fatalf("budget line: %q", got)
	}
}
