package acp

import (
	"context"
	"testing"
	"time"

	sdk "github.com/coder/acp-go-sdk"

	"github.com/iodesystems/dun/proto"
)

// restartRig is a rig with a session whose prompt "go" is mid-turn, on a
// log of epoch e1; subscribes reports each subscribe.
func restartRig(t *testing.T) (*rig, sdk.SessionId, chan promptResult) {
	t.Helper()
	r := newRig(t)
	r.d.epoch = "e1"
	r.d.subscribes = make(chan proto.Subscribe, 8)
	sid := r.newSession()
	<-r.d.subscribes
	ch := r.prompt(sid, "go")
	r.d.next()
	r.d.emit(&proto.UserEcho{ID: "m1", Content: "go"}, &proto.Turn{Kind: "user"}, &proto.Token{Text: "working"})
	return r, sid, ch
}

// resubscribed waits for the bridge's subscribe after a restart, and for
// its snapshot to be read.
func resubscribed(t *testing.T, r *rig) proto.Subscribe {
	t.Helper()
	select {
	case s := <-r.d.subscribes:
		time.Sleep(50 * time.Millisecond) // the snapshot is handled
		return s
	case <-time.After(5 * time.Second):
		t.Fatal("the bridge did not subscribe again")
	}
	return proto.Subscribe{}
}

func endedTurn(t *testing.T, ch chan promptResult) {
	t.Helper()
	if res := waitPrompt(t, ch); res.stop != sdk.StopReasonEndTurn || res.err != nil {
		t.Fatalf("prompt: %+v", res)
	}
}

// dund restarts (an upgrade) while a prompt's turn runs, and the turn's
// done is lost with the link. The new log shows the engine idle: the
// prompt is answered end_turn — from the snapshot's turn_state, from a
// new engine that has none yet, or from a turn_state that comes later
// (an adopted engine whose state the new log does not have yet).
func TestRestartIdleAnswersPrompt(t *testing.T) {
	idle := &proto.TurnState{Phase: proto.PhaseIdle, Queued: []proto.QueuedMsg{}}
	t.Run("turn_state in the snapshot", func(t *testing.T) {
		r, _, ch := restartRig(t)
		r.d.restart("e2", nil, &proto.EngineState{State: "started", Reason: "adopted"}, idle)
		resubscribed(t, r)
		endedTurn(t, ch)
	})
	t.Run("no turn_state, new engine", func(t *testing.T) {
		r, _, ch := restartRig(t)
		r.d.restart("e2", nil, &proto.EngineState{State: "started"})
		resubscribed(t, r)
		endedTurn(t, ch)
	})
	t.Run("no engine", func(t *testing.T) {
		r, _, ch := restartRig(t)
		r.d.restart("e2", nil)
		resubscribed(t, r)
		endedTurn(t, ch)
	})
	t.Run("turn_state later", func(t *testing.T) {
		r, _, ch := restartRig(t)
		r.d.restart("e2", nil, &proto.EngineState{State: "started", Reason: "adopted"})
		resubscribed(t, r)
		pending(t, ch, "adopted engine, state unknown")
		r.d.emit(&proto.EngineState{State: "stopped"}, idle)
		endedTurn(t, ch)
	})
	t.Run("adopted engine still working", func(t *testing.T) {
		r, _, ch := restartRig(t)
		r.d.restart("e2", nil, &proto.EngineState{State: "started", Reason: "adopted"},
			&proto.TurnState{Phase: proto.PhaseWorking, Queued: []proto.QueuedMsg{}})
		resubscribed(t, r)
		pending(t, ch, "working")
		r.d.emit(&proto.Token{Text: " more"}, &proto.Done{}, idle)
		endedTurn(t, ch)
	})
}

// A turn_state idle that is not after a gap (the one after a turn's done)
// ends nothing by itself: a prompt sent after it waits for its own turn.
func TestIdleTurnStateNoGap(t *testing.T) {
	r, _, ch := restartRig(t)
	r.d.emit(&proto.TurnState{Phase: proto.PhaseIdle, Queued: []proto.QueuedMsg{}})
	pending(t, ch, "idle turn_state, no gap")
	r.d.emit(&proto.Done{})
	endedTurn(t, ch)
}

// The bridge follows the new log's epoch from its snapshot: the next
// subscribe after another drop names it, with the new log's seq.
func TestRestartTracksEpoch(t *testing.T) {
	r, _, _ := restartRig(t)
	r.d.restart("e2", nil, &proto.EngineState{State: "started", Reason: "adopted"})
	if s := resubscribed(t, r); s.Epoch != "e1" || s.After == 0 {
		t.Fatalf("first resubscribe %+v, want e1 after the last event seen", s)
	}
	r.d.restart("e3", nil, &proto.EngineState{State: "started", Reason: "adopted"})
	if s := resubscribed(t, r); s.Epoch != "e2" || s.After != 0 {
		t.Fatalf("second resubscribe %+v, want e2 after 0", s)
	}
}

