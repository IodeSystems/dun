package dund

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/iodesystems/dun/proto"
)

// Listeners (dun msg --name, proto/msg.go): a name a client holds while its
// connection lasts. In memory only (owner, 2026-11-04): a dund restart ends
// every listener, and its client must listen again.

// mailboxCap is how many messages wait for a listener that reads slowly; past
// it the oldest go, and the listener is told how many.
const mailboxCap = 200

// cliName is the sender of a message sent with no listener token.
const cliName = "cli"

type mailbox struct {
	owner, name, token string
	in                 chan proto.MsgIn
}

// put queues m, dropping the oldest when the box is full; it reports how
// many it dropped.
func (b *mailbox) put(m proto.MsgIn) int {
	dropped := 0
	for {
		select {
		case b.in <- m:
			return dropped
		default:
		}
		select {
		case <-b.in:
			dropped++
		default:
		}
	}
}

// Listen claims name for principal.
func (s *Service) Listen(principal, name string) (*mailbox, error) {
	name = strings.TrimSpace(name)
	switch {
	case name == "":
		return nil, errors.New("a listener needs a name")
	case name == cliName:
		return nil, fmt.Errorf("%q is the name of messages sent with no token", cliName)
	case strings.ContainsAny(name, " \t\n>"):
		return nil, errors.New("a listener's name has no spaces and no '>'")
	}
	for _, p := range s.Peers(principal, "") {
		if p.Name == name || p.ID == name {
			return nil, fmt.Errorf("name %s is in use: a live session", name)
		}
	}
	tok := make([]byte, 16)
	_, _ = rand.Read(tok)
	b := &mailbox{owner: principal, name: name, token: hex.EncodeToString(tok), in: make(chan proto.MsgIn, mailboxCap)}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.boxes == nil {
		s.boxes = map[[2]string]*mailbox{}
	}
	key := [2]string{principal, name}
	if _, ok := s.boxes[key]; ok {
		return nil, fmt.Errorf("name %s is in use: another listener has it", name)
	}
	s.boxes[key] = b
	return b, nil
}

// Unlisten frees b's name; its token stops working.
func (s *Service) Unlisten(b *mailbox) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.boxes[[2]string{b.owner, b.name}] == b {
		delete(s.boxes, [2]string{b.owner, b.name})
	}
}

func (s *Service) box(principal, name string) *mailbox {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.boxes[[2]string{principal, name}]
}

// Listeners is principal's listener names, sorted.
func (s *Service) Listeners(principal string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for k := range s.boxes {
		if k[0] == principal {
			out = append(out, k[1])
		}
	}
	slices.Sort(out)
	return out
}

// listenerRows are the listeners as dun_list rows.
func (s *Service) listenerRows(principal string) []proto.PeerSession {
	var out []proto.PeerSession
	for _, n := range s.Listeners(principal) {
		out = append(out, proto.PeerSession{ID: n, Name: n, State: "listener (dun msg)"})
	}
	return out
}

// deliverBox puts text from fromName in principal's listener to, if there
// is one; ok false means there is none.
func (s *Service) deliverBox(principal, from, fromName, to, text string) (bool, error) {
	b := s.box(principal, strings.TrimSpace(to))
	if b == nil {
		return false, nil
	}
	if !s.peerAllow(from, "msg:"+b.name, time.Now()) {
		return true, fmt.Errorf("rate limit: at most %d messages a minute to %s", peerRateMax, b.name)
	}
	if n := b.put(proto.MsgIn{From: fromName, Text: text}); n > 0 {
		b.put(proto.MsgIn{From: "dun", Text: fmt.Sprintf("%d older messages dropped: this listener read too slowly", n)})
	}
	return true, nil
}

// MsgSend sends text to a session or a listener, as the listener token
// names, else as "cli". It returns what happened, in words.
func (s *Service) MsgSend(principal string, p proto.MsgSend) (proto.MsgSent, error) {
	to, text := strings.TrimSpace(p.To), p.Text
	if to == "" || strings.TrimSpace(text) == "" {
		return proto.MsgSent{}, errors.New("dun msg needs a name to send to and a message")
	}
	from, fromName := "msg:"+cliName, cliName
	if p.Token != "" {
		var b *mailbox
		s.mu.Lock()
		for k, mb := range s.boxes {
			if k[0] == principal && mb.token == p.Token {
				b = mb
			}
		}
		s.mu.Unlock()
		if b == nil {
			return proto.MsgSent{}, errors.New("no listener has that token: it ended when its dun msg --name stopped")
		}
		from, fromName = "msg:"+b.name, b.name
	}
	if ok, err := s.deliverBox(principal, from, fromName, to, text); ok {
		if err != nil {
			return proto.MsgSent{}, err
		}
		return proto.MsgSent{To: to, Detail: "listener"}, nil
	}
	live := s.Peers(principal, "")
	var dst *proto.PeerSession
	var named []*proto.PeerSession
	for i := range live {
		if live[i].ID == to {
			dst = &live[i]
		}
		if live[i].Name == to {
			named = append(named, &live[i])
		}
	}
	if dst == nil && len(named) == 1 {
		dst = named[0]
	}
	if dst == nil {
		if len(named) > 1 {
			return proto.MsgSent{}, fmt.Errorf("%q names %d sessions — use one of: %s", to, len(named), candidates(named))
		}
		return proto.MsgSent{}, fmt.Errorf("no live session or listener %q (dun msg --who lists them)", to)
	}
	ses, err := s.Get(principal, dst.ID)
	if err != nil {
		return proto.MsgSent{}, err
	}
	if !s.peerAllow(from, dst.ID, time.Now()) {
		return proto.MsgSent{}, fmt.Errorf("rate limit: at most %d messages a minute to %s", peerRateMax, dst.Name)
	}
	wake := !p.NoWake
	busy := ses.busy()
	if err := ses.sendLoaded(proto.NewCommand("", &proto.PeerMsg{From: from, FromName: fromName, Text: text, Wake: wake})); err != nil {
		return proto.MsgSent{}, fmt.Errorf("%s: %w", dst.Name, err)
	}
	detail := "idle, not woken"
	switch {
	case busy:
		detail = "busy: it reads the message in its turn"
	case wake:
		detail = "idle: woken"
	}
	return proto.MsgSent{To: dst.Name, Detail: detail}, nil
}
