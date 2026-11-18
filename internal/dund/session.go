package dund

import (
	"bufio"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/iodesystems/dun/internal/harness"
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
	// maxReplay is the longest backlog replayed event by event; past it a
	// subscriber gets a snapshot.
	maxReplay = 256
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
	// emptyStop is the idle stop for a session nothing was asked in.
	emptyStop = 10 * time.Minute
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
	persist      bool // keeps running when its last client leaves, and past the idle stop
	eng          Engine
	engDone      chan struct{} // closed when eng has exited
	running      bool
	stopping     bool // a stop was asked for; do not restart
	gaveUp       bool
	restarts     int
	restartStart time.Time
	// shown: an engine of this session already sent its conversation to
	// clients (or the session outlived a dund restart), so the next engine's
	// history is a restart, not a resume; switching: the next engine is on
	// another model (Restart). They make history's reason (attach).
	shown, switching bool
	claimed          time.Time // handed out by CreateOrReuse; see reuseHold

	// What the engine is doing, from its events: the idle stop must not end
	// a turn, a question waiting for a person, a job or a sub-agent. Its own
	// lock: the event reader must never wait on mu, which send holds while
	// writing to an engine that may itself be waiting for the reader.
	actMu      sync.Mutex
	lastActive time.Time
	inTurn     bool
	asking     bool
	// turnStated: this engine sends turn_state, and inTurn, asking and ask
	// follow it alone. Until then (an engine from before turn_state) they
	// are inferred from turn, done, error, ask, mount and answered.
	turnStated bool
	// ask is the open ask_user question (nil for a mount question): a
	// restart does not wait for it, it is saved and shown again (restored).
	ask *proto.Ask
	// restored is the question the last stop cut, saved in the registry
	// and shown to clients again (restoredEvs, appended to the log) with no
	// engine asking it: the person's answer, or interrupt, goes to the
	// agent as a message (upgrade.go restoredReply).
	restored    *proto.Ask
	restoredEvs []proto.Event
	jobs        []proto.JobInfo   // the running ones
	agents      []proto.AgentInfo // the running ones
	// agentDirs are the live (running or idle) sub-agents that work in a
	// tree of their own, for dun ps (procs.go).
	agentDirs []proto.ProcAgent
	// resume is the message a restart left for this session's agent: it was
	// working when dund stopped, and is woken with it on the next start
	// (persisted in the registry). Guarded by actMu, a leaf lock.
	resume string
	// noticed: told that the pending upgrade waits for it (upgrade.go
	// noticeBusy), so it is told again when the upgrade is done or off.
	noticed bool
	// queued are the automatic messages (automatic) that came while an
	// upgrade was pending: kept in the registry, sent after the restart, or
	// at once when the upgrade fails (USER 2026-11-17).
	queued []string
	// heldWake: the engine, held (proto.UpgradeHold), kept news that would
	// have started a turn (proto.WakeHeld); the session is woken for it
	// after the restart.
	heldWake bool
	// calls are the tool calls in flight, by call id, for the resume
	// message of a stop that cuts the turn.
	calls map[string]string
	// cutAgents are the sub-agents a stop cut mid-run: each is told to go
	// on with its task after the restart (registry).
	cutAgents []int
	// continued: the last start woke this session to go on with work a
	// stop cut, and its turn has not ended since. A stop that cuts that
	// turn again records resumeQuiet: the next start leaves a quiet note
	// and wakes nothing, so one cut turn gets one automatic continue, never
	// a loop. Both are in the registry.
	continued, resumeQuiet bool
	// worktree and branch are the worktree the engine reported working in
	// (empty: in place, in workspace), and lastUser the last message it
	// took: dun_list's worktree column and task (peer.go).
	worktree string
	branch   string
	lastUser string
	// personSeq counts the messages the person typed (user echoes that are
	// not dun's or dund's own, harness.PersonText): a peer ping-pong ends at
	// one (peer.go). notes is turn_state's notes_waiting, for dun ps.
	personSeq uint64
	notes     int
	netSince  int64 // turn_state's net_wait.since: waiting for the network

	// Engine adoption (adopt.go, docs/engine-adoption.md), under actMu.
	// stale: this engine was adopted and is to move to a new engine at its
	// next idle (an older build, or gateway grants that died with the old
	// dund), and why; live: its replay is over, so its state is current;
	// moving: the move is under way.
	stale        string
	live, moving bool
	// blockPid is an engine from before dund's restart that was not
	// adopted and may still run on this transcript: no engine starts for
	// the session while it lives (blocked), and blockWhy says why.
	blockPid int
	blockWhy string
	// fallback is what the next start does if this session's engine is not
	// adopted after an upgrade's restart: the resume record a stop would
	// have written (registry).
	fallback *fallbackRec
}

