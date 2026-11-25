package acp

import (
	"context"
	"encoding/json"
	"testing"

	sdk "github.com/coder/acp-go-sdk"

	"github.com/iodesystems/dun/proto"
)

// A steering request mid-turn is a queued user message: dund gets it, the
// editor gets {"status": "queued"} at once and no echo of its own words,
// and the running prompt ends with its turn.
func TestSteering(t *testing.T) {
	r := newRig(t)
	sid := r.newSession()
	first := r.prompt(sid, "fix it")
	r.d.next()
	r.d.emit(&proto.UserEcho{ID: "m1", Content: "fix it"}, &proto.Turn{Kind: "user"})
	res, err := r.cc.CallExtension(context.Background(), methodSteering, map[string]any{
		"sessionId": sid, "messageId": "x1", "prompt": []map[string]string{{"type": "text", "text": "also the tests"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]string
	if json.Unmarshal(res, &out) != nil || out["status"] != "queued" {
		t.Fatalf("response %s", res)
	}
	if c := r.d.next(); c.Type != "user" || c.Payload.(*proto.User).Content != "also the tests" {
		t.Fatalf("sent %s %+v", c.Type, c.Payload)
	}
	r.d.emit(&proto.UserEcho{ID: "m2", Content: "also the tests", Queued: true},
		&proto.ToolCall{CallID: "c1", Tool: "exec"}, &proto.ToolResult{CallID: "c1", Tool: "exec"},
		&proto.Delivered{IDs: []string{"m2"}, CallID: "c1"}, &proto.Done{})
	if res := waitPrompt(t, first); res.stop != sdk.StopReasonEndTurn {
		t.Fatalf("prompt: %+v", res)
	}
	for _, u := range r.ed.take() {
		if u.UserMessageChunk != nil {
			t.Errorf("the steering message echoed back: %+v", u.UserMessageChunk)
		}
	}
}

func TestSteeringRefused(t *testing.T) {
	r := newRig(t)
	sid := r.newSession()
	for _, tc := range []struct {
		method string
		params map[string]any
		code   int
	}{
		{"_session/other", map[string]any{"sessionId": sid}, -32601},
		{methodSteering, map[string]any{"sessionId": "nope", "text": "x"}, -32002},
		{methodSteering, map[string]any{"sessionId": sid}, -32602},
	} {
		_, err := r.cc.CallExtension(context.Background(), tc.method, tc.params)
		re, ok := err.(*sdk.RequestError)
		if !ok || re.Code != tc.code {
			t.Errorf("%s %v: %v, want code %d", tc.method, tc.params, err, tc.code)
		}
	}
}

func TestSteeringText(t *testing.T) {
	for _, tc := range []struct{ params, want string }{
		{`{"sessionId":"s","id":"x","content":[{"type":"text","text":"hi"}]}`, "hi"},
		{`{"sessionId":"s","_meta":{"k":[{"type":"text","text":"deep"}]}}`, "deep"},
		{`{"sessionId":"s","id":"x","message":"words"}`, "words"},
		{`{"sessionId":"s","q":"the only string"}`, "the only string"},
		{`{"sessionId":"s","a":"one","b":"two"}`, ""},
		{`{"sessionId":"s"}`, ""},
		{`not json`, ""},
	} {
		if got := steeringText(json.RawMessage(tc.params)); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.params, got, tc.want)
		}
	}
}
