//go:build unix

package dundcli

import (
	"context"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/iodesystems/dun/internal/dund"
	"github.com/iodesystems/dun/internal/dundapi"
	"github.com/iodesystems/dun/proto"
)

// ctlFixture is a webCtl serving a1 with the TUI off, whose "dun" prints
// in-tui and waits, and a browser signed in to it.
func ctlFixture(t *testing.T) (*webCtl, string, *http.Client) {
	t.Helper()
	home := t.TempDir()
	svc, err := dund.New(filepath.Join(home, "dund"), dund.ExecSpawner{Bin: "/bin/true"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)
	bin := filepath.Join(t.TempDir(), "dun")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho in-tui; exec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	c, err := newWebCtl(home, bin, svc, dundapi.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.stop)
	a1 := freeAddr(t)
	c.start(webState{Enabled: true, Addr: a1})
	tok, err := dund.NewToken(filepath.Join(home, "dund"))
	if err != nil {
		t.Fatal(err)
	}
	jar, _ := cookiejar.New(nil)
	b := &http.Client{Jar: jar, Timeout: 5 * time.Second, Transport: &http.Transport{DialContext: pin},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := b.PostForm(dl(a1)+"/auth/signin", url.Values{"token": {tok}})
	if err != nil || resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("sign-in: %v %v", resp, err)
	}
	resp.Body.Close()
	return c, a1, b
}

// The browser is on dun.localhost (dundapi sends shared loopback names
// there); pin dials it as 127.0.0.1 whatever the resolver says.
func dl(addr string) string {
	_, port, _ := net.SplitHostPort(addr)
	return "http://dun.localhost:" + port
}

func pin(ctx context.Context, network, addr string) (net.Conn, error) {
	if rest, ok := strings.CutPrefix(addr, "dun.localhost:"); ok {
		addr = "127.0.0.1:" + rest
	}
	return (&net.Dialer{}).DialContext(ctx, network, addr)
}

func status(t *testing.T, b *http.Client, u string) int {
	t.Helper()
	resp, err := b.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// A change refused for a busy port changes nothing: /tui stays off.
func TestRefusedChangeLeavesTheTUIOff(t *testing.T) {
	c, a1, b := ctlFixture(t)
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	if _, err := c.set(proto.WebSet{Addr: busy.Addr().String(), TUI: ptr(true)}); err == nil {
		t.Fatal("a busy port was taken")
	}
	if code := status(t, b, dl(a1)+"/tui/"); code != 404 {
		t.Errorf("/tui after a refused change: %d", code)
	}
	if c.st.TUI || c.tuiOn.Load() {
		t.Errorf("state %+v, tuiOn %v", c.st, c.tuiOn.Load())
	}
}

// disable tui kills the open terminals; after stop, no change is taken.
func TestDisableTUIKillsAnOpenSocket(t *testing.T) {
	c, a1, b := ctlFixture(t)
	if _, err := c.set(proto.WebSet{TUI: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(dl(a1))
	h := http.Header{"Origin": {dl(a1)}}
	for _, k := range b.Jar.Cookies(u) {
		h.Add("Cookie", k.Name+"="+k.Value)
	}
	conn, _, err := (&websocket.Dialer{NetDialContext: pin}).Dial("ws"+strings.TrimPrefix(dl(a1), "http")+"/tui/ws", h)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	got := ""
	for !strings.Contains(got, "in-tui") {
		_, m, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read: %v (%q)", err, got)
		}
		got += string(m)
	}
	if _, err := c.set(proto.WebSet{TUI: ptr(false)}); err != nil {
		t.Fatal(err)
	}
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				t.Fatal("the socket stayed open after disable tui")
			}
			break
		}
	}
	if n := c.term.Active(); n != 0 {
		// The handler's own cleanup runs as the read ends; give it a moment.
		time.Sleep(200 * time.Millisecond)
		if n = c.term.Active(); n != 0 {
			t.Errorf("%d ptys live after disable tui", n)
		}
	}
	c.stop()
	if _, err := c.set(proto.WebSet{Enable: ptr(true)}); err == nil || !strings.Contains(err.Error(), "stopping") {
		t.Errorf("set after stop: %v", err)
	}
	if status := func() error { _, err := net.DialTimeout("tcp", a1, time.Second); return err }(); status == nil {
		t.Error("still listening after stop")
	}
}

// --trust-proxy is saved in web.json and applied; a bad entry changes nothing.
func TestTrustProxies(t *testing.T) {
	c, _, _ := ctlFixture(t)
	if _, err := c.set(proto.WebSet{TrustProxies: []string{"not-a-cidr"}}); err == nil {
		t.Fatal("a bad proxy was taken")
	}
	st, err := c.set(proto.WebSet{TrustProxies: []string{"10.0.0.0/8", "192.0.2.1"}})
	if err != nil || strings.Join(st.TrustProxies, ",") != "10.0.0.0/8,192.0.2.1" {
		t.Fatalf("set: %v %+v", err, st)
	}
	if got := *c.trust.Load(); len(got) != 2 || got[1].String() != "192.0.2.1/32" {
		t.Errorf("applied: %v", got)
	}
	if saved, _, _ := loadWebState(c.home); strings.Join(saved.TrustedProxies, ",") != "10.0.0.0/8,192.0.2.1" {
		t.Errorf("saved: %+v", saved)
	}
	if _, err := c.set(proto.WebSet{TrustProxies: []string{}}); err != nil || len(*c.trust.Load()) != 0 {
		t.Errorf("clear: %v %v", err, *c.trust.Load())
	}
	if w := tuiWarning(webState{Enabled: true, TUI: true, Addr: "192.168.1.76:20003"}); !strings.Contains(w, "other servers on 192.168.1.76 receive this sign-in") {
		t.Errorf("LAN IP warning: %q", w)
	}
}