// track notes what an event says the engine is doing.
func (s *session) track(ev proto.Event) {
	s.actMu.Lock()
	defer s.actMu.Unlock()
	s.lastActive = time.Now()
	switch p := ev.Payload.(type) {
	case *proto.TurnState:
		s.turnStated = true
		s.inTurn = p.Phase != proto.PhaseIdle
		s.notes = p.NotesWaiting
		s.netSince = 0
		if p.NetWait != nil {
			s.netSince = p.NetWait.Since
		}
		s.asking = p.Ask != nil || p.Mount != nil
		s.ask = nil
		if p.Ask != nil {
			c := *p.Ask
			s.ask = &c
		}
	case *proto.Turn:
		if !s.turnStated { // fallback: an engine without turn_state
			s.inTurn = true
		}
	case *proto.Done, *proto.Error:
		// A question the turn asked ends with it, however it ended (an
		// interrupted ask_user sends no answered), as the clients take it.
		if !s.turnStated { // fallback: an engine without turn_state
			s.inTurn, s.asking, s.ask = false, false, nil
		}
		s.calls = nil
		// The continued turn ended on its own, or failed: a turn end the
		// person sees. dund's own stop interrupt leaves it set; a person's
		// interrupt clears it where it comes in (personEnded).
		if d, ok := p.(*proto.Done); !ok || d.Reason != "interrupted" {
			s.continued = false
		}
	case *proto.ToolCall:
		if s.calls == nil {
			s.calls = map[string]string{}
		}
		s.calls[p.CallID] = callLine(p)
	case *proto.ToolResult:
		delete(s.calls, p.CallID)
	case *proto.WakeHeld:
		s.heldWake = true
	case *proto.Ask:
		if !s.turnStated { // fallback: an engine without turn_state
			s.asking = true
			c := *p
			s.ask = &c
		}
		// The engine asks again, or something new: that question is the
		// open one now, as a client shows it.
		s.restored, s.restoredEvs = nil, nil
	case *proto.Mount:
		if !s.turnStated { // fallback: an engine without turn_state
			s.asking, s.ask = true, nil
		}
	case *proto.Answered:
		if !s.turnStated { // fallback: an engine without turn_state
			s.asking, s.ask = false, nil
		}
	case *proto.Workspace:
		// The in-place Worktree that a session without --worktree makes reports the workspace
		// itself: that is no worktree.
		s.worktree, s.branch = "", p.Branch
		if filepath.Clean(p.Path) != filepath.Clean(s.workspace) {
			s.worktree = p.Path
		}
	case *proto.UserEcho:
		s.lastUser = p.Content
		if harness.PersonText(p.Content) {
			s.personSeq++
		}
	case *proto.Jobs:
		s.jobs = nil
		for _, j := range p.Jobs {
			if j.State == "running" {
				s.jobs = append(s.jobs, j)
			}
		}
	case *proto.Agents:
		s.agents, s.agentDirs = nil, nil
		for _, a := range p.Agents {
			if a.State == "running" {
				s.agents = append(s.agents, a)
			}
			if (a.State == "running" || a.State == "idle") && a.Dir != "" {
				s.agentDirs = append(s.agentDirs, proto.ProcAgent{ID: a.ID, Dir: a.Dir})
			}
		}
	case *proto.EngineState:
		if p.State != "started" {
			// The next engine says again whether it sends turn_state.
			s.inTurn, s.asking, s.ask, s.jobs, s.agents, s.turnStated = false, false, nil, nil, nil, false
			s.agentDirs = nil
			s.calls = nil
		}
	}
}

