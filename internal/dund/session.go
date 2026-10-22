package dund

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"time"

	"github.com/iodesystems/dun"
	"github.com/iodesystems/dun/proto"
)

// Engine deaths worth surviving are the ones a session can outlive: a turn
// that took the process down, an OOM, a bad build. A crash loop is not —
// restarting forever would hide the failure — so restarts are capped per
// window, then reported. The same numbers the TUI used when it supervised.
const (
	restartMax    = 3
	restartWindow = 2 * time.Minute
	subBuffer     = 4096
)

// Variables so tests can shorten them.
var (
	restartDelay = time.Second
	stopGrace    = 10 * time.Second
	logCapacity  = 20000
	// idleStop is how long an engine nobody follows and nothing occupies
	// keeps running. Stopping it loses nothing: the next command or
	// subscribe starts it again on the same conversation.
	idleStop = 30 * time.Minute
)

// session is one conversation: its engine, its event log, who may see it.
type session struct {
	svc       *Service
	id        string
	workspace string
	owner     string
	members   []string
	args      []string
	log       *proto.Log

	mu           sync.Mutex
	eng          Engine
	engDone      chan struct{} // closed when eng has exited
	running      bool
	stopping     bool // a stop was asked for; do not restart
	gaveUp       bool
	restarts     int
	restartStart time.Time

	// What the engine is doing, from its events: the idle stop must not end
	// a turn, a question waiting for a person, a job or a sub-agent. Its own
	// lock: the event reader must never wait on mu, which send holds while
	// writing to an engine that may itself be waiting for the reader.
	actMu      sync.Mutex
	lastActive time.Time
	inTurn     bool
	asking     bool
	jobsBusy   bool
	agentsBusy bool
}

// track notes what an event says the engine is doing.
func (s *session) track(ev proto.Event) {
	s.actMu.Lock()
	defer s.actMu.Unlock()
	s.lastActive = time.Now()
	switch p := ev.Payload.(type) {
	case *proto.Turn:
		s.inTurn = true
	case *proto.Done, *proto.Error:
		s.inTurn = false
	case *proto.Ask, *proto.Mount:
		s.asking = true
	case *proto.Answered:
		s.asking = false
	case *proto.Jobs:
		s.jobsBusy = false
		for _, j := range p.Jobs {
			if j.State == "running" {
				s.jobsBusy = true
			}
		}
	case *proto.Agents:
		s.agentsBusy = false
		for _, a := range p.Agents {
			if a.State == "running" || a.Blocked {
				s.agentsBusy = true
			}
		}
	case *proto.EngineState:
		if p.State != "started" {
			s.inTurn, s.asking, s.jobsBusy, s.agentsBusy = false, false, false, false
		}
	}
}

// idle reports whether the engine runs with no one following it and nothing
// to do, for at least d.
func (s *session) idle(d time.Duration) bool {
	if s.log.Subscribers() > 0 {
		return false
	}
	s.mu.Lock()
	running := s.running
	s.mu.Unlock()
	s.actMu.Lock()
	defer s.actMu.Unlock()
	return running && !s.inTurn && !s.asking && !s.jobsBusy && !s.agentsBusy &&
		time.Since(s.lastActive) >= d
}

// busy reports whether the engine is doing something a restart would cut
// short: a turn, a question to a person, a job, a sub-agent.
func (s *session) busy() bool {
	s.mu.Lock()
	running := s.running
	s.mu.Unlock()
	s.actMu.Lock()
	defer s.actMu.Unlock()
	return running && (s.inTurn || s.asking || s.jobsBusy || s.agentsBusy)
}

// touch marks the session used now (a command, a subscribe).
func (s *session) touch() {
	s.actMu.Lock()
	s.lastActive = time.Now()
	s.actMu.Unlock()
}

func (s *session) isMember(principal string) bool { return slices.Contains(s.members, principal) }

func (s *session) summary() proto.SessionSummary {
	s.mu.Lock()
	defer s.mu.Unlock()
	return proto.SessionSummary{ID: s.id, Workspace: s.workspace, Owner: s.owner,
		Members: slices.Clone(s.members), Running: s.running, LastSeq: s.log.Last()}
}

// readEvents appends every frame the engine writes until its stdout closes,
// then handles the exit.
func (s *session) readEvents(eng Engine, done chan struct{}, sc *bufio.Scanner) {
	for sc.Scan() {
		if ev, ok := decodeEvent(sc.Bytes()); ok {
			s.track(ev)
			s.log.Append(ev)
		}
	}
	err := eng.Wait()
	s.exited(eng, err)
	close(done)
}

func decodeEvent(line []byte) (proto.Event, bool) {
	var ev proto.Event
	if json.Unmarshal(line, &ev) != nil || ev.Type == "" {
		return ev, false
	}
	return ev, true
}

