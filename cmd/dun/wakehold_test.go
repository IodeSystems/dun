package main

import "testing"

func drainHoldPoke() {
	select {
	case <-wakeHoldChanged:
	default:
	}
}

// dund's upgrade hold (proto.UpgradeHold): held, the engine's own wakes do
// not start a turn, except the one pass a waking service notice gives; the
// hold's end lets them go and pokes the loop.
func TestWakeHold(t *testing.T) {
	t.Cleanup(func() { wakeHoldGen.Store(0); setWakeHold(false, 0); drainHoldPoke() })
	if !mayWake() {
		t.Fatal("not held, yet no wake")
	}
	setWakeHold(true, 0)
	<-wakeHoldChanged
	if mayWake() {
		t.Fatal("held, yet woke")
	}
	wakePass.Store(true)
	if !mayWake() || mayWake() {
		t.Fatal("the pass is one wake")
	}
	setWakeHold(false, 0)
	select {
	case <-wakeHoldChanged:
	default:
		t.Fatal("the hold's end did not poke the loop")
	}
	if !mayWake() {
		t.Fatal("released, yet no wake")
	}
}

// An older hold arriving after a newer one is ignored: an engine started as
// an upgrade failed got the failure's Off (gen 2) before its own On (gen 1).
func TestWakeHoldIgnoresAnOlderGeneration(t *testing.T) {
	t.Cleanup(func() { wakeHoldGen.Store(0); setWakeHold(false, 0); drainHoldPoke() })
	setWakeHold(false, 2)
	setWakeHold(true, 1)
	if wakeHold.Load() {
		t.Fatal("a stale On held the engine")
	}
	setWakeHold(true, 3) // the next upgrade
	if !wakeHold.Load() {
		t.Fatal("a newer On was ignored")
	}
}
