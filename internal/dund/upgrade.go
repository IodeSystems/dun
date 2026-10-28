package dund

import (
	"errors"
	"fmt"
	"log"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/iodesystems/dun"
	"github.com/iodesystems/dun/proto"
)

// A coordinated upgrade. When a new build is in place (dund's own binary was
// replaced, or a client of another build asked), dund does not restart at
// once: a restart stops every engine, which would cut a turn, a question, a
// job or a sub-agent short. It becomes pending instead, tells every
// connection what it waits for (proto.UpgradeStatus), and restarts when that
// is done — or when someone says "now", or after upgradeGrace. A restart that
// cuts work short interrupts the turns with the reason, records in the
// registry what each session was doing, and the next dund wakes those
// sessions with a message that says what happened (resumeFlagged). Other
// planned stops (SIGTERM, a service restart) record and resume the same way
// (PrepareStop).

// Variables so tests can shorten them.
var (
	upgradePoll  = 500 * time.Millisecond // how often the waiting list is read
	upgradeGrace = 10 * time.Minute       // pending this long, it restarts anyway
	// interruptWait is how long a stop waits for an interrupted turn to end
	// (to record its partial reply and its note) before stopping the engine.
	interruptWait = 10 * time.Second
	// crashWindow: only a transcript written this recently is taken as cut
	// by a crash, so a start does not wake sessions that died long ago.
	crashWindow = 30 * time.Minute
)

// resumePrefix starts the message a woken session's agent gets; clients
// show such a user message as automatic, not as the person's.
const resumePrefix = "[automatic] "

type upgrader struct {
	mu       sync.Mutex
	st       *proto.UpgradeStatus // nil: none pending
	since    time.Time
	now      bool
	kick     chan struct{}
	ready    chan string
	watchers map[int]func(proto.UpgradeStatus)
	nextW    int
	// install, when set, puts the new binary in place at the quiet point,
	// before anything is interrupted (a service's dund: SetUpgradeInstall).
	install func(exe, version string) error
}

func (u *upgrader) init() {
	u.kick, u.ready = make(chan struct{}, 1), make(chan string, 1)
	u.watchers = map[int]func(proto.UpgradeStatus){}
}

// snapshot is a copy of the status, or nil. Called with mu held.
func (u *upgrader) snapshot() *proto.UpgradeStatus {
	if u.st == nil {
		return nil
	}
	c := *u.st
	c.Waiting = slices.Clone(u.st.Waiting)
	return &c
}

func (u *upgrader) poke() {
	select {
	case u.kick <- struct{}{}:
	default:
	}
}

// notify hands st to every watcher, in the caller's goroutine.
func (s *Service) notifyUpgrade(st proto.UpgradeStatus) {
	s.up.mu.Lock()
	ws := make([]func(proto.UpgradeStatus), 0, len(s.up.watchers))
	for _, w := range s.up.watchers {
		ws = append(ws, w)
	}
	s.up.mu.Unlock()
	for _, w := range ws {
		w(st)
	}
}

// WatchUpgrade calls fn with every change of the upgrade status until the
// returned cancel. fn runs on dund's goroutine and must not block.
func (s *Service) WatchUpgrade(fn func(proto.UpgradeStatus)) (cancel func()) {
	s.up.mu.Lock()
	id := s.up.nextW
	s.up.nextW++
	s.up.watchers[id] = fn
	s.up.mu.Unlock()
	return func() {
		s.up.mu.Lock()
		delete(s.up.watchers, id)
		s.up.mu.Unlock()
	}
}

// UpgradeStatus is the pending upgrade, or nil.
func (s *Service) UpgradeStatus() *proto.UpgradeStatus {
	s.up.mu.Lock()
	defer s.up.mu.Unlock()
	return s.up.snapshot()
}

// UpgradeReady delivers the binary to restart into, once: everything the
// upgrade waited for has ended, or was cut short and recorded. The caller
// stops the service and execs it.
func (s *Service) UpgradeReady() <-chan string { return s.up.ready }

