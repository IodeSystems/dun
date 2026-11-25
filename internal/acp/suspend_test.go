package acp

import (
	"testing"
	"time"

	sdk "github.com/coder/acp-go-sdk"

	"github.com/iodesystems/dun/proto"
)

// The engine's stop with a question open (done suspended, a restart or a
// model switch) is not the turn's end: the prompt stays held, the open form
// stays open, and the same ask coming again raises no second form. The
// answer goes on with the continuation turn, which answers the prompt once,
// with both turns' usage added up.
func TestSuspendedKeepsPromptAndForm(t *testing.T) {
	r := newRigCaps(t, formCaps)
	release := make(chan struct{})
	r.ed.elicited = make(chan sdk.CreateElicitationRequest, 4)
	r.ed.elicit = func(sdk.CreateElicitationRequest) sdk.CreateElicitationResponse {
		<-release
		return sdk.CreateElicitationResponse{Accept: &sdk.CreateElicitationAccept{Action: "accept",
			Content: map[string]sdk.ElicitationContentValue{fieldAnswer: strVal("x")}}}
	}
	sid := r.newSession()
	ch := r.prompt(sid, "go")
	r.d.next()
	ask := &proto.Ask{AskID: "a1", CallID: "c1", Question: "which?", Options: []string{"x", "y"}}
	r.d.emit(&proto.UserEcho{ID: "m1", Content: "go"}, &proto.Turn{Kind: "user"}, ask, usageEvent(2100, 0.25, "USD"))
	<-r.ed.elicited
	r.d.emit(&proto.Done{Reason: "suspended"})
	pending(t, ch, "suspended")

	// The next engine asks again (and turn_state repeats it): the form is
	// still open, so no second one.
	r.d.emit(&proto.Turn{Kind: "continue"}, ask, &proto.TurnState{Phase: proto.PhaseWaitingAnswer, Ask: ask})
	select {
	case <-r.ed.elicited:
		t.Fatal("a second form for the same question")
	case <-time.After(200 * time.Millisecond):
	}
	pending(t, ch, "asked again")

	close(release)
	if a := answerSent(t, r); a.Value != "x" || a.AskID != "a1" {
		t.Fatalf("answer %+v", a)
	}
	pending(t, ch, "answered, turn not over")
	r.d.emit(&proto.Done{}) // no usage in this one: the first turn's alone
	res := waitPrompt(t, ch)
	if res.stop != sdk.StopReasonEndTurn || res.err != nil {
		t.Fatalf("response %+v", res)
	}
}

