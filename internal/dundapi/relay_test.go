package dundapi_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

// The dundapi surface (the web/mobile client's) carries the engine's explicit
// sequence unchanged: a user event with the message's id, a delivered event
// for a queued message, a tool_call with a call_id and a tool_result with the
// same one. This reads the live stream over the same graphql-ws socket a
// browser uses, so it pins the API, not just the service.

func subSocket(t *testing.T, srv *httptest.Server, tok, id string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, tk := call(t, srv, tok, "POST", "/api/v1/sessions/"+id+"/subscribe-token", nil)
	principal := tk["principal"].(string)
	ts := int64(tk["timestamp"].(float64))
	mac := tk["hmac"].(string)
	c := wsDial(t, ctx, dl(srv))
	p, _ := json.Marshal(map[string]any{"query": subscribeQuery(id, principal, ts, mac, 0)})
	wsWrite(t, ctx, c, wsMsg{ID: "1", Type: "subscribe", Payload: p})
	return c
}

func collect(t *testing.T, c *websocket.Conn, until string) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		msg := wsRead(t, ctx, c)
		if msg.Type == "error" {
			t.Fatalf("subscription error: %s", msg.Payload)
		}
		if msg.Type != "next" {
			continue
		}
		var body struct {
			Data struct {
				Ev struct {
					Event map[string]any `json:"event"`
				} `json:"dund_sessionEvents"`
			} `json:"data"`
		}
		_ = json.Unmarshal(msg.Payload, &body)
		ev := body.Data.Ev.Event
		if len(ev) > 0 && ev["type"] == until {
			return ev
		}
	}
}

func newSession(t *testing.T, srv *httptest.Server, tok string) string {
	t.Helper()
	_, m := call(t, srv, tok, "POST", "/api/v1/sessions", map[string]any{"workspace": t.TempDir()})
	return m["id"].(string)
}

// F13 (sp_1m7j1wrb262p13019119xh, sp_1m7j1wrb25qdgzzbvc3ms8): a tool call in
// a turn reaches the API as a tool_call event with a call_id, and the call's
// end as a tool_result with the same call_id.
func TestSessionEventsCarryToolCallIDs(t *testing.T) {
	srv, tok, _ := setup(t)
	id := newSession(t, srv, tok)
	c := subSocket(t, srv, tok, id)
	if ev := collect(t, c, "ready"); ev["type"] != "ready" {
		t.Fatalf("no ready")
	}
	if code, _ := call(t, srv, tok, "POST", "/api/v1/sessions/"+id+"/messages", map[string]any{"content": "toolpair"}); code != 200 {
		t.Fatalf("message: %d", code)
	}
	tc := collect(t, c, "tool_call")
	callID, _ := tc["call_id"].(string)
	if callID == "" {
		t.Fatalf("tool_call over the API has no call_id: %v", tc)
	}
	tr := collect(t, c, "tool_result")
	if got, _ := tr["call_id"].(string); got != callID {
		t.Fatalf("tool_result call_id %q, want %q", got, callID)
	}
	if msg := collect(t, c, "message"); msg["id"] == nil {
		t.Fatalf("message over the API has no id: %v", msg)
	}
}

// F13 (sp_1m7j1wrb296xmcbpkr2t06): a user message queued while a turn runs
// reaches the API as a user event with an id, followed by a delivered event
// for the message.
func TestSessionEventsCarryMessageIDAndDelivery(t *testing.T) {
	srv, tok, _ := setup(t)
	id := newSession(t, srv, tok)
	c := subSocket(t, srv, tok, id)
	if ev := collect(t, c, "ready"); ev["type"] != "ready" {
		t.Fatalf("no ready")
	}
	if code, _ := call(t, srv, tok, "POST", "/api/v1/sessions/"+id+"/messages", map[string]any{"content": "deliveredmsg"}); code != 200 {
		t.Fatalf("message: %d", code)
	}
	u := collect(t, c, "user")
	uid, _ := u["id"].(string)
	if u["queued"] != true || uid == "" {
		t.Fatalf("queued user over the API: %v", u)
	}
	if code, _ := call(t, srv, tok, "POST", "/api/v1/sessions/"+id+"/messages", map[string]any{"content": "lift"}); code != 200 {
		t.Fatalf("lift: %d", code)
	}
	d := collect(t, c, "delivered")
	ids, _ := d["ids"].([]any)
	if len(ids) != 1 || ids[0] != uid {
		t.Fatalf("delivered over the API: %v, want ids [%s]", d, uid)
	}
	if d["call_id"] == nil {
		t.Fatalf("delivered over the API names no call: %v", d)
	}
}
