package dund

import (
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/dun/proto"
)

func recv(t *testing.T, b *mailbox) proto.MsgIn {
	t.Helper()
	select {
	case m := <-b.in:
		return m
	case <-time.After(2 * time.Second):
		t.Fatal("nothing arrived")
	}
	return proto.MsgIn{}
}

// A listener's name is its own while it listens: a second claim, a live
// session's name and another owner's sends are refused; unlistening frees
// the name and ends the token.
func TestListen_NameIsHeldAndFreed(t *testing.T) {
	svc, _, _ := newSvc(t)
	b, err := svc.Listen(alice, "claude")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Listen(alice, "claude"); err == nil || !strings.Contains(err.Error(), "in use") {
		t.Fatalf("a second listener: %v", err)
	}
	s1, _ := svc.Create(alice, t.TempDir(), nil, "")
	name := svc.Peers(alice, "")[0].Name
	if _, err := svc.Listen(alice, name); err == nil {
		t.Fatalf("a live session's name %q was taken", name)
	}
	if _, err := svc.MsgSend("bob", proto.MsgSend{Token: b.token, To: s1, Text: "x"}); err == nil {
		t.Error("another owner used alice's token")
	}
	svc.Unlisten(b)
	if _, err := svc.MsgSend(alice, proto.MsgSend{Token: b.token, To: s1, Text: "x"}); err == nil {
		t.Error("a token outlived its listener")
	}
	if _, err := svc.Listen(alice, "claude"); err != nil {
		t.Errorf("the name was not freed: %v", err)
	}
}

// A listener sends to a session as its name; the session's dun_list shows
// the listener, and its dun_msg reaches it, from the session's name.
func TestListen_SessionsAndListenersTalk(t *testing.T) {
	svc, sp, _ := newSvc(t)
	b, _ := svc.Listen(alice, "claude")
	s1, _ := svc.Create(alice, t.TempDir(), nil, "")
	sent, err := svc.MsgSend(alice, proto.MsgSend{Token: b.token, To: s1, Text: "do the thing"})
	if err != nil || !strings.Contains(sent.Detail, "woken") {
		t.Fatalf("to a session: %+v %v", sent, err)
	}
	if r := waitRecord(t, sp, "peer "+s1+":"); !strings.Contains(r, "do the thing") {
		t.Fatalf("delivered %q", r)
	}
	if _, err := svc.PeerSend(alice, s1, "claude", "done it", true); err != nil {
		t.Fatal(err)
	}
	if m := recv(t, b); m.Text != "done it" || m.From != svc.Peers(alice, "")[0].Name {
		t.Fatalf("the listener got %+v", m)
	}
	if _, err := svc.MsgSend(alice, proto.MsgSend{To: "claude", Text: "from a shell"}); err != nil {
		t.Fatal(err)
	}
	if m := recv(t, b); m.From != "cli" {
		t.Errorf("no token: from %q, want cli", m.From)
	}
	rows := svc.listenerRows(alice)
	if len(rows) != 1 || rows[0].Name != "claude" {
		t.Errorf("dun_list's listener rows: %+v", rows)
	}
}

// A full box drops its oldest and says how many.
func TestListen_FullBoxDropsTheOldest(t *testing.T) {
	b := &mailbox{in: make(chan proto.MsgIn, 2)}
	b.put(proto.MsgIn{Text: "1"})
	b.put(proto.MsgIn{Text: "2"})
	if n := b.put(proto.MsgIn{Text: "3"}); n != 1 {
		t.Fatalf("dropped %d", n)
	}
	if m := <-b.in; m.Text != "2" {
		t.Errorf("oldest kept: %q", m.Text)
	}
}
