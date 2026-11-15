package tui

import (
	"encoding/json"
	"slices"
	"time"

	"github.com/iodesystems/dun/proto"
)

// The tests play the engine: after each event a test feeds, the TUI gets
// the turn_state the engine would send next (proto.StepTurnState, the
// engine's own rule, and the queue as the harness keeps it). One per
// model, by its proc.
var simTurns = map[*dunProc]*proto.TurnState{}

func simTurn(p *dunProc) *proto.TurnState {
	st := simTurns[p]
	if st == nil {
		st = &proto.TurnState{Phase: proto.PhaseIdle, Queued: []proto.QueuedMsg{}}
		simTurns[p] = st
	}
	return st
}

// feed applies ev, then the turn_state that follows it, if it changed.
func (m tuiModel) feed(ev evMsg) tuiModel {
	syncSim(m)
	m = m.handleEvent(ev)
	if ts := simAfter(m.proc, ev); ts != nil {
		m = m.handleEvent(ts)
	}
	return m
}

// simAfter is the turn_state the engine sends after ev, or nil.
func simAfter(proc *dunProc, ev evMsg) evMsg {
	st := simTurn(proc)
	b, _ := json.Marshal(map[string]any(ev))
	var pe proto.Event
	if json.Unmarshal(b, &pe) != nil {
		return nil
	}
	if p, ok := pe.Payload.(*proto.TurnState); ok { // the test says it itself
		*st = *p
		return nil
	}
	before, _ := json.Marshal(st)
	proto.StepTurnState(st, pe.Payload, time.Now())
	switch p := pe.Payload.(type) {
	case *proto.UserEcho:
		if p.Queued && p.ID != "" {
			st.Queued = append(st.Queued, proto.QueuedMsg{ID: p.ID, Text: p.Content})
		}
	case *proto.Delivered:
		st.Queued = slices.DeleteFunc(st.Queued, func(q proto.QueuedMsg) bool { return slices.Contains(p.IDs, q.ID) })
	case *proto.Withdrawn:
		st.Queued = slices.DeleteFunc(st.Queued, func(q proto.QueuedMsg) bool { return slices.Contains(p.IDs, q.ID) })
	}
	after, _ := json.Marshal(st)
	if string(before) == string(after) {
		return nil
	}
	var ts map[string]any
	_ = json.Unmarshal(after, &ts)
	ts["type"] = "turn_state"
	return evMsg(ts)
}

// syncSim: a test that set busy itself has a turn running, as far as the
// engine it plays is concerned.
func syncSim(m tuiModel) {
	if st := simTurn(m.proc); m.busy && st.Phase == proto.PhaseIdle {
		st.Phase = proto.PhaseWorking
	}
}