// SetUpgradeInstall makes every upgrade call fn once nothing is busy (or on
// "now", or at the deadline), before the restart: a service's dund copies
// the new binary over the one its service starts. An error ends the upgrade
// as "failed" with that error; nothing is interrupted and dund runs on.
func (s *Service) SetUpgradeInstall(fn func(exe, version string) error) {
	s.up.mu.Lock()
	s.up.install = fn
	s.up.mu.Unlock()
}

// RequestUpgrade makes an upgrade into exe (of build version, "" when
// unknown) pending, or retargets the pending one.
func (s *Service) RequestUpgrade(exe, version string) error {
	s.up.mu.Lock()
	if s.up.st != nil && s.up.st.State == "restarting" {
		s.up.mu.Unlock()
		return errors.New("dund is already restarting")
	}
	start := s.up.st == nil
	if start {
		s.up.st, s.up.since, s.up.now = &proto.UpgradeStatus{State: "pending"}, time.Now(), false
		s.up.st.Deadline = s.up.since.Add(upgradeGrace).Unix()
	}
	s.up.st.Exe, s.up.st.Version = exe, version
	st := *s.up.snapshot()
	s.up.mu.Unlock()
	if start {
		go s.upgradeLoop()
	} else {
		s.notifyUpgrade(st)
		s.up.poke()
	}
	return nil
}

// UpgradeNow restarts into the pending upgrade without waiting: what it waits
// for is interrupted and resumed after the restart.
func (s *Service) UpgradeNow() error {
	s.up.mu.Lock()
	defer s.up.mu.Unlock()
	if s.up.st == nil {
		return errors.New("no upgrade is pending")
	}
	s.up.now = true
	s.up.poke()
	return nil
}

func (s *Service) upgradeLoop() {
	first := true
	for {
		waiting := s.Waiting()
		s.up.mu.Lock()
		forced := s.up.now || time.Since(s.up.since) >= upgradeGrace
		changed := first || !slices.Equal(waiting, s.up.st.Waiting)
		first = false
		s.up.st.Waiting = waiting
		if len(waiting) == 0 || forced {
			if install := s.up.install; install != nil {
				exe, ver := s.up.st.Exe, s.up.st.Version
				s.up.mu.Unlock()
				if err := install(exe, ver); err != nil {
					s.failUpgrade(err)
					return
				}
				s.up.mu.Lock()
			}
			s.up.st.State = "restarting"
			st, exe := *s.up.snapshot(), s.up.st.Exe
			s.up.mu.Unlock()
			s.notifyUpgrade(st)
			s.PrepareStop("dund restarted into " + orBuild(st.Version))
			s.up.ready <- exe
			return
		}
		st := *s.up.snapshot()
		s.up.mu.Unlock()
		if changed {
			s.notifyUpgrade(st)
		}
		t := time.NewTimer(upgradePoll)
		select {
		case <-s.done:
			t.Stop()
			return
		case <-s.up.kick:
		case <-t.C:
		}
		t.Stop()
	}
}

// failUpgrade ends the pending upgrade without a restart, and says why to
// every connection; a later request starts a new one.
func (s *Service) failUpgrade(err error) {
	s.up.mu.Lock()
	st := *s.up.snapshot()
	s.up.st = nil
	s.up.mu.Unlock()
	st.State, st.Error, st.Waiting = "failed", err.Error(), nil
	log.Printf("dund: the upgrade to %s failed, running on: %v", orBuild(st.Version), err)
	s.notifyUpgrade(st)
}

func orBuild(v string) string {
	if v == "" {
		return "a new build"
	}
	return "build " + v
}

// PrepareStop readies the sessions for a planned stop of dund (why: "dund
// restarted into build X", "dund was stopped (terminated)"): each session
// that is working has its turn interrupted with why as the reason, and is
// flagged in the registry with the message that wakes it on the next start.
// Idle sessions are left as they are. The caller then stops the service.
func (s *Service) PrepareStop(why string) {
	var wg sync.WaitGroup
	flagged := 0
	for _, ses := range s.all() {
		w := ses.waiting()
		if len(w) == 0 {
			continue
		}
		flagged++
		wg.Add(1)
		go func() {
			defer wg.Done()
			ses.prepareStop(why)
		}()
	}
	wg.Wait()
	if flagged > 0 {
		if err := s.saveRegistry(); err != nil {
			log.Printf("dund: could not record the sessions to resume: %v", err)
		}
		log.Printf("dund: %s: %d working session(s) will resume on the next start", why, flagged)
	}
}