// Usage of both turns adds up in the one response.
func TestSuspendedUsageSums(t *testing.T) {
	r := newRig(t)
	r.ed.asked = make(chan sdk.RequestPermissionRequest, 4)
	r.ed.perm = func(sdk.RequestPermissionRequest) sdk.RequestPermissionResponse {
		return sdk.RequestPermissionResponse{Outcome: sdk.NewRequestPermissionOutcomeSelected("0")}
	}
	sid := r.newSession()
	done := make(chan sdk.PromptResponse, 1)
	go func() {
		res, err := r.cc.Prompt(t.Context(), sdk.PromptRequest{SessionId: sid, Prompt: []sdk.ContentBlock{sdk.TextBlock("go")}})
		if err != nil {
			t.Error(err)
		}
		done <- res
	}()
	r.d.next()
	ask := &proto.Ask{AskID: "a1", Question: "which?", Options: []string{"x", "y"}}
	r.d.emit(&proto.UserEcho{ID: "m1", Content: "go"}, &proto.Turn{Kind: "user"}, usageEvent(1000, 0.1, "USD"), &proto.Done{Reason: "suspended"})
	select {
	case <-done:
		t.Fatal("suspended answered the prompt")
	case <-time.After(150 * time.Millisecond):
	}
	r.d.emit(&proto.Turn{Kind: "continue"}, ask)
	<-r.ed.asked
	if a := answerSent(t, r); a.Value != "x" {
		t.Fatalf("answer %+v", a)
	}
	r.d.emit(usageEvent(2000, 0.2, "USD"), &proto.Done{})
	select {
	case res := <-done:
		if res.StopReason != sdk.StopReasonEndTurn || res.Usage == nil || res.Usage.InputTokens != 3000 || res.Usage.OutputTokens != 400 {
			t.Fatalf("response %+v usage %+v", res, res.Usage)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the prompt was not answered")
	}
	select {
	case <-r.ed.asked:
		t.Error("the question was raised twice")
	default:
	}
}

// A request the editor dropped (it cancelled the permission request) is
// raised again when the next engine asks the question again.
func TestSuspendedReaskAfterDrop(t *testing.T) {
	r := newRig(t)
	r.ed.asked = make(chan sdk.RequestPermissionRequest, 4)
	n := 0
	r.ed.perm = func(sdk.RequestPermissionRequest) sdk.RequestPermissionResponse {
		n++
		if n == 1 {
			return sdk.RequestPermissionResponse{Outcome: sdk.NewRequestPermissionOutcomeCancelled()}
		}
		return sdk.RequestPermissionResponse{Outcome: sdk.NewRequestPermissionOutcomeSelected("1")}
	}
	sid := r.newSession()
	ch := r.prompt(sid, "go")
	r.d.next()
	ask := &proto.Ask{AskID: "a1", Question: "which?", Options: []string{"x", "y"}}
	r.d.emit(&proto.UserEcho{ID: "m1", Content: "go"}, &proto.Turn{Kind: "user"}, ask)
	<-r.ed.asked
	time.Sleep(100 * time.Millisecond) // the cancelled reply is read
	r.d.emit(&proto.Done{Reason: "suspended"})
	pending(t, ch, "suspended")
	r.d.emit(&proto.Turn{Kind: "continue"}, ask)
	select {
	case <-r.ed.asked:
	case <-time.After(5 * time.Second):
		t.Fatal("the dropped question was not raised again")
	}
	if a := answerSent(t, r); a.Value != "y" || a.AskID != "a1" {
		t.Fatalf("answer %+v", a)
	}
	r.d.emit(&proto.Done{})
	if res := waitPrompt(t, ch); res.stop != sdk.StopReasonEndTurn {
		t.Fatalf("response %+v", res)
	}
}

// A snapshot that shows the session idle (the link came back while the
// engine restarts) does not end a suspended prompt.
func TestSuspendedSnapshotIdle(t *testing.T) {
	r := newRig(t)
	r.ed.asked = make(chan sdk.RequestPermissionRequest, 4)
	sid := r.newSession()
	ch := r.prompt(sid, "go")
	r.d.next()
	ask := &proto.Ask{AskID: "a1", Question: "which?", Options: []string{"x", "y"}}
	r.d.emit(&proto.UserEcho{ID: "m1", Content: "go"}, &proto.Turn{Kind: "user"}, ask, &proto.Done{Reason: "suspended"})
	<-r.ed.asked
	r.d.emit(&proto.Snapshot{State: []proto.Event{{Type: "turn_state", Payload: &proto.TurnState{Phase: proto.PhaseIdle}}}})
	pending(t, ch, "idle snapshot while suspended")
}

// A model switch while a question is open is allowed (the restart suspends
// the question); while the model generates it is refused.
func TestSetModelWithOpenQuestion(t *testing.T) {
	r, res := configRig(t)
	r.ed.asked = make(chan sdk.RequestPermissionRequest, 4)
	ch := r.prompt(res.SessionId, "work")
	r.d.next()
	r.d.emit(&proto.UserEcho{ID: "m1", Content: "work"}, &proto.Turn{Kind: "user"})
	time.Sleep(100 * time.Millisecond)
	if _, err := set(r, res.SessionId, optModel, "b/x"); err == nil {
		t.Fatal("a model switch while the model generates")
	}
	r.d.emit(&proto.Ask{AskID: "a1", Question: "which?", Options: []string{"x", "y"}})
	<-r.ed.asked
	sw := make(chan error, 1)
	go func() { _, err := set(r, res.SessionId, optModel, "b/x"); sw <- err }()
	c := r.d.next()
	if _, ok := c.Payload.(*proto.SwitchModel); !ok {
		t.Fatalf("sent %s %+v", c.Type, c.Payload)
	}
	r.d.emit(&proto.Settings{Model: "x", URL: "http://b", Provider: "b", Effort: "high"}, &proto.Done{Reason: "suspended"})
	select {
	case err := <-sw:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the switch was not answered")
	}
	pending(t, ch, "the switch suspends, it does not end the prompt")
}
