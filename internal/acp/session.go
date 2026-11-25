package acp

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	sdk "github.com/coder/acp-go-sdk"

	"github.com/iodesystems/dun/proto"
)

// peer is what a session says to the editor: the ACP connection's agent
// side (a test's fake has the same two methods).
type peer interface {
	SessionUpdate(ctx context.Context, n sdk.SessionNotification) error
	RequestPermission(ctx context.Context, r sdk.RequestPermissionRequest) (sdk.RequestPermissionResponse, error)
}

// session is one ACP session: a dund session the bridge follows over its
// own link. dund owns the session's state; this keeps only what the
// translation needs: the prompts waiting for their turn to end, the
// question open, and the tool calls' working directory.
type session struct {
	id   string
	cwd  string
	ctx  context.Context // ends with the ACP connection
	peer peer
	caps caps // what the editor can take (caps.go)
	log  *slog.Logger
	dial func() (net.Conn, error)

	mu       sync.Mutex
	link     *link
	prompts  []*prompt
	cancel   bool            // a session/cancel waits for the turn's end
	streamed bool            // the reply's text came as tokens
	commands map[string]bool // dun's command names, for slash prompts
	ask      *proto.Ask      // a question asked as text: the next prompt answers it
	lastAsk  string          // the ask id handled last (ask and turn_state both carry it)
	perm     string          // the ask id a permission request is open for
	permStop context.CancelFunc
	seq      uint64 // the last event seen, to resubscribe after
	epoch    string
	snapped  bool // the first snapshot is in
	replay   bool // session/load: stream the first snapshot's history
	ready    chan struct{}
	closed   bool

	// The config options (config.go): what they are made from, the list
	// the editor has (cfgSent, once cfgLive), the set_config_options under
	// way and their commands' ids, and a channel closed at each change.
	cfg     config
	cfgSent string
	cfgLive bool
	setting int
	cfgAcks map[string]chan string
	cfgWake chan struct{}

	usage usageState // usage.go

	// untilSent counts the /until lines the bridge sent whose replies are
	// not shown; untilHeld: a turn ended while its run goes on, and its
	// prompts wait for the run's end (until.go).
	untilSent int
	untilHeld bool
}

func newSession(ctx context.Context, id, cwd string, p peer, log *slog.Logger, dial func() (net.Conn, error), replay bool) *session {
	return &session{id: id, cwd: cwd, ctx: ctx, peer: p, log: log, dial: dial,
		replay: replay, ready: make(chan struct{}), commands: map[string]bool{},
		cfgAcks: map[string]chan string{}, cfgWake: make(chan struct{})}
}

// update sends one session/update; a failed write is logged, as the
// connection's end is noticed elsewhere.
func (s *session) update(u sdk.SessionUpdate) {
	if err := s.peer.SessionUpdate(s.ctx, sdk.SessionNotification{SessionId: sdk.SessionId(s.id), Update: u}); err != nil {
		s.log.Debug("acp: session/update", "session", s.id, "err", err)
	}
}

func (s *session) text(t string) { s.update(sdk.UpdateAgentMessageText(t)) }

// send writes a command to the session's engine through dund.
func (s *session) send(cid string, payload any) error {
	s.mu.Lock()
	l := s.link
	s.mu.Unlock()
	if l == nil {
		return errors.New("not connected to dund")
	}
	return l.send(s.id, cid, payload)
}

// run follows the session until the ACP connection ends: each event
// translated, and after a lost link (dund restarted) a new one, subscribed
// after the last event seen.
func (s *session) run(l *link) {
	for {
		err := l.read(s.handle)
		l.close()
		s.mu.Lock()
		closed := s.closed
		s.link = nil
		s.mu.Unlock()
		if closed || s.ctx.Err() != nil {
			return
		}
		s.log.Info("acp: lost dund, reconnecting", "session", s.id, "err", err)
		if l = s.reconnect(); l == nil {
			s.failAll(errors.New("lost the connection to dund: " + err.Error()))
			return
		}
	}
}

// reconnectFor is how long a lost link is retried before the waiting
// prompts fail: a dund upgrade restarts within seconds.
const reconnectFor = 2 * time.Minute

func (s *session) reconnect() *link {
	deadline := time.Now().Add(reconnectFor)
	for wait := 200 * time.Millisecond; time.Now().Before(deadline); wait = min(wait*2, 5*time.Second) {
		select {
		case <-s.ctx.Done():
			return nil
		case <-time.After(wait):
		}
		c, err := s.dial()
		if err != nil {
			continue
		}
		l := newLink(c)
		s.mu.Lock()
		after, epoch := s.seq, s.epoch
		s.mu.Unlock()
		if _, err := l.call(s.id, &proto.Subscribe{After: after, Epoch: epoch}); err != nil {
			l.close()
			continue
		}
		s.mu.Lock()
		s.link = l
		s.mu.Unlock()
		return l
	}
	return nil
}

// close ends the session's link; dund keeps the session.
func (s *session) close() {
	s.mu.Lock()
	s.closed = true
	l := s.link
	s.mu.Unlock()
	if l != nil {
		_ = l.send(s.id, "", &proto.Detach{})
		l.close()
	}
}