func newScanner(r io.Reader) *bufio.Scanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	return sc
}

func (s *session) exited(eng Engine, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.eng != eng { // an engine already replaced
		return
	}
	s.eng, s.running = nil, false
	reason := "engine exited"
	if err != nil {
		reason = "engine exited: " + err.Error()
	}
	if s.stopping || s.svc.isClosing() {
		s.log.Append(proto.NewEvent(&proto.EngineState{State: "stopped"}))
		return
	}
	if time.Since(s.restartStart) > restartWindow {
		s.restarts, s.restartStart = 0, time.Now()
	}
	if s.restarts >= restartMax {
		s.gaveUp = true
		s.log.Append(proto.NewEvent(&proto.EngineState{State: "gave_up", Reason: reason, Restarts: s.restarts}))
		return
	}
	s.restarts++
	s.log.Append(proto.NewEvent(&proto.EngineState{State: "restarting", Reason: reason, Restarts: s.restarts}))
	time.AfterFunc(restartDelay, func() {
		s.mu.Lock()
		stopped := s.stopping
		s.mu.Unlock()
		if stopped {
			return
		}
		if err := s.ensureRunning(); err != nil {
			s.log.Append(proto.NewEvent(&proto.EngineState{State: "gave_up", Reason: err.Error()}))
		}
	})
}

// ensureRunning starts the engine, reattached to this conversation, if it is
// not running. A session that gave up is started again: a person asking for
// it is the retry.
func (s *session) ensureRunning() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return nil
	}
	if s.svc.isClosing() {
		return errors.New("service is shutting down")
	}
	eng, err := s.svc.spawner.Spawn(s.workspace, append(slices.Clone(s.args), "--resume", s.id))
	if err != nil {
		return err
	}
	s.attach(eng, newScanner(eng.Stdout()))
	return nil
}

// attach makes eng this session's engine and starts reading it. Called with
// mu held.
func (s *session) attach(eng Engine, sc *bufio.Scanner) {
	s.eng, s.engDone = eng, make(chan struct{})
	s.running, s.stopping, s.gaveUp = true, false, false
	s.log.Append(proto.NewEvent(&proto.EngineState{State: "started"}))
	go s.readEvents(eng, s.engDone, sc)
}

// send writes a command to the engine, starting it first if need be.
func (s *session) send(c proto.Command) error {
	s.touch()
	if err := s.ensureRunning(); err != nil {
		return err
	}
	c.CID, c.Session = "", "" // the engine's own frames carry neither
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.eng == nil {
		return errors.New("engine not running")
	}
	if _, err := s.eng.Stdin().Write(append(b, '\n')); err != nil {
		return fmt.Errorf("send to engine: %w", err)
	}
	return nil
}

// stop asks the engine to end and waits for it, killing it after a grace
// period. The session stays registered; the next command starts it again.
func (s *session) stop() {
	s.mu.Lock()
	eng, done := s.eng, s.engDone
	s.stopping = true
	s.mu.Unlock()
	if eng == nil {
		return
	}
	_, _ = eng.Stdin().Write([]byte(`{"type":"stop"}` + "\n"))
	select {
	case <-done:
	case <-time.After(stopGrace):
		eng.Kill()
		<-done
	}
}

// subscribe returns what a client needs to catch up after seq after — the
// kept events, or a snapshot when they are gone — and the live stream.
func (s *session) subscribe(after uint64) (*proto.Event, []proto.Event, *proto.Subscription, error) {
	s.touch()
	replay, sub, err := s.log.Subscribe(after, subBuffer)
	if err == nil {
		return nil, replay, sub, nil
	}
	if !errors.Is(err, proto.ErrGap) && !errors.Is(err, proto.ErrFuture) {
		return nil, nil, nil, err
	}
	// Last before State: a state event newer than last is sent twice (once
	// here, once replayed), which is harmless; one older than the state read
	// would otherwise be lost.
	last := s.log.Last()
	state := s.log.State()
	items := dun.SessionHistory(dun.SessionFile(s.workspace, s.id))
	replay, sub, err = s.log.Subscribe(last, subBuffer)
	if err != nil {
		return nil, nil, nil, err
	}
	snap := proto.NewEvent(&proto.Snapshot{After: last, Items: historyItems(items), State: state})
	snap.Session, snap.TS = s.id, time.Now()
	return &snap, replay, sub, nil
}

func historyItems(items []dun.HistoryItem) []proto.HistoryItem {
	out := make([]proto.HistoryItem, 0, len(items))
	for _, it := range items {
		out = append(out, proto.HistoryItem{Kind: it.Kind, Content: it.Content, Tool: it.Tool, CallID: it.CallID, Args: it.Args})
	}
	return out
}