// A question suspended by the restart keeps its prompt held while the new
// log shows the question still open (its call has no result); a history
// whose call has its result (answered while the bridge was away) ends it.
func TestRestartSuspended(t *testing.T) {
	ask := &proto.Ask{AskID: "a1", CallID: "c1", Question: "which?", Options: []string{"x", "y"}}
	idle := &proto.TurnState{Phase: proto.PhaseIdle, Queued: []proto.QueuedMsg{}}
	call := proto.HistoryItem{Kind: "tool_call", CallID: "c1", Tool: "ask_user"}
	suspend := func(t *testing.T) (*rig, chan promptResult) {
		r, _, ch := restartRig(t)
		r.ed.asked = make(chan sdk.RequestPermissionRequest, 4)
		r.d.emit(ask)
		<-r.ed.asked
		r.d.emit(&proto.Done{Reason: "suspended"})
		pending(t, ch, "suspended")
		return r, ch
	}
	t.Run("still open", func(t *testing.T) {
		r, ch := suspend(t)
		r.d.restart("e2", []proto.HistoryItem{call}, &proto.EngineState{State: "started"}, idle)
		resubscribed(t, r)
		pending(t, ch, "the question is still open")
		r.d.emit(&proto.Turn{Kind: "continue"}, ask)
		pending(t, ch, "asked again")
		r.d.emit(&proto.Done{})
		endedTurn(t, ch)
	})
	t.Run("answered meanwhile", func(t *testing.T) {
		r, ch := suspend(t)
		r.d.restart("e2", []proto.HistoryItem{call, {Kind: "tool_result", CallID: "c1", Content: "x"}, {Kind: "assistant", Content: "done"}},
			&proto.EngineState{State: "started"}, idle)
		resubscribed(t, r)
		endedTurn(t, ch)
	})
}

// A prompt held for a stop condition's run (untilHeld) is answered when
// the new log shows the run stopped and the engine idle.
func TestRestartUntilHeld(t *testing.T) {
	r, _, ch := restartRig(t)
	conds := []proto.UntilCond{{Kind: proto.UntilTurns, N: 3}}
	r.d.emit(&proto.Until{Conds: conds, Cap: 3, Turn: 1, Running: true}, &proto.Done{})
	pending(t, ch, "the run goes on")
	idle := &proto.TurnState{Phase: proto.PhaseIdle, Queued: []proto.QueuedMsg{}}
	t.Run("running", func(t *testing.T) {
		r.d.restart("e2", nil, &proto.EngineState{State: "started", Reason: "adopted"},
			&proto.Until{Conds: conds, Cap: 3, Turn: 1, Running: true}, idle)
		resubscribed(t, r)
		pending(t, ch, "the run goes on after the restart")
	})
	t.Run("stopped", func(t *testing.T) {
		r.d.restart("e3", nil, &proto.EngineState{State: "started", Reason: "adopted"},
			&proto.Until{Conds: conds, Cap: 3, Turn: 1, Stopped: "cap"}, idle)
		resubscribed(t, r)
		endedTurn(t, ch)
	})
}

// session/cancel always answers: at once when the engine says nothing
// runs, and after cancelWait when no done comes.
func TestCancelHeldPrompt(t *testing.T) {
	old := cancelWait
	cancelWait = 300 * time.Millisecond
	t.Cleanup(func() { cancelWait = old })
	cancel := func(t *testing.T, r *rig, sid sdk.SessionId) {
		t.Helper()
		if err := r.cc.Cancel(context.Background(), sdk.CancelNotification{SessionId: sid}); err != nil {
			t.Fatal(err)
		}
		if c := r.d.next(); c.Type != "interrupt" {
			t.Fatalf("want an interrupt, got %s", c.Type)
		}
	}
	cancelled := func(t *testing.T, ch chan promptResult, within time.Duration) {
		t.Helper()
		select {
		case res := <-ch:
			if res.stop != sdk.StopReasonCancelled || res.err != nil {
				t.Fatalf("prompt: %+v", res)
			}
		case <-time.After(within):
			t.Fatal("the cancel was not answered")
		}
	}
	t.Run("nothing to interrupt", func(t *testing.T) {
		r, sid, ch := restartRig(t)
		cancel(t, r, sid)
		r.d.emit(&proto.ControlEvent{ID: "interrupt", Message: "nothing to interrupt: no turn is running"})
		cancelled(t, ch, 200*time.Millisecond)
	})
	t.Run("no done", func(t *testing.T) {
		r, sid, ch := restartRig(t)
		cancel(t, r, sid)
		pending(t, ch, "before the bound")
		cancelled(t, ch, 2*time.Second)
	})
	t.Run("suspended", func(t *testing.T) {
		r, sid, ch := restartRig(t)
		r.d.emit(&proto.Done{Reason: "suspended"})
		cancel(t, r, sid)
		cancelled(t, ch, 2*time.Second)
	})
}
