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
// side.
type peer interface {
	SessionUpdate(ctx context.Context, n sdk.SessionNotification) error
	RequestPermission(ctx context.Context, r sdk.RequestPermissionRequest) (sdk.RequestPermissionResponse, error)
	CreateElicitation(ctx context.Context, r sdk.CreateElicitationRequest) (sdk.CreateElicitationResponse, error)
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
	// suspended: the engine stopped with a question open (done suspended):
	// the prompt it holds is not over, the next engine asks again.
	suspended bool
	lastAsk   string // the ask id handled last (ask and turn_state both carry it)
	askCall   string // that ask's call id: its result in the history closes it
	perm      string // the ask id a permission request is open for
	permStop  context.CancelFunc
	seq       uint64 // the last event seen, to resubscribe after
	epoch     string
	snapped   bool // the first snapshot is in
	replay    bool // session/load: stream the first snapshot's history
	ready     chan struct{}
	closed    bool

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

	plan []sdk.PlanEntry // the last plan sent: resent at each prompt's start and end

	// untilSent counts the /until lines the bridge sent whose replies are
	// not shown; untilHeld: a turn ended while its run goes on, and its
	// prompts wait for the run's end (until.go).
	untilSent int
	untilHeld bool

	// gap: a snapshot stood in for events the link missed (dund restarted,
	// or dropped the link), and the prompts in a turn may have lost their
	// turn's done. The engine's idle state ends them (gapIdle).
	gap bool
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

// sendPlan sends dun's plan to the editor and keeps its entries.
func (s *session) sendPlan(p *proto.Plan) {
	e := planEntries(p)
	s.mu.Lock()
	s.plan = e
	s.mu.Unlock()
	s.update(planUpdate(s.caps, s.id, e))
}

// resendPlan sends the last plan again, when there is one: an editor that
// clears its plan per turn (JetBrains Air) shows it again. The editor
// replaces its plan at each update, so a resend changes nothing else.
func (s *session) resendPlan() {
	s.mu.Lock()
	e := s.plan
	s.mu.Unlock()
	if len(e) > 0 {
		s.update(planUpdate(s.caps, s.id, e))
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
		s.sendPlan(p)
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
		s.gapIdle(p)
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
	case *proto.Retry:
		// A wait on the provider shows as a thought: the editor has no
		// banner, and a silent wait of minutes looks like a hang.
		if p.Text != "" {
			s.update(sdk.UpdateAgentThoughtText(p.Text + "\n"))
		}
	case *proto.ImageRefused:
		s.text(p.Message)
	case *proto.Until:
		s.onUntil(p)
	case *proto.ControlEvent:
		if p.ID == "until" && s.ownUntilReply() {
			break
		}
		if p.ID == "interrupt" && s.cancelIdle() {
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
// one stands in for events the link missed (a gap): its state is applied,
// and an engine it shows idle ends the prompts held (gapIdle). A snapshot
// of another epoch is a new log (dund restarted): its seqs start again, so
// seq and epoch are taken from it whatever the seq.
func (s *session) onSnapshot(ev proto.Event, sn *proto.Snapshot) {
	s.mu.Lock()
	first := !s.snapped
	s.snapped = true
	newLog := ev.Epoch != s.epoch
	if newLog || sn.After > s.seq {
		s.seq, s.epoch = sn.After, ev.Epoch
	}
	replay := first && s.replay
	s.mu.Unlock()
	if replay {
		for _, u := range replayItems(sn.Items, s.cwd) {
			s.update(u)
		}
	}
	var turn *proto.TurnState
	var engine *proto.EngineState
	until := false
	for _, st := range sn.State {
		switch p := st.Payload.(type) {
		case *proto.Commands:
			s.onCommands(p)
		case *proto.Settings:
			s.onConfig(func(c *config) { c.settings = p })
		case *proto.Usage:
			s.loadUsage(p, replay)
		case *proto.Plan:
			s.sendPlan(p)
		case *proto.Until:
			until = true
			s.onConfig(func(c *config) { c.until = p })
		case *proto.EngineState:
			engine = p
		case *proto.TurnState:
			turn = p
			if p.Phase == proto.PhaseWaitingAnswer && p.Ask != nil {
				s.onAsk(p.Ask)
			}
		}
	}
	if first {
		close(s.ready)
		return
	}
	s.afterGap(sn, turn, engine, newLog, until)
}

// afterGap: a later snapshot. The prompts in a turn are marked as held by
// the gap, and the engine's state says whether that turn is over. An
// engine a new dund adopted (docs/engine-adoption.md) may be in a turn the
// new log has no turn_state for yet: its next turn_state or done says.
// Any other engine with none is idle: an engine says turn_state when a
// turn starts, and one that starts is idle (a new dund's log, an engine
// started since).
func (s *session) afterGap(sn *proto.Snapshot, turn *proto.TurnState, engine *proto.EngineState, newLog, until bool) {
	adopted := engine != nil && engine.State == "started" && engine.Reason == "adopted"
	if newLog && !until && !adopted {
		// A new engine and no until event: the run that held prompts
		// (untilHeld) ended with the old one.
		s.onConfig(func(c *config) {
			if c.until != nil && c.until.Running {
				u := *c.until
				u.Running = false
				c.until = &u
			}
		})
	}
	s.mu.Lock()
	// A question the restart suspended whose call has its result in the
	// history was answered while the bridge was away: no engine asks it
	// again.
	if s.suspended && s.askCall != "" && resultIn(sn.Items, s.askCall) {
		s.suspended = false
	}
	for _, p := range s.prompts {
		if p.inTurn {
			p.gap, s.gap = true, true
		}
	}
	s.mu.Unlock()
	if turn == nil && !adopted {
		turn = &proto.TurnState{Phase: proto.PhaseIdle}
	}
	if turn != nil {
		s.gapIdle(turn)
	}
}

// resultIn reports whether the history holds call's result.
func resultIn(items []proto.HistoryItem, call string) bool {
	for _, it := range items {
		if it.Kind == "tool_result" && it.CallID == call {
			return true
		}
	}
	return false
}

// gapIdle: after a gap, the engine idle with nothing queued and no
// question open ends the prompts the gap held (end_turn), from the
// snapshot or from a turn_state that comes later. Not while a question the
// restart suspended waits for the next engine to ask it again, nor while a
// stop condition's run goes on (its until event answers them). A turn
// started since the gap ends them with its done instead (onTurn).
func (s *session) gapIdle(ts *proto.TurnState) {
	if ts.Phase != proto.PhaseIdle || len(ts.Queued) > 0 || ts.Ask != nil || ts.Mount != nil {
		return
	}
	s.mu.Lock()
	ok := s.gap && !s.suspended && !(s.untilHeld && s.untilRunning())
	s.mu.Unlock()
	if ok {
		s.endPrompts(outcome{stop: sdk.StopReasonEndTurn}, func(p *prompt) bool { return p.gap })
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
