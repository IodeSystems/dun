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
// once: a restart stops every engine, which would cut a turn, a job or a
// sub-agent short. It becomes pending instead, tells every
// connection what it waits for (proto.UpgradeStatus), and restarts when that
// is done — or when someone says "now", or after upgradeGrace. A restart that
// cuts work short interrupts the turns with the reason, records in the
// registry what each session was doing, and the next dund wakes those
// sessions with a message that says what happened (resumeFlagged). Other
// planned stops (SIGTERM, a service restart) record and resume the same way
// (PrepareStop).
//
// An open ask_user question does not hold it (Carl 2026-11-10): the turn
// around it waits on the person, not on work, and the question survives
// the restart. The stop interrupts that turn and saves the question (its
// proto.Ask, whole) in the registry; the next dund shows it again to every
// client as an open question in a turn (showRestored) and does not wake
// the agent. The person's answer wakes it with a user message, `[automatic]
// Answer to your question "<question>": <answer>` — a new message, not the
// ask_user result (Carl accepted). An interrupt instead ends the shown turn
// and leaves a quiet note, as an interrupted question starts no turn. What
// else the stop cut (jobs, sub-agents) goes with the answer or the note:
// the agent could not act on it before. A question beside running jobs or
// sub-agents is saved the same way; those still hold the upgrade. A mount
// question is not saved and holds as a turn does.
//
// The busy sessions are told (USER 2026-10-28): each one with a turn, a
// question, jobs or sub-agents gets a notice, once, that an upgrade waits
// for it — finish the step, end the turn. After the restart, one that
// wrapped up is woken with "done, resume"; one the restart cut short gets
// the resume message that says what was cut. If the upgrade fails, they are
// told to carry on.
//
// While it is pending, nothing on dund's side starts a turn (Carl
// 2026-11-10): a turn started now is one more thing the restart waits for.
// The notices are quiet (proto.ServiceNotice.Quiet): a running turn reads
// one, an idle session — one with only jobs or sub-agents — is not woken by
// it; this replaces the 2026-10-28 "and wakes for it if idle". Scheduled
// messages and loops are held (schedule.go): they keep their time and fire
// once when the hold ends, in the next dund or after a failed upgrade.

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
			// The noticed sessions that wrapped up: woken once dund is back.
			for _, ses := range s.all() {
				ses.actMu.Lock()
				if ses.noticed && ses.resume == "" {
					ses.resume = resumePrefix + "dun was upgraded (" + strings.TrimPrefix(orBuild(s.up.st.Version), "build ") +
						"). You wrapped up for it; the work can go on: resume the task."
				}
				ses.actMu.Unlock()
			}
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
		left := upgradeGrace - time.Since(s.up.since)
		s.up.mu.Unlock()
		if changed {
			s.notifyUpgrade(st)
		}
		s.noticeBusy(st.Version, left)
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
	for _, ses := range s.all() { // told to wrap up: told it is off
		ses.actMu.Lock()
		was := ses.noticed
		ses.noticed, ses.resume = false, ""
		ses.actMu.Unlock()
		if was {
			_ = ses.sendLoaded(proto.NewCommand("", &proto.ServiceNotice{Quiet: true, Text: "The dun upgrade you were told about did not happen (" +
				err.Error() + "); dun keeps running as it was. Carry on."}))
		}
	}
	s.sched.poke() // the held firings go now (schedule.go)
}

// noticeBusy tells each session the pending upgrade waits for, once, to wrap
// up: what is coming, how long it waits, and what happens to unfinished
// work. left is the time until it stops waiting.
func (s *Service) noticeBusy(version string, left time.Duration) {
	for _, ses := range s.all() {
		if len(ses.waiting()) == 0 {
			continue
		}
		ses.actMu.Lock()
		if ses.noticed {
			ses.actMu.Unlock()
			continue
		}
		ses.noticed = true
		ses.actMu.Unlock()
		mins := int(left.Minutes() + 0.5)
		text := fmt.Sprintf("dun is about to be upgraded to %s: dund restarts as soon as this session is idle — no turn "+
			"running, no background jobs or sub-agents — or in about %d minutes, when a running turn is interrupted and "+
			"background jobs are killed. Finish the step you are on, leave the work where it resumes cleanly, let your "+
			"background jobs finish or stop the ones you do not need, and end your turn. You will be told when it is done.",
			strings.TrimPrefix(orBuild(version), "build "), max(1, mins))
		// Quiet: a turn running gets it, an idle session is not woken by it
		// (a turn started now would be one more thing the upgrade waits for).
		_ = ses.sendLoaded(proto.NewCommand("", &proto.ServiceNotice{Quiet: true, Text: text}))
	}
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
	for _, ses := range s.all() {
		if len(ses.waiting()) == 0 && ses.openAsk() == nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			ses.prepareStop(why)
		}()
	}
	wg.Wait()
	flagged := 0
	for _, ses := range s.all() { // also the ones told to wrap up, which did
		if ses.resumeNote() != "" || ses.restoredAsk() != nil {
			flagged++
		}
	}
	if flagged > 0 {
		if err := s.saveRegistry(); err != nil {
			log.Printf("dund: could not record the sessions to resume: %v", err)
		}
		log.Printf("dund: %s: %d working session(s) will resume on the next start", why, flagged)
	}
}

