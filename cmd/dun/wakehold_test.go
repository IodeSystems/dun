package main

import "testing"

// dund's upgrade hold (proto.UpgradeHold): held, the engine's own wakes do
// not start a turn, except the one pass a waking service notice gives; the
// hold's end lets them go and pokes the loop.
func TestWakeHold(t *testing.T) {
	t.Cleanup(func() {
		setWakeHold(false)
		select {
		case <-wakeHoldChanged:
		default:
		}
	})
	if !mayWake() {
		t.Fatal("not held, yet no wake")
	}
	setWakeHold(true)
	<-wakeHoldChanged
	if mayWake() {
		t.Fatal("held, yet woke")
	}
	wakePass.Store(true)
	if !mayWake() || mayWake() {
		t.Fatal("the pass is one wake")
	}
	setWakeHold(false)
	select {
	case <-wakeHoldChanged:
	default:
		t.Fatal("the hold's end did not poke the loop")
	}
	if !mayWake() {
		t.Fatal("released, yet no wake")
	}
}
