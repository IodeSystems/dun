package dundapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

type wsMsg struct {
	ID      string          `json:"id,omitempty"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

func wsDial(t *testing.T, ctx context.Context, url string) *websocket.Conn {
	t.Helper()
	// No Authorization header: a browser cannot send one on a WebSocket.
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(url, "http")+"/api/graphql",
		&websocket.DialOptions{Subprotocols: []string{"graphql-transport-ws"}})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { c.Close(websocket.StatusNormalClosure, "") })
	wsWrite(t, ctx, c, wsMsg{Type: "connection_init"})
	if m := wsRead(t, ctx, c); m.Type != "connection_ack" {
		t.Fatalf("want connection_ack, got %+v", m)
	}
	return c
}

func wsWrite(t *testing.T, ctx context.Context, c *websocket.Conn, m wsMsg) {
	t.Helper()
	b, _ := json.Marshal(m)
	if err := c.Write(ctx, websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
}

func wsRead(t *testing.T, ctx context.Context, c *websocket.Conn) wsMsg {
	t.Helper()
	_, b, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var m wsMsg
	_ = json.Unmarshal(b, &m)
	return m
}

func subscribeQuery(id, principal string, ts int64, mac string, after int) string {
	return fmt.Sprintf(`subscription { dund_sessionEvents(session: %q, after: %d, principal: %q, timestamp: %d, hmac: %q) { event } }`,
		id, after, principal, ts, mac)
}

// The live path end to end: a token from an authenticated call, a socket
// with no bearer, the session's events replayed then live.
func TestSessionEventsSubscription(t *testing.T) {
	srv, tok, _ := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, m := call(t, srv, tok, "POST", "/api/v1/sessions", map[string]any{"workspace": t.TempDir()})
	id := m["id"].(string)
	code, tk := call(t, srv, tok, "POST", "/api/v1/sessions/"+id+"/subscribe-token", nil)
	if code != 200 {
		t.Fatalf("token: %d %v", code, tk)
	}
	principal, ts, mac := tk["principal"].(string), int64(tk["timestamp"].(float64)), tk["hmac"].(string)

	c := wsDial(t, ctx, srv.URL)
	p, _ := json.Marshal(map[string]any{"query": subscribeQuery(id, principal, ts, mac, 0)})
	wsWrite(t, ctx, c, wsMsg{ID: "1", Type: "subscribe", Payload: p})
	sentAfterReady := false
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
		if ev["type"] == "ready" && !sentAfterReady {
			sentAfterReady = true
			if code, _ := call(t, srv, tok, "POST", "/api/v1/sessions/"+id+"/messages", map[string]any{"content": "live?"}); code != 200 {
				t.Fatalf("message: %d", code)
			}
		}
		if ev["type"] == "message" && ev["content"] == "re: live?" {
			if ev["seq"] == nil {
				t.Error("live events carry no seq")
			}
			return
		}
	}
}

// A forged token is refused; a plain query over the socket gets no data
// either — there is no principal on it.
func TestSubscriptionRefusesWithoutAValidToken(t *testing.T) {
	srv, tok, _ := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, m := call(t, srv, tok, "POST", "/api/v1/sessions", map[string]any{"workspace": t.TempDir()})
	id := m["id"].(string)
	c := wsDial(t, ctx, srv.URL)
	p, _ := json.Marshal(map[string]any{"query": subscribeQuery(id, "unix:1000", time.Now().Unix(), "deadbeef", 0)})
	wsWrite(t, ctx, c, wsMsg{ID: "1", Type: "subscribe", Payload: p})
	if msg := wsRead(t, ctx, c); msg.Type != "error" || !strings.Contains(string(msg.Payload), "token") {
		t.Fatalf("forged token: %+v %s", msg, msg.Payload)
	}
	q, _ := json.Marshal(map[string]any{"query": `{ dund { listSessions { items { id } } } }`})
	wsWrite(t, ctx, c, wsMsg{ID: "2", Type: "subscribe", Payload: q})
	for {
		msg := wsRead(t, ctx, c)
		if msg.ID != "2" {
			continue
		}
		if strings.Contains(string(msg.Payload), id) {
			t.Fatalf("a query over the socket listed sessions with no token: %s", msg.Payload)
		}
		return
	}
}

