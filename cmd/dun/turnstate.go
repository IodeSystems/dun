package main

import (
	"sync"
	"time"

	"github.com/iodesystems/dun/internal/harness"
	"github.com/iodesystems/dun/proto"
)

// turnTracker keeps the session's turn_state (proto.TurnState) and sends it
// on every change. The engine owns it: it alone knows a turn is running, a
// question is open, an interrupt is under way, a provider wait is on, and
// what is queued. dund sends idle itself when the engine stops or crashes,
// since nothing of the turn survives that (internal/dund/session.go).
//
// It reads the engine's own events as they are written (emitter.emit calls
// observe), so no path can change the turn without the state following: a
// client is told the state by the same events it would otherwise have had
// to infer it from.
type turnTracker struct {
	mu  sync.Mutex
	st  proto.TurnState
	em  *emitter
	h   func() *harness.Harness // nil until the harness exists
	now func() time.Time
}

func newTurnTracker(em *emitter, h func() *harness.Harness) *turnTracker {
	return &turnTracker{st: proto.TurnState{Phase: proto.PhaseIdle, Queued: []proto.QueuedMsg{}},
		em: em, h: h, now: time.Now}
}

// callID is the tool call running now ("" between calls, or nil tracker).
func (t *turnTracker) callID() string {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.st.CallID
}

// interrupting: an interrupt reached a running turn; its done ends it.
func (t *turnTracker) interrupting() {
	t.change(false, func(st *proto.TurnState) {
		if st.Phase != proto.PhaseIdle {
			st.Phase, st.Retry, st.Discard = proto.PhaseInterrupting, nil, false
		}
	})
}

// observe moves the state for an event the engine just wrote.
func (t *turnTracker) observe(payload any) {
	queue, ok := affectsTurn(payload)
	if !ok {
		return
	}
	t.change(queue, func(st *proto.TurnState) { proto.StepTurnState(st, payload, t.now()) })
}

// change applies f and sends the state if it changed. queue: read the
// harness's queue again (an event about the queue, or a turn's edge).
func (t *turnTracker) change(queue bool, f func(*proto.TurnState)) {
	t.mu.Lock()
	defer t.mu.Unlock() // held over the emit: turn_state events go out in order
	before := cloneTurnState(t.st)
	f(&t.st)
	if queue && t.h != nil {
		if h := t.h(); h != nil {
			t.st.Queued = queuedProto(h.QueuedUser())
			t.st.NotesWaiting = h.NotesWaiting()
			t.st.NetWait = netWaitProto(h.NetWait())
		}
	}
	if turnStateEqual(before, t.st) {
		return
	}
	ev := cloneTurnState(t.st)
	t.em.emit(&ev)
}

// affectsTurn: whether stepTurnState reads the event, and whether the queue
// is read again after it. Tokens are read only to end a provider wait the
// stream outlived.
func affectsTurn(payload any) (queue, ok bool) {
	switch payload.(type) {
	case *proto.Turn, *proto.Done, *proto.Error, *proto.Queued, *proto.UserEcho,
		*proto.Delivered, *proto.Withdrawn:
		return true, true
	case *proto.Ask, *proto.Mount, *proto.Answered, *proto.Retry, *proto.ToolCall,
		*proto.ToolResult, *proto.Token, *proto.Reasoning:
		return false, true
	}
	return false, false
}

// netWaitChanged sends the state again: the wait for the network started,
// moved its next probe, or ended (harness OnNetWait).
func (t *turnTracker) netWaitChanged() {
	if t != nil {
		t.change(true, func(*proto.TurnState) {})
	}
}

func netWaitProto(w harness.NetWaitInfo) *proto.NetWait {
	if !w.Waiting() {
		return nil
	}
	return &proto.NetWait{Since: w.Since.UnixMilli(), Next: w.Next.UnixMilli()}
}

func netWaitEqual(a, b *proto.NetWait) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func queuedProto(qs []harness.QueuedMsg) []proto.QueuedMsg {
	out := make([]proto.QueuedMsg, 0, len(qs))
	for _, q := range qs {
		out = append(out, proto.QueuedMsg{ID: q.ID, Text: q.Text})
	}
	return out
}

func cloneTurnState(s proto.TurnState) proto.TurnState {
	s.Queued = append([]proto.QueuedMsg{}, s.Queued...)
	return s
}

// turnStateEqual: an Ask, Mount or Retry is a new pointer per event, so a
// new one always counts as a change.
func turnStateEqual(a, b proto.TurnState) bool {
	if a.Phase != b.Phase || a.Since != b.Since || a.Tool != b.Tool || a.CallID != b.CallID ||
		a.Ask != b.Ask || a.Mount != b.Mount || a.Retry != b.Retry || a.Discard != b.Discard ||
		a.NotesWaiting != b.NotesWaiting || !netWaitEqual(a.NetWait, b.NetWait) || len(a.Queued) != len(b.Queued) {
		return false
	}
	for i := range a.Queued {
		if a.Queued[i] != b.Queued[i] {
			return false
		}
	}
	return true
}