// callLine names a tool call for a resume message: the tool and its
// arguments, clipped.
func callLine(p *proto.ToolCall) string {
	b, _ := json.Marshal(p.Args)
	return clipLine(p.Tool+" "+string(b), 200)
}

// occupied: a turn, a question, a job or a sub-agent. Called with actMu held.
func (s *session) occupied() bool {
	return s.inTurn || s.asking || len(s.jobs) > 0 || len(s.agents) > 0
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
	return running && !s.occupied() && time.Since(s.lastActive) >= d
}

// waiting is what a pending upgrade waits for in this session, one entry
// per thing, for it to name. An open ask_user question is not in it (Carl
// 2026-11-10): a stop saves it and the next dund asks it again
// (prepareStop); the turn around it waits only on the person.
func (s *session) waiting() []proto.UpgradeWait {
	s.mu.Lock()
	running := s.running
	s.mu.Unlock()
	if !running {
		return nil
	}
	s.actMu.Lock()
	defer s.actMu.Unlock()
	var out []proto.UpgradeWait
	switch {
	case s.asking && s.ask != nil:
	case s.asking: // a mount question: not saved, so it holds as a turn does
		out = append(out, proto.UpgradeWait{Session: s.id, What: "question", Hint: "answer it, or interrupt the turn"})
	case s.inTurn:
		out = append(out, proto.UpgradeWait{Session: s.id, What: "turn", Hint: "interrupt it"})
	}
	for _, j := range s.jobs {
		out = append(out, proto.UpgradeWait{Session: s.id, What: "job", ID: j.ID, Detail: j.Command,
			Hint: fmt.Sprintf("/jobs kill %d", j.ID)})
	}
	for _, a := range s.agents {
		out = append(out, proto.UpgradeWait{Session: s.id, What: "agent", ID: a.ID, Detail: a.Prompt,
			Hint: "interrupt the turn"})
	}
	return out
}

// openAsk is the open ask_user question, or nil.
func (s *session) openAsk() *proto.Ask {
	s.mu.Lock()
	running := s.running
	s.mu.Unlock()
	s.actMu.Lock()
	defer s.actMu.Unlock()
	if !running || !s.asking {
		return nil
	}
	return s.ask
}

// touch marks the session used now (a command, a subscribe).
func (s *session) touch() {
	s.actMu.Lock()
	s.lastActive = time.Now()
	s.actMu.Unlock()
}

func (s *session) isMember(principal string) bool { return slices.Contains(s.members, principal) }