// prepareStop interrupts this session's turn (if any) and records what it
// was doing for the next start. An open ask_user question is saved: the
// next dund shows it again, and the agent is woken by its answer, not by
// the resume message, which waits for that answer (restoredReply).
func (s *session) prepareStop(why string) {
	s.actMu.Lock()
	turn, asking, ask := s.inTurn, s.asking, s.ask
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
	if ask != nil {
		note := ""
		if len(jobs) > 0 || len(agents) > 0 {
			note = resumeMessage(why, false, jobs, agents)
		}
		s.actMu.Lock()
		s.restored, s.restoredEvs, s.resume = ask, nil, note
		s.actMu.Unlock()
		return
	}
	s.setResume(resumeMessage(why, turn || asking, jobs, agents))
}

func (s *session) restoredAsk() *proto.Ask {
	s.actMu.Lock()
	defer s.actMu.Unlock()
	return s.restored
}

// showRestored puts the restored question in the session's log as a turn
// waiting on it, so every client shows it as it shows any open question
// (and can answer it, or interrupt the turn to dismiss it). No engine runs
// that turn: dund ends it (restoredReply).
func (s *session) showRestored() {
	s.actMu.Lock()
	r := s.restored
	shown := len(s.restoredEvs) > 0
	s.actMu.Unlock()
	if r == nil || shown {
		return
	}
	// Appended with actMu released: it is a leaf lock. One caller at a
	// time (the start's resumeFlagged, or a failed reply).
	q := *r
	evs := []proto.Event{s.log.Append(proto.NewEvent(&proto.Turn{Kind: "user"})), s.log.Append(proto.NewEvent(&q))}
	s.actMu.Lock()
	if s.restored == r {
		s.restoredEvs = evs
	}
	s.actMu.Unlock()
}

// restoredReply handles c when it answers or interrupts the restored
// question (done), and lets anything else through to the engine. The answer
// wakes the agent with a user message; an interrupt ends the shown turn and
// leaves the agent a quiet note, as an interrupted question starts no turn.
// Either way the resume note held with the question goes with it.
func (s *session) restoredReply(c proto.Command) (done bool, err error) {
	s.actMu.Lock()
	q := s.restored
	// The engine's own open question, or its running turn, takes these.
	if q == nil || s.asking || s.inTurn {
		s.actMu.Unlock()
		return false, nil
	}
	var answer *proto.Answer
	switch p := c.Payload.(type) {
	case *proto.Answer:
		answer = p
	case *proto.Interrupt:
	default:
		s.actMu.Unlock()
		return false, nil
	}
	note := s.resume
	s.restored, s.restoredEvs, s.resume = nil, nil, ""
	s.actMu.Unlock()
	if answer != nil {
		s.log.Append(proto.NewEvent(&proto.Answered{Value: answer.Value}))
		err = s.send(proto.NewCommand("", &proto.User{Content: answerMessage(q.Question, answer.Value, note)}))
	} else {
		s.log.Append(proto.NewEvent(&proto.Done{Reason: "interrupted", Detail: "question dismissed"}))
		err = s.send(proto.NewCommand("", &proto.ServiceNotice{Quiet: true, Text: dismissedNote(q.Question, note)}))
	}
	if err != nil {
		// Not delivered: it stays for the next try (a retry asks again).
		s.actMu.Lock()
		if s.restored == nil {
			s.restored, s.resume = q, note
		}
		s.actMu.Unlock()
		s.showRestored()
		return true, err
	}
	if err := s.svc.saveRegistry(); err != nil {
		log.Printf("dund: registry: %v", err)
	}
	return true, nil
}

// answerMessage is the user message a restored question's answer wakes the
// agent with; note is what the stop cut besides (resumeMessage), if anything.
func answerMessage(question, answer, note string) string {
	m := resumePrefix + "Answer to your question \"" + question + "\": " + answer
	if note != "" {
		m += "\n" + strings.TrimPrefix(note, resumePrefix)
	}
	return m
}

// dismissedNote is what the agent is told when the person interrupts a
// restored question instead of answering it.
func dismissedNote(question, note string) string {
	m := "Your question \"" + question + "\" (asked before dund restarted) was dismissed without an answer."
	if note != "" {
		m += " " + strings.TrimPrefix(note, resumePrefix)
	}
	return m
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
func resumeMessage(why string, turn bool, jobs []proto.JobInfo, agents []proto.AgentInfo) string {
	var b strings.Builder
	b.WriteString(resumePrefix + why + " while you were working")
	if turn {
		b.WriteString("; your turn was interrupted at the point above")
	}
	b.WriteString(".")
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
// once sent, so it is sent once. A session with a restored question is not
// woken: the question is shown again, and its answer wakes it.
func (s *Service) resumeFlagged() {
	sent := 0
	for _, ses := range s.all() {
		if ses.restoredAsk() != nil {
			ses.showRestored()
			continue
		}
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
