package acp

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	sdk "github.com/coder/acp-go-sdk"

	"github.com/iodesystems/dun/proto"
)

var formCaps = sdk.ClientCapabilities{Elicitation: &sdk.ElicitationCapabilities{Form: &sdk.ElicitationFormCapabilities{}}}

// askVia starts a turn on a rig whose editor takes forms, asks a, and
// returns the form the editor got.
func askVia(t *testing.T, r *rig, a *proto.Ask) sdk.CreateElicitationRequest {
	t.Helper()
	if r.ed.elicited == nil {
		r.ed.elicited = make(chan sdk.CreateElicitationRequest, 1)
	}
	sid := r.newSession()
	r.prompt(sid, "go")
	r.d.next()
	r.d.emit(&proto.UserEcho{ID: "m1", Content: "go"}, &proto.Turn{Kind: "user"},
		a, &proto.TurnState{Phase: proto.PhaseWaitingAnswer, Ask: a})
	select {
	case q := <-r.ed.elicited:
		return q
	case <-time.After(5 * time.Second):
		t.Fatal("no elicitation/create")
	}
	return sdk.CreateElicitationRequest{}
}

func answerSent(t *testing.T, r *rig) *proto.Answer {
	t.Helper()
	c := r.d.next()
	a, ok := c.Payload.(*proto.Answer)
	if !ok {
		t.Fatalf("want an answer, got %s %+v", c.Type, c.Payload)
	}
	return a
}

func accept(content map[string]sdk.ElicitationContentValue) func(sdk.CreateElicitationRequest) sdk.CreateElicitationResponse {
	return func(sdk.CreateElicitationRequest) sdk.CreateElicitationResponse {
		return sdk.CreateElicitationResponse{Accept: &sdk.CreateElicitationAccept{Action: "accept", Content: content}}
	}
}

func strVal(s string) sdk.ElicitationContentValue {
	v := sdk.ElicitationContentValueString(s)
	return sdk.ElicitationContentValue{String: &v}
}

// One answer of options: a select with the recommended default, and the
// custom-answer field with Air's _meta, on the tool call that asked; the
// pick is sent as the answer.
func TestElicitSelect(t *testing.T) {
	r := newRigCaps(t, formCaps)
	r.ed.elicit = accept(map[string]sdk.ElicitationContentValue{fieldAnswer: strVal("x")})
	q := askVia(t, r, &proto.Ask{AskID: "a1", CallID: "c1", Question: "which?", Options: []string{"x", "y"}, Recommended: "y"})
	f := q.Form
	if f == nil || f.Message != "which?" || f.Session == nil || f.Session.ToolCallId == nil || *f.Session.ToolCallId != "c1" {
		t.Fatalf("form %+v", f)
	}
	ans := f.RequestedSchema.Properties[fieldAnswer].String
	if ans == nil || !slices.Equal(ans.Enum, []string{"x", "y"}) || ans.Default == nil || *ans.Default != "y" {
		t.Fatalf("answer field %+v", ans)
	}
	other := f.RequestedSchema.Properties[fieldOther].String
	if other == nil || string(other.Meta["_askUserQuestionCustomAnswer"]) != "true" || len(f.RequestedSchema.Required) != 0 {
		t.Fatalf("other field %+v required %v", other, f.RequestedSchema.Required)
	}
	b, _ := json.Marshal(q)
	if !strings.Contains(string(b), `"mode":"form"`) || !strings.Contains(string(b), `"sessionId":"s1"`) || !strings.Contains(string(b), `"customAnswer":true`) {
		t.Errorf("wire %s", b)
	}
	if a := answerSent(t, r); a.Value != "x" || a.AskID != "a1" {
		t.Fatalf("answer %+v", a)
	}
}

// The custom answer wins over the select.
func TestElicitOther(t *testing.T) {
	r := newRigCaps(t, formCaps)
	r.ed.elicit = accept(map[string]sdk.ElicitationContentValue{fieldAnswer: strVal("x"), fieldOther: strVal("neither")})
	askVia(t, r, &proto.Ask{AskID: "a1", Question: "which?", Options: []string{"x", "y"}})
	if a := answerSent(t, r); a.Value != "neither" {
		t.Fatalf("answer %+v", a)
	}
}

