package dund

import (
	"bufio"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/dun/proto"
)

type client struct {
	t  *testing.T
	c  net.Conn
	sc *bufio.Scanner
}

func dial(t *testing.T, sock string) *client {
	t.Helper()
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return &client{t: t, c: c, sc: newScanner(c)}
}

func (c *client) send(cid, session string, payload any) {
	cmd := proto.NewCommand(cid, payload)
	cmd.Session = session
	if err := json.NewEncoder(c.c).Encode(cmd); err != nil {
		c.t.Fatal(err)
	}
}

// until reads events until one matches.
func (c *client) until(match func(proto.Event) bool) proto.Event {
	c.t.Helper()
	_ = c.c.SetReadDeadline(time.Now().Add(10 * time.Second)) // generous: the full suite runs in parallel
	for c.sc.Scan() {
		var ev proto.Event
		if json.Unmarshal(c.sc.Bytes(), &ev) == nil && match(ev) {
			return ev
		}
	}
	c.t.Fatalf("no matching event: %v", c.sc.Err())
	return proto.Event{}
}

func ackOf(cid string) func(proto.Event) bool {
	return func(ev proto.Event) bool { return ev.Type == "ack" && ev.Re == cid }
}

func typed(typ string) func(proto.Event) bool {
	return func(ev proto.Event) bool { return ev.Type == typ }
}

func TestServerOverAUnixSocket(t *testing.T) {
	svc, _, _ := newSvc(t)
	sock := filepath.Join(t.TempDir(), "dund.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go NewServer(svc, UnixPeer).Serve(ln)

	a := dial(t, sock)
	ws := t.TempDir()
	a.send("c1", "", &proto.Create{Workspace: ws})
	ack := a.until(ackOf("c1"))
	if p := ack.Payload.(*proto.Ack); p.Error != "" || ack.Session == "" {
		t.Fatalf("create: %+v %+v", ack, p)
	}
	id := ack.Session

	// The principal is the peer's uid, from the kernel.
	if ls := svc.List("unix:" + strconv.Itoa(os.Getuid())); len(ls) != 1 || ls[0].ID != id {
		t.Fatalf("owner: %+v", ls)
	}

	a.send("c2", id, &proto.Subscribe{})
	a.until(ackOf("c2"))
	a.until(typed("ready")) // replayed from the log

	// A second device of the same user follows the session too.
	b := dial(t, sock)
	b.send("c3", id, &proto.Subscribe{})
	b.until(ackOf("c3"))

	a.send("c4", id, &proto.User{Content: "hello"})
	a.until(ackOf("c4"))
	for _, cl := range []*client{a, b} {
		ev := cl.until(typed("message"))
		if ev.Payload.(*proto.Message).Content != "re: hello" || ev.Session != id || ev.Seq == 0 {
			t.Fatalf("message: %+v", ev)
		}
	}

	// UI state: saved, and pushed to the same user's other connection.
	a.send("c5", id, &proto.UIPut{State: map[string]any{"draft": "wip"}})
	a.until(ackOf("c5"))
	if ev := b.until(typed("ui_state")); ev.Payload.(*proto.UIState).State["draft"] != "wip" {
		t.Fatalf("ui push: %+v", ev)
	}
	b.send("c6", id, &proto.UIGet{})
	if ev := b.until(ackOrType("c6", "ui_state")); ev.Type != "ui_state" || ev.Payload.(*proto.UIState).State["draft"] != "wip" {
		t.Fatalf("ui get: %+v", ev)
	}

	a.send("c7", "", &proto.ListSessions{})
	if ev := a.until(typed("sessions")); len(ev.Payload.(*proto.SessionList).Items) != 1 {
		t.Fatalf("sessions: %+v", ev.Payload)
	}

	// A command for a session that does not exist is refused, by ack.
	a.send("c8", "nope", &proto.User{Content: "x"})
	if p := a.until(ackOf("c8")).Payload.(*proto.Ack); !strings.Contains(p.Error, "no such session") {
		t.Fatalf("unknown session: %+v", p)
	}
	a.send("c9", "", &proto.User{Content: "x"})
	if p := a.until(ackOf("c9")).Payload.(*proto.Ack); !strings.Contains(p.Error, "needs a session") {
		t.Fatalf("no session: %+v", p)
	}

	// Detached, b no longer hears the session.
	b.send("c10", id, &proto.Detach{})
	b.until(ackOf("c10"))
	a.send("c11", id, &proto.User{Content: "again"})
	a.until(typed("message"))
	_ = b.c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	for b.sc.Scan() {
		var ev proto.Event
		if json.Unmarshal(b.sc.Bytes(), &ev) == nil && ev.Type == "message" {
			t.Fatal("a detached connection still gets the session's events")
		}
	}
}

func ackOrType(cid, typ string) func(proto.Event) bool {
	return func(ev proto.Event) bool { return ev.Re == cid && (ev.Type == "ack" || ev.Type == typ) }
}

// A connection whose peer cannot be identified is refused with a reason the
// client can show, not closed silently.
func TestServerRefusalSaysWhy(t *testing.T) {
	svc, _, _ := newSvc(t)
	sock := filepath.Join(t.TempDir(), "dund.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go NewServer(svc, func(net.Conn) (string, error) { return "", errors.New("no peer credentials") }).Serve(ln)
	c := dial(t, sock)
	ev := c.until(typed("ack"))
	if p := ev.Payload.(*proto.Ack); !strings.Contains(p.Error, "cannot tell who is connecting: no peer credentials") {
		t.Fatalf("refusal: %+v", p)
	}
}

// hello names the build; upgrade is honoured only when an upgrade func is set
// (a client-started dund), and refused with the service hint otherwise.
func TestServerHelloAndUpgrade(t *testing.T) {
	svc, _, _ := newSvc(t)
	for _, c := range []struct {
		name    string
		upgrade func(string) error
		ok      bool
	}{{"client-started", func(string) error { return nil }, true}, {"service", nil, false}} {
		sock := filepath.Join(t.TempDir(), "dund.sock")
		ln, err := net.Listen("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		srv := NewServer(svc, func(net.Conn) (string, error) { return alice, nil })
		var asked string
		up := c.upgrade
		if up != nil {
			up = func(exe string) error { asked = exe; return nil }
		}
		srv.SetBuild("v1", "/bin/dund-v1", up)
		go srv.Serve(ln)
		cl := dial(t, sock)
		cl.send("h", "", &proto.Hello{Version: "v2"})
		r := cl.until(typed("hello")).Payload.(*proto.HelloReply)
		if r.Version != "v1" || r.Exe != "/bin/dund-v1" || r.Upgradable != c.ok {
			t.Errorf("%s: hello %+v", c.name, r)
		}
		cl.send("u", "", &proto.Upgrade{Exe: "/bin/dun-v2"})
		a := cl.until(ackOf("u")).Payload.(*proto.Ack)
		if c.ok && (a.Error != "" || asked != "/bin/dun-v2") {
			t.Errorf("%s: upgrade ack %+v asked %q", c.name, a, asked)
		}
		if !c.ok && !strings.Contains(a.Error, "service") {
			t.Errorf("%s: want a service refusal, got %+v", c.name, a)
		}
		ln.Close()
	}
}