// prepareStop interrupts this session's turn (if any) and records what it
// was doing for the next start.
func (s *session) prepareStop(why string) {
	s.actMu.Lock()
	turn, asking := s.inTurn, s.asking
	jobs, agents := slices.Clone(s.jobs), slices.Clone(s.agents)
	s.actMu.Unlock()
	if turn || asking {
		if err := s.send(proto.NewCommand("", &proto.Interrupt{Reason: why + " while this turn was running"})); err == nil {
			deadline := time.Now().Add(interruptWait)
			for time.Now().Before(deadline) {
				s.actMu.Lock()
				still := s.inTurn
				s.actMu.Unlock()
				if !still {
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
		}
	}
	if s.persists() {
		// The engine restarts these and says so itself (dun's
		// restoredAgentsNote): saying "stopped" here would be wrong.
		agents = nil
	}
	s.setResume(resumeMessage(why, turn || asking, asking, jobs, agents))
}

func (s *session) setResume(msg string) {
	s.actMu.Lock()
	s.resume = msg
	s.actMu.Unlock()
}

func (s *session) resumeNote() string {
	s.actMu.Lock()
	defer s.actMu.Unlock()
	return s.resume
}

// resumeMessage is what a session's agent is woken with after a stop that
// cut its work short: what happened, what was lost, and to carry on.
func resumeMessage(why string, turn, asking bool, jobs []proto.JobInfo, agents []proto.AgentInfo) string {
	var b strings.Builder
	b.WriteString(resumePrefix + why + " while you were working")
	if turn {
		b.WriteString("; your turn was interrupted at the point above")
	}
	b.WriteString(".")
	if asking {
		b.WriteString(" Your question to the user (ask_user) was cut before it was answered: ask it again if you still need the answer.")
	}
	if len(jobs) > 0 {
		list := make([]string, len(jobs))
		for i, j := range jobs {
			list[i] = fmt.Sprintf("#%d `%s`", j.ID, j.Command)
		}
		b.WriteString(" The restart killed these background jobs: " + strings.Join(list, ", ") + "; start again the ones you still need.")
	}
	if len(agents) > 0 {
		list := make([]string, len(agents))
		for i, a := range agents {
			list[i] = fmt.Sprintf("#%d", a.ID)
		}
		b.WriteString(" It also stopped these sub-agents: " + strings.Join(list, ", ") + ".")
	}
	b.WriteString(" Resume the task.")
	return b.String()
}

// crashedMidTurn reports whether a session's transcript was cut mid-turn
// recently (a variable: the tests' fake engine writes no transcript).
var crashedMidTurn = func(workspace, id string) bool {
	f := dun.SessionFile(workspace, id)
	st, err := os.Stat(f)
	if err != nil || time.Since(st.ModTime()) > crashWindow {
		return false
	}
	return dun.EndedMidTurn(f)
}

// resumeFlagged wakes every session the last stop flagged: its engine
// starts and gets the resume message as a user message. The flag is cleared
// once sent, so it is sent once.
func (s *Service) resumeFlagged() {
	sent := 0
	for _, ses := range s.all() {
		msg := ses.resumeNote()
		if msg == "" {
			continue
		}
		if err := ses.send(proto.NewCommand("", &proto.User{Content: msg})); err != nil {
			log.Printf("dund: could not resume %s: %v", ses.id, err)
			continue
		}
		ses.setResume("")
		sent++
	}
	if sent > 0 {
		if err := s.saveRegistry(); err != nil {
			log.Printf("dund: registry: %v", err)
		}
		log.Printf("dund: resumed %d session(s) cut short by the last stop", sent)
	}
}