// Multiple choice: an array of the options; the checked ones and the words,
// joined as the TUI joins them.
func TestElicitMulti(t *testing.T) {
	r := newRigCaps(t, formCaps)
	picked := sdk.ElicitationContentValueStringArray{"x", "z"}
	r.ed.elicit = accept(map[string]sdk.ElicitationContentValue{fieldAnswer: {StringArray: &picked}, fieldOther: strVal("w")})
	q := askVia(t, r, &proto.Ask{AskID: "a1", Question: "which ones?", Options: []string{"x", "y", "z"}, Multi: true, RecommendedAll: []string{"y"}})
	arr := q.Form.RequestedSchema.Properties[fieldAnswer].Array
	if arr == nil || arr.Items.String == nil || len(arr.Items.String.Enum) != 3 || !slices.Equal(arr.Default, []string{"y"}) {
		t.Fatalf("array field %+v", arr)
	}
	if a := answerSent(t, r); a.Value != "x, z, w" {
		t.Fatalf("answer %+v", a)
	}
}

// No options: one required text field.
func TestElicitText(t *testing.T) {
	r := newRigCaps(t, formCaps)
	r.ed.elicit = accept(map[string]sdk.ElicitationContentValue{fieldAnswer: strVal("blue")})
	q := askVia(t, r, &proto.Ask{AskID: "a1", Question: "colour?"})
	if p := q.Form.RequestedSchema.Properties; p[fieldAnswer].String == nil || p[fieldOther].String != nil ||
		!slices.Equal(q.Form.RequestedSchema.Required, []string{fieldAnswer}) {
		t.Fatalf("schema %+v", q.Form.RequestedSchema)
	}
	if a := answerSent(t, r); a.Value != "blue" {
		t.Fatalf("answer %+v", a)
	}
}

// The file-access question takes only its options: no custom field, the
// select required; decline sends its "no".
func TestElicitDecline(t *testing.T) {
	r := newRigCaps(t, formCaps)
	r.ed.elicit = func(sdk.CreateElicitationRequest) sdk.CreateElicitationResponse {
		return sdk.CreateElicitationResponse{Decline: &sdk.CreateElicitationDecline{Action: "decline"}}
	}
	q := askVia(t, r, &proto.Ask{AskID: "a1", CallID: "c1", Question: "read /etc/hosts?", Options: []string{"yes, for this session", "no"}, NoCustom: true})
	if _, ok := q.Form.RequestedSchema.Properties[fieldOther]; ok || !slices.Equal(q.Form.RequestedSchema.Required, []string{fieldAnswer}) {
		t.Fatalf("schema %+v", q.Form.RequestedSchema)
	}
	if a := answerSent(t, r); a.Value != "no" {
		t.Fatalf("answer %+v", a)
	}
}

// cancel leaves the question open as text: the next prompt answers it.
func TestElicitCancel(t *testing.T) {
	r := newRigCaps(t, formCaps)
	r.ed.elicit = func(sdk.CreateElicitationRequest) sdk.CreateElicitationResponse {
		return sdk.CreateElicitationResponse{Cancel: &sdk.CreateElicitationCancel{Action: "cancel"}}
	}
	askVia(t, r, &proto.Ask{AskID: "a1", Question: "which?", Options: []string{"x", "y"}})
	r.ed.waitSaid(t, "which?")
	second := r.prompt("s1", "y")
	if a := answerSent(t, r); a.Value != "y" || a.AskID != "a1" {
		t.Fatalf("answer %+v", a)
	}
	pending(t, second, "an answer waits for its turn")
}

// A question answered by another client withdraws the form.
func TestElicitWithdrawn(t *testing.T) {
	r := newRigCaps(t, formCaps)
	r.ed.withdrawn = make(chan struct{})
	askVia(t, r, &proto.Ask{AskID: "a1", Question: "which?", Options: []string{"x", "y"}})
	r.d.emit(&proto.Answered{AskID: "a1", Value: "x"})
	select {
	case <-r.ed.withdrawn:
	case <-time.After(5 * time.Second):
		t.Fatal("the form was not withdrawn")
	}
}

// An editor that fails elicitation/create is asked with request_permission.
func TestElicitFallsBack(t *testing.T) {
	r := newRigCaps(t, formCaps)
	r.ed.asked = make(chan sdk.RequestPermissionRequest, 1)
	r.ed.perm = func(sdk.RequestPermissionRequest) sdk.RequestPermissionResponse {
		return sdk.RequestPermissionResponse{Outcome: sdk.NewRequestPermissionOutcomeSelected("1")}
	}
	sid := r.newSession()
	r.prompt(sid, "go")
	r.d.next()
	r.d.emit(&proto.UserEcho{ID: "m1", Content: "go"}, &proto.Turn{Kind: "user"},
		&proto.Ask{AskID: "a1", Question: "which?", Options: []string{"x", "y"}})
	<-r.ed.asked
	if a := answerSent(t, r); a.Value != "y" {
		t.Fatalf("answer %+v", a)
	}
}
