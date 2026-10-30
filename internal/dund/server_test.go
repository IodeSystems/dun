package dund

import (
	"bufio"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/dun"
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
		upgrade func(string, string) error
		ok      bool
	}{{"client-started", func(string, string) error { return nil }, true}, {"service", nil, false}} {
		sock := filepath.Join(t.TempDir(), "dund.sock")
		ln, err := net.Listen("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		srv := NewServer(svc, func(net.Conn) (string, error) { return alice, nil })
		var asked string
		up := c.upgrade
		if up != nil {
			up = func(exe, _ string) error { asked = exe; return nil }
		}
		srv.SetBuild("v1", "/bin/dund-v1", up)
		why := ""
		if !c.ok {
			why = "this user cannot write in /bin, where its binary is"
			srv.SetNoUpgrade(why)
		}
		go srv.Serve(ln)
		cl := dial(t, sock)
		cl.send("h", "", &proto.Hello{Version: "v2"})
		r := cl.until(typed("hello")).Payload.(*proto.HelloReply)
		if r.Version != "v1" || r.Exe != "/bin/dund-v1" || r.Upgradable != c.ok || r.NoUpgrade != why {
			t.Errorf("%s: hello %+v", c.name, r)
		}
		cl.send("u", "", &proto.Upgrade{Exe: "/bin/dun-v2"})
		a := cl.until(ackOf("u")).Payload.(*proto.Ack)
		if c.ok && (a.Error != "" || asked != "/bin/dun-v2") {
			t.Errorf("%s: upgrade ack %+v asked %q", c.name, a, asked)
		}
		if !c.ok && (!strings.Contains(a.Error, "service") || !strings.Contains(a.Error, why)) {
			t.Errorf("%s: want a service refusal, got %+v", c.name, a)
		}
		ln.Close()
	}
}

// Resuming a session whose log holds a long backlog (a turn streams an event
// per token) must not drop the connection: it gets a snapshot. Before, the
// replay overflowed the connection's queue and the connection was closed
// silently — every resume of a long session failed the same way.
func TestResumeWithALongBacklog(t *testing.T) {
	svc, _, _ := newSvc(t)
	sock := filepath.Join(t.TempDir(), "dund.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go NewServer(svc, func(net.Conn) (string, error) { return alice, nil }).Serve(ln)

	id, _ := svc.Create(alice, t.TempDir(), nil, "")
	ses, _ := svc.Get(alice, id)
	for i := 0; i < 5000; i++ {
		ses.log.Append(proto.NewEvent(&proto.Token{Text: "x"}))
	}
	c := dial(t, sock)
	c.send("s", id, &proto.Subscribe{After: 0})
	c.until(ackOf("s"))
	if ev := c.until(typed("snapshot")); ev.Payload.(*proto.Snapshot).After < 5000 {
		t.Fatalf("snapshot after %d", ev.Payload.(*proto.Snapshot).After)
	}
	// Still connected: a command is answered.
	c.send("l", "", &proto.ListSessions{})
	c.until(typed("sessions"))
}

// A client that goes while viewing a sub-agent never sends "leave", and the
// engine went on streaming that child's entries to nobody. dund sends the
// leaves the connection still owed; ones it sent itself are not repeated.
func TestServerLeavesTheViewsOfAClientThatWent(t *testing.T) {
	svc, sp, _ := newSvc(t)
	sock := filepath.Join(t.TempDir(), "dund.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go NewServer(svc, UnixPeer).Serve(ln)

	a := dial(t, sock)
	a.send("c1", "", &proto.Create{Workspace: t.TempDir()})
	id := a.until(ackOf("c1")).Session
	a.send("c2", id, &proto.Subscribe{})
	a.until(ackOf("c2"))
	for i, act := range []struct{ action, agent string }{{"view", "1"}, {"leave", "1"}, {"view", "2"}} {
		cid := "v" + strconv.Itoa(i)
		a.send(cid, id, &proto.AgentCmd{ID: act.agent, Action: act.action})
		a.until(ackOf(cid))
	}
	a.c.Close()

	want := []string{"agent " + id + ": view 1", "agent " + id + ": leave 1", "agent " + id + ": view 2", "agent " + id + ": leave 2"}
	deadline := time.Now().Add(5 * time.Second)
	var got []string
	for time.Now().Before(deadline) {
		got = got[:0]
		for _, r := range sp.received() {
			if strings.HasPrefix(r, "agent ") {
				got = append(got, r)
			}
		}
		if len(got) >= len(want) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("the engine received %q, want %q", got, want)
	}
}

// `dun oauth token` reaches the broker over the socket; an engine's peer
// request reaches the same one (dun/oauthbroker.go).
func TestServerOAuthCommand(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	svc, sp, _ := newSvc(t)
	sock := filepath.Join(t.TempDir(), "dund.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go NewServer(svc, UnixPeer).Serve(ln)

	a := dial(t, sock)
	spec := dun.MarshalOAuthSpec(dun.OAuthSpec{ClientID: "c"})
	a.send("o1", "", &proto.OAuthCmd{OAuthReq: proto.OAuthReq{Op: "status", ID: "eleven", URL: "https://x.example/mcp", Spec: spec}})
	ev := a.until(func(ev proto.Event) bool { return ev.Re == "o1" })
	r, ok := ev.Payload.(*proto.OAuthResult)
	if !ok || r.Error != "" || dun.UnmarshalOAuthStatus(r.Status).LoggedIn {
		t.Fatalf("status: %+v", ev.Payload)
	}
	a.send("o2", "nope", &proto.OAuthCmd{OAuthReq: proto.OAuthReq{Op: "status", ID: "eleven", URL: "https://x.example/mcp", Spec: spec}})
	if p, ok := a.until(ackOf("o2")).Payload.(*proto.Ack); !ok || !strings.Contains(p.Error, "no such session") {
		t.Errorf("another's session: %+v", p)
	}

	// The engine's way: a peer request, answered on its stdin.
	a.send("c1", "", &proto.Create{Workspace: t.TempDir()})
	id := a.until(ackOf("c1")).Session
	a.send("c2", id, &proto.Subscribe{})
	a.until(ackOf("c2"))
	req, _ := json.Marshal(proto.PeerRequest{RID: "r1", Op: "oauth", OAuth: &proto.OAuthReq{Op: "status", ID: "eleven", URL: "https://x.example/mcp", Spec: spec}})
	a.send("c3", id, &proto.User{Content: "peer-request " + string(req)})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, g := range sp.received() {
			if strings.HasPrefix(g, "reply "+id+": ") && strings.Contains(g, `"rid":"r1"`) {
				if !strings.Contains(g, `"oauth":{"status":`) || strings.Contains(g, `"error"`) {
					t.Fatalf("peer reply: %s", g)
				}
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no peer reply: %q", sp.received())
}
