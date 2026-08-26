package main

import (
	"context"
	"testing"
	"time"
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
	c := newTurnClock(parent, 100*time.Millisecond)
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