func (s *session) summary() proto.SessionSummary {
	s.actMu.Lock()
	last := s.lastActive
	s.actMu.Unlock()
	if mt, ok := transcriptTime(s.workspace, s.id); ok {
		last = mt
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return proto.SessionSummary{LastActive: rfc3339(last), ID: s.id, Workspace: s.workspace, Owner: s.owner,
		Members: slices.Clone(s.members), Running: s.running, LastSeq: s.log.Last(), Persist: s.persist,
		Attached: s.log.Subscribers()}
}

// transcriptTime is when the session's transcript was last written: the
// activity that outlives a dund restart.
func transcriptTime(workspace, id string) (time.Time, bool) {
	st, err := os.Stat(harness.SessionFile(workspace, id))
	if err != nil {
		return time.Time{}, false
	}
	return st.ModTime(), true
}

func rfc3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// reusable reports whether CreateOrReuse may hand this session to a client
// starting with args: nobody attached, not just handed to another, same
// flags.
func (s *session) reusable(args []string) bool {
	if s.log.Subscribers() > 0 {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Since(s.claimed) >= reuseHold && slices.Equal(s.args, args)
}

// claim marks the session handed out, and used now: the empty-session
// reaper must not take it before its client subscribes.
func (s *session) claim() {
	s.mu.Lock()
	s.claimed = time.Now()
	s.mu.Unlock()
	s.touch()
}

func (s *session) persists() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.persist
}

// hasMessages reports whether the session's transcript, or an archive of it
// (/clear's, a recap's), holds a user message. A session without one is not
// worth keeping: nothing was asked in it.
func (s *session) hasMessages() bool { return askedIn(s.workspace, s.id) }

// askedIn is the same rule PruneSessions keeps transcripts by, so dund never
// drops a session the engine keeps (a variable: the tests' fake engine writes
// no transcript).
var askedIn = harness.SessionAsked

// readEvents appends every frame the engine writes until its stdout closes,
// then handles the exit.
func (s *session) readEvents(eng Engine, done chan struct{}, sc *bufio.Scanner, reason string) {
	for sc.Scan() {
		// The engine link's markers, in band (enginelink.go).
		switch string(sc.Bytes()) {
		case linkGapMark:
			s.reattachHistory()
			continue
		case proto.LinkLiveLine:
			s.actMu.Lock()
			s.live = true
			s.actMu.Unlock()
			s.checkStale()
			continue
		}
		if ev, ok := decodeEvent(sc.Bytes()); ok {
			// Why this engine replays the conversation: dund knows, the
			// engine does not (docs/session-events.md, "History reason").
			if h, ok := ev.Payload.(*proto.History); ok && h.Reason == "" {
				h.Reason = reason
			}
			if req, ok := ev.Payload.(*proto.PeerRequest); ok {
				go s.svc.handlePeer(s, eng, *req)
				continue
			}
			if _, ok := ev.Payload.(*proto.WakeHeld); ok { // dund's, not the clients'
				s.track(ev)
				s.svc.heldAfterStop(s)
				continue
			}
			cont := s.isContinued()
			s.track(ev)
			s.log.Append(ev)
			s.checkStale()
			switch ev.Payload.(type) {
			case *proto.Done, *proto.Error:
				s.svc.loopTurnEnded(s)
				if cont && !s.isContinued() {
					_ = s.svc.saveRegistry() // a crash now wakes it again
				}
			}
		}
	}
	err := eng.Wait()
	if errors.Is(err, errDetached) {
		// dund let go of it (Service.Detach): it runs on, for the next dund.
		close(done)
		return
	}
	s.svc.releaseEngine(eng) // its gateway clients (mcpgw.go)
	s.exited(eng, err)
	// What it was doing ended with it. dund's own "stopped" goes to the log,
	// not through track, so without this a turn cut by a stop stayed "in a
	// turn" until the next engine finished one (found live: a loop waited on
	// it, and busy() said so to dun_list too).
	s.track(proto.NewEvent(&proto.EngineState{State: "stopped"}))
	// Nothing of a turn outlives its engine, its queue included: the
	// engine cannot say so, dund does (proto.TurnState).
	s.log.Append(proto.NewEvent(idleTurn()))
	// An engine gone mid-turn (stopped by /exit, crashed) never sends that
	// turn's done: for a self-paced loop waiting on it, this is its end.
	s.svc.loopTurnEnded(s)
	close(done)
}

// idleTurn is the turn_state of a session with no engine turn.
func idleTurn() *proto.TurnState {
	return &proto.TurnState{Phase: proto.PhaseIdle, Queued: []proto.QueuedMsg{}}
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
	if err := s.blocked(); err != nil {
		return err
	}
	eng, err := s.svc.spawner.Spawn(s.workspace, s.spawnArgs())
	if err != nil {
		return err
	}
	s.attach(eng, newScanner(eng.Stdout()))
	return nil
}

// spawnArgs is how an engine is started on this conversation. --persist
// carries the setting to the engine, whose sub-agents follow it: a persisted
// session's running and idle children are restarted with it. Called with mu
// held.
func (s *session) spawnArgs() []string {
	args := append(slices.Clone(s.args), "--resume", s.id)
	if s.persist {
		args = append(args, "--persist")
	}
	return args
}

// attach makes eng this session's engine and starts reading it. Called with
// mu held.
func (s *session) attach(eng Engine, sc *bufio.Scanner) {
	s.eng, s.engDone = eng, make(chan struct{})
	s.running, s.stopping, s.gaveUp = true, false, false
	s.log.Append(proto.NewEvent(&proto.EngineState{State: "started"}))
	go s.readEvents(eng, s.engDone, sc, s.historyReason())
	if _, linked := eng.(*linkEngine); linked {
		return // not held: an upgrade adopts it (docs/engine-adoption.md)
	}
	if on, gen := s.svc.holdGen(); on {
		// Held from its start, as the running engines are. Not under mu: a
		// write may wait for the engine to read. Gen: should the upgrade
		// fail meanwhile, the failure's Off (gen+1) wins whatever the order.
		go func() { _ = s.sendLoaded(proto.NewCommand("", &proto.UpgradeHold{On: true, Gen: gen})) }()
	}
}

// personEnded notes a client's interrupt or stop: the person ended the turn
// dund continued, so a later stop that cuts another turn continues it again.
func (s *session) personEnded(c proto.Command) {
	switch c.Payload.(type) {
	case *proto.Interrupt, *proto.Stop:
		s.actMu.Lock()
		s.continued = false
		s.actMu.Unlock()
	}
}

func (s *session) isContinued() bool {
	s.actMu.Lock()
	defer s.actMu.Unlock()
	return s.continued
}

// historyReason is why the engine attached now replays the conversation,
// for its history event: the first engine resumes it; a later one restarts
// on what the clients have, or switches model (Restart). Called with mu held.
func (s *session) historyReason() string {
	reason := proto.HistoryResume
	switch {
	case s.switching:
		reason = proto.HistoryModelSwitch
	case s.shown:
		reason = proto.HistoryRestart
	}
	s.shown, s.switching = true, false
	return reason
}

// send writes a command to the engine, starting it first if need be. An
// answer or interrupt for a restored question is dund's to handle.
func (s *session) send(c proto.Command) error {
	if done, err := s.restoredReply(c); done {
		return err
	}
	s.touch()
	if err := s.ensureRunning(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.eng == nil {
		return errors.New("engine not running")
	}
	return s.write(c)
}

// sendIfRunning writes a command to a running engine and starts none.
func (s *session) sendIfRunning(c proto.Command) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.eng == nil || !s.running {
		return errors.New("engine not running")
	}
	return s.write(c)
}

// write puts one command on the engine's stdin. Called with mu held and an
// engine attached.
func (s *session) write(c proto.Command) error {
	c.CID, c.Session = "", "" // the engine's own frames carry neither
	b, err := json.Marshal(c)
	if err != nil {
		return err
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
		// Bounded too: a process outside the group (one that made its own)
		// can still hold stdout open, and a stop that never returns keeps
		// the pidfile and blocks the next dund from starting.
		select {
		case <-done:
		case <-time.After(stopGrace):
			log.Printf("dund: session %s: engine killed but its output never closed; going on without it", s.id)
		}
	}
}

// subscribe returns what a client needs to catch up after seq after of
// epoch epoch — the kept events, or a snapshot when they are gone or the seq
// is another log's (the service restarted since) — and the live stream. An
// empty epoch is this log's.
func (s *session) subscribe(after uint64, epoch string) (*proto.Event, []proto.Event, *proto.Subscription, error) {
	s.touch()
	s.mu.Lock()
	s.claimed = time.Time{} // its client is here: the subscription holds it now
	s.mu.Unlock()
	var replay []proto.Event
	var sub *proto.Subscription
	err := errEpoch
	if epoch == "" || epoch == s.log.Epoch() {
		replay, sub, err = s.log.Subscribe(after, subBuffer)
		if err == nil && len(replay) <= maxReplay {
			return nil, replay, sub, nil
		}
		if err == nil {
			// A long backlog (a turn streams an event per token) goes as a
			// snapshot: the conversation and the current state, not thousands
			// of deltas that only rebuild it.
			sub.Close()
			err = proto.ErrGap
		}
	}
	if err != errEpoch && !errors.Is(err, proto.ErrGap) && !errors.Is(err, proto.ErrFuture) {
		return nil, nil, nil, err
	}
	// The conversation and the seq it is current to, read so they agree: a
	// read during which the engine wrote an event is read again. What a
	// snapshot's items hold is then dropped from the replay after it
	// (afterSnapshot), so an entry is never in both.
	last, items := s.historyAt()
	// Last before State: a state event newer than last is sent twice (once
	// here, once replayed); the copy here is marked replayed, so a client
	// shows no note for it. One older than the state read would otherwise
	// be lost.
	state := s.log.State()
	for i := range state {
		state[i].Replayed = true
	}
	// A restored question is no engine event a snapshot keeps: it goes with
	// the state, so a client that reconnects after the restart shows it.
	s.actMu.Lock()
	if evs := s.restoredEvs; len(evs) > 0 {
		state = append(state, evs...)
		slices.SortFunc(state, func(a, b proto.Event) int { return cmp.Compare(a.Seq, b.Seq) })
	}
	s.actMu.Unlock()
	replay, sub, err = s.log.Subscribe(last, subBuffer)
	if err != nil {
		return nil, nil, nil, err
	}
	replay = afterSnapshot(items, replay)
	snap := proto.NewEvent(&proto.Snapshot{After: last, Items: historyItems(items), State: state})
	snap.Session, snap.Epoch, snap.TS = s.id, s.log.Epoch(), time.Now()
	return &snap, replay, sub, nil
}

// historyAt reads the conversation and the log's last seq so that they
// agree: when an event was written while the file was read, it reads again
// (a few times; a session that never pauses is read as it is, and
// afterSnapshot drops what repeats).
func (s *session) historyAt() (uint64, []harness.HistoryItem) {
	for try := 0; ; try++ {
		last := s.log.Last()
		items := harness.SessionHistory(harness.SessionFile(s.workspace, s.id))
		if s.log.Last() == last || try == 4 {
			return last, items
		}
	}
}

// afterSnapshot drops from the replay after a snapshot the events whose
// entries the snapshot's items already hold: a user message by its id, a
// tool call or result by its call_id, a reply by its id. The engine writes an entry and sends its event apart
// (either may come first), so without this an entry could show twice.
func afterSnapshot(items []harness.HistoryItem, replay []proto.Event) []proto.Event {
	users, calls, results, replies := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	lastReply := ""
	for _, it := range items {
		switch it.Kind {
		case "user":
			users[it.ID] = it.ID != ""
		case "tool_call":
			calls[it.CallID] = it.CallID != ""
		case "tool_result":
			results[it.CallID] = it.CallID != ""
		case "assistant":
			replies[it.ID] = it.ID != ""
			lastReply = it.Content
		}
	}
	out := replay[:0:0]
	for _, ev := range replay {
		switch p := ev.Payload.(type) {
		case *proto.UserEcho:
			if users[p.ID] {
				continue
			}
		case *proto.ToolCall:
			if calls[p.CallID] {
				continue
			}
		case *proto.ToolResult:
			if results[p.CallID] {
				continue
			}
		case *proto.Message:
			if replies[p.ID] {
				continue
			}
			// Fallback, for an engine older than reply ids: a reply equal to
			// the last one in the items is taken for it (a repeated reply
			// is dropped wrongly; ids fixed that).
			if p.ID == "" && lastReply != "" && p.Content == lastReply {
				continue
			}
		}
		out = append(out, ev)
	}
	return out
}

// errEpoch: a seq from another log than this one.
var errEpoch = errors.New("seq from another epoch")

func historyItems(items []harness.HistoryItem) []proto.HistoryItem {
	out := make([]proto.HistoryItem, 0, len(items))
	for _, it := range items {
		out = append(out, proto.HistoryItem{Kind: it.Kind, ID: it.ID, Queued: it.Queued, IsError: it.IsError, Content: it.Content, Tool: it.Tool, CallID: it.CallID, Args: it.Args, Agent: it.Agent})
	}
	return out
}
