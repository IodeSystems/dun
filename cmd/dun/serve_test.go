package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A new subscriber replays the history; later events arrive live; unsubscribe
// closes the channel.
func TestServeHub_BroadcastReplay(t *testing.T) {
	h := &serveHub{subs: map[chan string]struct{}{}}
	h.broadcast(`{"type":"ready"}`) // before anyone subscribes

	ch, hist := h.subscribe()
	if len(hist) != 1 || !strings.Contains(hist[0], "ready") {
		t.Fatalf("history not replayed: %v", hist)
	}
	h.broadcast(`{"type":"token","text":"hi"}`)
	select {
	case l := <-ch:
		if !strings.Contains(l, "token") {
			t.Fatalf("live event wrong: %s", l)
		}
	default:
		t.Fatal("live event not delivered")
	}
	h.unsubscribe(ch)
	if _, ok := <-ch; ok {
		t.Fatal("channel should be closed after unsubscribe")
	}
}

// POST /input writes a valid event to the engine stdin as ONE compact line, and
// rejects event types the engine doesn't understand.
func TestServeHub_Input(t *testing.T) {
	var stdin bytes.Buffer
	h := &serveHub{stdin: &stdin, subs: map[chan string]struct{}{}}

	req := httptest.NewRequest("POST", "/input", strings.NewReader(`{"type":"user","content":"go"}`))
	rw := httptest.NewRecorder()
	h.input(rw, req)
	if rw.Code != http.StatusNoContent {
		t.Fatalf("valid input code=%d", rw.Code)
	}
	got := stdin.String()
	if strings.Count(got, "\n") != 1 || !strings.HasSuffix(got, "\n") {
		t.Fatalf("stdin should be one newline-terminated line: %q", got)
	}
	if !strings.Contains(got, `"content":"go"`) {
		t.Fatalf("stdin missing payload: %q", got)
	}

	stdin.Reset()
	req = httptest.NewRequest("POST", "/input", strings.NewReader(`{"type":"bogus"}`))
	rw = httptest.NewRecorder()
	h.input(rw, req)
	if rw.Code != http.StatusBadRequest {
		t.Fatalf("bad type should be 400, got %d", rw.Code)
	}
	if stdin.Len() != 0 {
		t.Fatalf("rejected event must not reach the engine: %q", stdin.String())
	}
}

func TestReachableURLs(t *testing.T) {
	// A specific host → just that URL.
	if got := reachableURLs("127.0.0.1:8734"); len(got) != 1 || got[0] != "http://127.0.0.1:8734" {
		t.Fatalf("specific host: %v", got)
	}
	// All-interfaces bind → loopback plus (maybe) LAN IPs, port preserved.
	got := reachableURLs("0.0.0.0:8734")
	if len(got) < 1 || got[0] != "http://127.0.0.1:8734" {
		t.Fatalf("all-interfaces should lead with loopback: %v", got)
	}
	for _, u := range got {
		if !strings.HasSuffix(u, ":8734") {
			t.Fatalf("port not preserved in %q", u)
		}
	}
}

func TestLoopbackOnly(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:8734": true, "localhost:8734": false, // localhost isn't an IP literal
		"0.0.0.0:8734": false, ":8734": false, "192.168.1.76:8734": false,
	}
	for addr, want := range cases {
		if got := loopbackOnly(addr); got != want {
			t.Errorf("loopbackOnly(%q)=%v want %v", addr, got, want)
		}
	}
}

// A wedged subscriber (full buffer) is dropped rather than stalling broadcast.
func TestServeHub_DropsSlowSubscriber(t *testing.T) {
	h := &serveHub{subs: map[chan string]struct{}{}}
	slow := make(chan string) // unbuffered + nobody reading → always full
	h.mu.Lock()
	h.subs[slow] = struct{}{}
	h.mu.Unlock()

	h.broadcast(`{"type":"token","text":"x"}`)
	h.mu.Lock()
	_, still := h.subs[slow]
	h.mu.Unlock()
	if still {
		t.Fatal("a non-receiving subscriber should be dropped, not block the engine")
	}
}
