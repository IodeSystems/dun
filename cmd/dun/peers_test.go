package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iodesystems/dun/proto"
)

func TestUnderDundUnsetsItself(t *testing.T) {
	t.Setenv("DUN_DUND", "1")
	if !underDund() {
		t.Fatal("DUN_DUND set, not under dund")
	}
	if underDund() {
		t.Fatal("still set: a dun run in exec would think it is under dund")
	}
}

// Peer frames reach the peer handler and never the answer channel: a peer
// message cannot answer an open ask_user.
func TestInputStreamRoutesPeerFrames(t *testing.T) {
	in := `{"type":"peer","from":"a","from_name":"dun:1","text":"yes","wake":true}` + "\n" +
		`{"type":"peer_reply","rid":"p1"}` + "\n" + `{"type":"user","content":"after"}` + "\n"
	s := newInputStreamFrom(strings.NewReader(in))
	var mu sync.Mutex
	var got []string
	s.setPeerFn(func(typ string, line []byte) {
		mu.Lock()
		got = append(got, typ)
		mu.Unlock()
	})
	select {
	case a := <-s.answers:
		t.Fatalf("a peer frame answered: %q", a)
	case u := <-s.users:
		if u != "after" {
			t.Fatalf("user %q", u)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no user message")
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(got, ",") != "peer,peer_reply" {
		t.Fatalf("peer frames %v", got)
	}
}

// A dun_list call goes out as a peer_request and returns on the reply with
// its rid.
func TestPeerLinkRoundTrip(t *testing.T) {
	var out bytes.Buffer
	var outMu sync.Mutex
	em := &emitter{w: lockedWriter{&out, &outMu}}
	link := newPeerLink(em)
	go func() {
		for {
			outMu.Lock()
			line := out.String()
			outMu.Unlock()
			if strings.Contains(line, "peer_request") {
				var ev proto.Event
				_ = json.Unmarshal([]byte(strings.TrimSpace(line)), &ev)
				req := ev.Payload.(*proto.PeerRequest)
				link.reply(proto.PeerReply{RID: "other"}) // someone else's: dropped
				link.reply(proto.PeerReply{RID: req.RID, Sessions: []proto.PeerSession{
					{ID: "20261021-222836", Name: "dun:222836", Worktree: "/w/dun-worktree-1", Branch: "dun/1", You: true}}})
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	ps, err := link.List(context.Background())
	if err != nil || len(ps) != 1 || ps[0].Name != "dun:222836" || !ps[0].You || ps[0].Worktree == "" {
		t.Fatalf("%+v %v", ps, err)
	}
}

func TestPeerLinkTimesOutWithoutDund(t *testing.T) {
	old := peerTimeout
	peerTimeout = 50 * time.Millisecond
	defer func() { peerTimeout = old }()
	link := newPeerLink(&emitter{w: &bytes.Buffer{}})
	if _, err := link.Send(context.Background(), "x", "y", true); err == nil || !strings.Contains(err.Error(), "dund did not answer") {
		t.Fatalf("err %v", err)
	}
}

func TestDunMsgRender(t *testing.T) {
	pv, _ := renderToolResult(renderCtx{tool: "dun_msg", args: map[string]any{"to": "raglit:230000", "text": "is it rebuilt?"},
		result: "delivered to raglit:230000 (20261021-230000): it is busy and reads it in its current turn."})
	if !strings.Contains(pv, "raglit:230000") || !strings.Contains(pv, "is it rebuilt?") {
		t.Fatalf("preview %q", pv)
	}
	pv, _ = renderToolResult(renderCtx{tool: "dun_msg", args: map[string]any{"to": "x", "text": "y"},
		result: "ERROR: rate limit: at most 6 messages a minute to x"})
	if !strings.Contains(pv, "rate limit") {
		t.Fatalf("refusal preview %q", pv)
	}
}

type lockedWriter struct {
	b  *bytes.Buffer
	mu *sync.Mutex
}

func (w lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}
