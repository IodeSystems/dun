package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/iodesystems/dun"
	"github.com/iodesystems/dun/proto"
)

// The engine's half of dun↔dun messaging (dun/peers.go, proto/peer.go): a
// dun.PeerLink over the engine's own stdio. A request goes out as a
// peer_request event; dund answers with a peer_reply command, which the
// input reader hands back here by rid.

// underDund reports whether dund spawned this engine (internal/dund
// ExecSpawner sets DUN_DUND), and unsets it: a dun the agent runs in exec is
// not under dund, and must not think so.
func underDund() bool {
	on := os.Getenv("DUN_DUND") != ""
	_ = os.Unsetenv("DUN_DUND")
	return on
}

// peerTimeout bounds a wait for dund's reply. dund answers at once; a reply
// that does not come means dund is not there.
var peerTimeout = 15 * time.Second

type peerLink struct {
	em *emitter

	mu      sync.Mutex
	seq     int
	waiting map[string]chan proto.PeerReply
}

func newPeerLink(em *emitter) *peerLink {
	return &peerLink{em: em, waiting: map[string]chan proto.PeerReply{}}
}

func (p *peerLink) call(ctx context.Context, req proto.PeerRequest) (proto.PeerReply, error) {
	ch := make(chan proto.PeerReply, 1)
	p.mu.Lock()
	p.seq++
	req.RID = "p" + strconv.Itoa(p.seq)
	p.waiting[req.RID] = ch
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.waiting, req.RID)
		p.mu.Unlock()
	}()
	p.em.emit(&req)
	t := time.NewTimer(peerTimeout)
	defer t.Stop()
	select {
	case rep := <-ch:
		if rep.Error != "" {
			return rep, errors.New(rep.Error)
		}
		return rep, nil
	case <-t.C:
		return proto.PeerReply{}, fmt.Errorf("dund did not answer within %s", peerTimeout)
	case <-ctx.Done():
		return proto.PeerReply{}, ctx.Err()
	}
}

// reply hands dund's answer to the call waiting for it; a late one is
// dropped.
func (p *peerLink) reply(rep proto.PeerReply) {
	p.mu.Lock()
	ch := p.waiting[rep.RID]
	p.mu.Unlock()
	if ch != nil {
		ch <- rep
	}
}

func (p *peerLink) List(ctx context.Context) ([]dun.PeerSession, error) {
	rep, err := p.call(ctx, proto.PeerRequest{Op: "list"})
	if err != nil {
		return nil, err
	}
	out := make([]dun.PeerSession, 0, len(rep.Sessions))
	for _, s := range rep.Sessions {
		out = append(out, dun.PeerSession{ID: s.ID, Name: s.Name, Workspace: s.Workspace, Branch: s.Branch, Worktree: s.Worktree,
			State: s.State, Task: s.Task, Attached: s.Attached, You: s.You})
	}
	return out, nil
}

func (p *peerLink) Send(ctx context.Context, to, text string, wake bool) (dun.PeerSent, error) {
	rep, err := p.call(ctx, proto.PeerRequest{Op: "msg", To: to, Text: text, Wake: wake})
	if err != nil {
		return dun.PeerSent{}, err
	}
	return dun.PeerSent{ID: rep.ID, Name: rep.Name, Busy: rep.Busy, Started: rep.Started}, nil
}

// peerIn routes the peer frames the input reader gets: dund's reply to this
// engine's request, another session's message, and this session's loop
// count (schedule.go).
func peerIn(link *peerLink, h *dun.Harness, em *emitter) func(typ string, line []byte) {
	return func(typ string, line []byte) {
		switch typ {
		case "peer_reply":
			var rep proto.PeerReply
			if json.Unmarshal(line, &rep) == nil && link != nil {
				link.reply(rep)
			}
		case "peer":
			var m proto.PeerMsg
			if json.Unmarshal(line, &m) != nil {
				return
			}
			// Every client shows it, dim, naming the sender; the model reads
			// it labelled (dun.PeerLabel).
			em.emit(&proto.Notification{Kind: "peer", Text: m.FromName + " (" + m.From + "): " + m.Text})
			h.PeerMessage(m.From, m.FromName, m.Text, m.Wake)
		case "servers":
			// Another session wrote the global server file: re-read it now if
			// idle; a running turn's next one does at its start.
			// The clients get the new states quietly, as a server's activity
			// does: the splash and /mcp say the mode now.
			go func() {
				if h.FollowServersIfIdle(context.Background()) {
					em.emit(&proto.ServerEvent{Action: "activity", Servers: serversProto(h.Servers()),
						Tools: h.ToolNames(), Background: true})
				}
			}()
		}
	}
}
