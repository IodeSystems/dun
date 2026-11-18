//go:build unix

package webterm

import (
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func wsURL(srv *httptest.Server) string { return "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws" }

func shell(script string) Config {
	return Config{Bin: "/bin/sh", Args: []string{"-c", script}, Env: os.Environ()}
}

// The page, its assets, and the program's output over the socket; KillAll
// ends the program and closes the socket (Shutdown would not: it is hijacked).
func TestTermRunsAndKillAll(t *testing.T) {
	h := New(shell("echo hello-pty; sleep 30"))
	srv := httptest.NewServer(h)
	defer srv.Close()
	for _, p := range []string{"/", "/static/xterm.js"} {
		resp, err := http.Get(srv.URL + p)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("%s: %v %v", p, resp, err)
		}
		resp.Body.Close()
	}
	hdr := http.Header{"Origin": {srv.URL}}
	c, _, err := websocket.DefaultDialer.Dial(wsURL(srv), hdr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	var got string
	for !strings.Contains(got, "hello-pty") {
		_, b, err := c.ReadMessage()
		if err != nil {
			t.Fatalf("read: %v (got %q)", err, got)
		}
		got += string(b)
	}
	if n := h.Active(); n != 1 {
		t.Fatalf("active %d", n)
	}
	h.KillAll()
	for {
		if _, _, err := c.ReadMessage(); err != nil {
			break
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	for h.Active() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := h.Active(); n != 0 {
		t.Fatalf("active after KillAll: %d", n)
	}
	h.Close()
	if _, resp, err := websocket.DefaultDialer.Dial(wsURL(srv), hdr); err == nil || resp == nil || resp.StatusCode != 503 {
		t.Fatalf("after Close: %v %v", resp, err)
	}
}

// A WebSocket from another site, or with no Origin, is refused.
func TestTermRefusesAForeignOrigin(t *testing.T) {
	srv := httptest.NewServer(New(shell("sleep 30")))
	defer srv.Close()
	for _, o := range []string{"https://evil.example", "", "null"} {
		hdr := http.Header{}
		if o != "" {
			hdr.Set("Origin", o)
		}
		_, resp, err := websocket.DefaultDialer.Dial(wsURL(srv), hdr)
		if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
			t.Errorf("origin %q: %v %v", o, resp, err)
		}
	}
}

func TestCleanEnv(t *testing.T) {
	got := CleanEnv([]string{"PATH=/bin", "TMUX=/tmp/s", "INVOCATION_ID=x", "JOURNAL_STREAM=1:2", "XPC_SERVICE_NAME=com.iodesystems.dund", "HOME=/h"})
	if !slices.Equal(got, []string{"PATH=/bin", "HOME=/h"}) {
		t.Fatalf("%v", got)
	}
}
