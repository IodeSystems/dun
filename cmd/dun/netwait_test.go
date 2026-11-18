package main

import (
	"testing"
	"time"

	"github.com/iodesystems/dun/internal/harness"
	"github.com/iodesystems/dun/proto"
)

// The wait for the network is in turn_state: nil when not waiting, the
// two instants in ms, compared by value (a new pointer each read), and
// kept across the turn's edges (proto.StepTurnState's idle).
func TestTurnStateNetWait(t *testing.T) {
	if netWaitProto(harness.NetWaitInfo{}) != nil {
		t.Error("net_wait set with no wait")
	}
	w := netWaitProto(harness.NetWaitInfo{Since: time.UnixMilli(1000), Next: time.UnixMilli(31000)})
	if w == nil || w.Since != 1000 || w.Next != 31000 {
		t.Fatalf("net_wait %+v", w)
	}
	a := proto.TurnState{Phase: proto.PhaseIdle, NetWait: w}
	b := cloneTurnState(a)
	b.NetWait = &proto.NetWait{Since: 1000, Next: 31000}
	if !turnStateEqual(a, b) {
		t.Error("the same wait read twice counts as a change")
	}
	b.NetWait = &proto.NetWait{Since: 1000, Next: 61000}
	if turnStateEqual(a, b) {
		t.Error("a moved next probe is not a change")
	}
	b.NetWait = nil
	if turnStateEqual(a, b) {
		t.Error("the wait's end is not a change")
	}
	st := a
	proto.StepTurnState(&st, &proto.Done{}, time.UnixMilli(2000))
	if st.NetWait == nil {
		t.Error("idle dropped net_wait")
	}
}