// handle translates one event from dund.
func (s *session) handle(ev proto.Event) {
	if ev.Session != "" && ev.Session != s.id {
		return
	}
	s.mu.Lock()
	if ev.Seq > 0 {
		s.seq, s.epoch = ev.Seq, ev.Epoch
	}
	s.mu.Unlock()
	switch p := ev.Payload.(type) {
	case *proto.Ack:
		s.onAck(ev.Re, p)
	case *proto.Snapshot:
		s.onSnapshot(ev, p)
	case *proto.Token:
		s.mu.Lock()
		s.streamed = true
		s.mu.Unlock()
		s.text(p.Text)
	case *proto.Reasoning:
		s.update(sdk.UpdateAgentThoughtText(p.Text))
	case *proto.Message:
		s.mu.Lock()
		streamed := s.streamed
		s.streamed = false
		s.mu.Unlock()
		// A reply that came whole, not as tokens, is sent whole.
		if p.Role == "assistant" && !streamed && strings.TrimSpace(p.Content) != "" {
			s.text(p.Content)
		}
	case *proto.ToolCall:
		if p.CallID != "" {
			s.update(startCall(p, s.cwd))
		}
	case *proto.ToolResult:
		if p.CallID != "" {
			s.update(endCall(p))
		}
	case *proto.Plan:
		s.update(planUpdate(s.caps, planEntries(p)))
		s.slashReply(p.Message)
	case *proto.Commands:
		s.onCommands(p)
	case *proto.Settings:
		s.onConfig(func(c *config) { c.settings = p })
	case *proto.Models:
		s.onConfig(func(c *config) { c.providers, c.models = p.Providers, true })
	case *proto.Ask:
		s.onAsk(p)
	case *proto.Answered:
		s.onAnswered(p)
	case *proto.TurnState:
		if p.Phase == proto.PhaseWaitingAnswer && p.Ask != nil {
			s.onAsk(p.Ask)
		}
	case *proto.UserEcho:
		s.onEcho(p)
	case *proto.Delivered:
		s.onDelivered(p.IDs)
	case *proto.Withdrawn:
		s.onWithdrawn(p.IDs)
	case *proto.Request:
		s.onRound(p)
	case *proto.Usage:
		s.onUsage(p)
	case *proto.Turn:
		s.onTurn()
	case *proto.Done:
		s.onDone(p)
	case *proto.Error:
		s.onError(p)
	case *proto.ImageRefused:
		s.text(p.Message)
	case *proto.Until:
		s.onUntil(p)
	case *proto.ControlEvent:
		if p.ID == "until" && s.ownUntilReply() {
			break
		}
		s.slashReply(p.Message)
	case *proto.ServerEvent:
		if !p.Background {
			s.slashReply(p.Message)
		}
	case *proto.EngineState:
		if p.State == "gave_up" {
			s.failAll(errors.New("dun's engine for this session keeps crashing: " + p.Reason))
		}
	}
}

// onAck: an error on a command this session sent fails its prompt; dund
// dropping a slow subscriber is answered by subscribing again after the
// last event seen.
func (s *session) onAck(re string, a *proto.Ack) {
	if a.Error == "" {
		return
	}
	if re != "" && s.cfgAck(re, a.Error) {
		return
	}
	if strings.HasPrefix(a.Error, "dropped:") {
		s.mu.Lock()
		after, epoch := s.seq, s.epoch
		s.mu.Unlock()
		_ = s.send("", &proto.Subscribe{After: after, Epoch: epoch})
		return
	}
	if re != "" && s.failCID(re, errors.New(a.Error)) {
		return
	}
	s.log.Warn("acp: dund", "session", s.id, "error", a.Error)
}

// onSnapshot: the first one is the session as it is when the bridge
// subscribes (it asks for one); session/load streams its history. A later
// one stands in for events the link missed: its state is applied and a
// turn it shows over ends the prompts it held.
func (s *session) onSnapshot(ev proto.Event, sn *proto.Snapshot) {
	s.mu.Lock()
	first := !s.snapped
	s.snapped = true
	if sn.After > s.seq {
		s.seq, s.epoch = sn.After, ev.Epoch
	}
	replay := first && s.replay
	s.mu.Unlock()
	if replay {
		for _, u := range replayItems(sn.Items, s.cwd) {
			s.update(u)
		}
	}
	for _, st := range sn.State {
		switch p := st.Payload.(type) {
		case *proto.Commands:
			s.onCommands(p)
		case *proto.Settings:
			s.onConfig(func(c *config) { c.settings = p })
		case *proto.Usage:
			s.loadUsage(p, replay)
		case *proto.Plan:
			s.update(planUpdate(s.caps, planEntries(p)))
		case *proto.Until:
			s.onConfig(func(c *config) { c.until = p })
		case *proto.TurnState:
			if p.Phase == proto.PhaseWaitingAnswer && p.Ask != nil {
				s.onAsk(p.Ask)
			}
			if !first && p.Phase == proto.PhaseIdle && len(p.Queued) == 0 {
				s.endTurn(sdk.StopReasonEndTurn, nil)
			}
		}
	}
	if first {
		close(s.ready)
	}
}

func (s *session) onCommands(c *proto.Commands) {
	s.mu.Lock()
	s.commands = map[string]bool{}
	for _, it := range c.Items {
		s.commands[it.Name] = true
	}
	s.mu.Unlock()
	s.onConfig(func(cfg *config) { cfg.effort = effortSpec(c) })
	s.update(sdk.SessionUpdate{AvailableCommandsUpdate: &sdk.SessionAvailableCommandsUpdate{AvailableCommands: availableCommands(c)}})
}

// isCommand reports whether a prompt is one of dun's slash commands.
func (s *session) isCommand(text string) bool {
	if !strings.HasPrefix(text, "/") {
		return false
	}
	name, _, _ := strings.Cut(strings.TrimPrefix(text, "/"), " ")
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.commands[strings.ToLower(strings.TrimSpace(name))]
}
