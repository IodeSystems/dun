package dund

import (
	"testing"

	"github.com/iodesystems/dun/proto"
)

// dund relays the engine's explicit sequence: a user message is a "user"
// event carrying the message's id; a tool call is a "tool_call" with its
// call_id; the result is a "tool_result" with the same call_id; the reply is
// a "message" with its own id. A queued message lifted into a tool result
// arrives as a "delivered" event naming the ids and the call. The engine's
// wire is the contract; these tests pin what the dund stream a TUI/web
// client sees contains.

// F13 (sp_1m7j1wrb262p13019119xh, sp_1m7j1wrb25qdgzzbvc3ms8): a tool call in
// a turn reaches the dund stream as a tool_call with a call_id, and the
// call's end as a tool_result with the same call_id.
func TestRelayToolCallAndResultCarryTheSameCallID(t *testing.T) {
	svc, _, _ := newSvc(t)
	id, _ := svc.Create(alice, t.TempDir(), nil, "")
	ses, _ := svc.Get(alice, id)
	_, sub, _ := ses.log.Subscribe(ses.log.Last(), 100)
	defer sub.Close()
	if err := ses.send(proto.NewCommand("", &proto.User{Content: "toolpair"})); err != nil {
		t.Fatal(err)
	}
	u := next(t, sub, "user")
	if id := u.Payload.(*proto.UserEcho).ID; id == "" {
		t.Fatal("user event has no id")
	}
	tc := next(t, sub, "tool_call")
	callID := tc.Payload.(*proto.ToolCall).CallID
	if callID == "" {
		t.Fatal("tool_call has no call_id")
	}
	tr := next(t, sub, "tool_result")
	if got := tr.Payload.(*proto.ToolResult).CallID; got != callID {
		t.Fatalf("tool_result call_id %q, want %q", got, callID)
	}
	if got := next(t, sub, "message").Payload.(*proto.Message).ID; got == "" {
		t.Fatal("message has no id")
	}
}

// F13 (sp_1m7j1wrb296xmcbpkr2t06): a user message queued while a turn runs
// is delivered as a "user" event with an id, followed by a "delivered"
// event for the message.
func TestRelayQueuedMessageThenDelivery(t *testing.T) {
	svc, _, _ := newSvc(t)
	id, _ := svc.Create(alice, t.TempDir(), nil, "")
	ses, _ := svc.Get(alice, id)
	_, sub, _ := ses.log.Subscribe(ses.log.Last(), 100)
	defer sub.Close()
	if err := ses.send(proto.NewCommand("", &proto.User{Content: "deliveredmsg"})); err != nil {
		t.Fatal(err)
	}
	u := next(t, sub, "user")
	e := u.Payload.(*proto.UserEcho)
	if !e.Queued || e.ID == "" {
		t.Fatalf("queued user event: %+v", e)
	}
	_ = ses.send(proto.NewCommand("", &proto.User{Content: "lift"}))
	next(t, sub, "tool_call")
	next(t, sub, "tool_result")
	d := next(t, sub, "delivered")
	p := d.Payload.(*proto.Delivered)
	if len(p.IDs) != 1 || p.IDs[0] != e.ID || p.CallID == "" {
		t.Fatalf("delivered %+v, want ids [%s] for a call", p, e.ID)
	}
}
