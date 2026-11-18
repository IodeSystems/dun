package dund

import (
	"errors"
	"fmt"
	"log"
	"os"
	"slices"
	"sort"
	"strconv"
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
// One exception: an idle session with background jobs or sub-agents the
// upgrade waits for is woken by its wrap-up notice, once per pending
// upgrade, to wrap them up (the 2026-10-28 "and wakes for it if idle").
// Otherwise the notices are quiet (proto.ServiceNotice.Quiet): to a running
// turn, and the "did not happen" one; a session with nothing running is not
// told at all. Scheduled messages and loops are held (schedule.go): they
// keep their time and fire once when the hold ends, in the next dund or
// after a failed upgrade.
//
// The drain gate and the durable resume (USER 2026-11-17). Automatic
// launches wait for the new build: a message no person typed (automatic:
// dun mcp's dun_start and dun_send) is kept in the registry (queued) and
// sent after the restart, and its sender is told "queued"
// (proto.Ack.Queued); a person's message starts its turn at once. Each
// engine is held (proto.UpgradeHold): its own wakes — a finished job, a
// peer's dun_msg, a server's wake, an escalated aside — start no turn; the
// engine writes that news into the conversation, where the restart keeps
// it, and reports it (proto.WakeHeld), and the next dund wakes the
// session to act on it. The stop writes each session's resume record
// before it interrupts anything, and a failed write puts the upgrade back
// to pending with the error. After the restart a cut turn is continued
// (the message names the tool call in flight and the killed jobs), and
// each sub-agent the stop cut is told to continue its task. A continued
// turn that is cut again gets a quiet note instead (resumeQuiet): one
// continue per cut turn.

// Variables so tests can shorten them.
var (
	upgradePoll  = 500 * time.Millisecond // how often the waiting list is read
	upgradeGrace = 10 * time.Minute       // pending this long, it restarts anyway
	// upgradeRetry: a restart that could not record the sessions' resume
	// is tried again after this.
	upgradeRetry = 5 * time.Second
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
		s.holdEngines(true) // before the loop's wrap-up notices
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
			s.up.st.State, s.up.st.Error = "restarting", ""
			st, exe := *s.up.snapshot(), s.up.st.Exe
			s.up.mu.Unlock()
			s.notifyUpgrade(st)
			err := s.PrepareStop("dund restarted into " + orBuild(st.Version))
			if err == nil {
				s.up.ready <- exe
				return
			}
			// Nothing was interrupted: without the record, a restart would
			// lose what the sessions were doing. Pending again, and said.
			s.up.mu.Lock()
			s.up.st.State, s.up.st.Error = "pending", err.Error()
			st = *s.up.snapshot()
			s.up.mu.Unlock()
			log.Printf("dund: the upgrade to %s waits: %v", orBuild(st.Version), err)
			s.notifyUpgrade(st)
			if !s.upgradeWait(upgradeRetry) {
				return
			}
			continue
		}
		st := *s.up.snapshot()
		left := upgradeGrace - time.Since(s.up.since)
		s.up.mu.Unlock()
		if changed {
			s.notifyUpgrade(st)
		}
		s.noticeBusy(st.Version, left)
		if !s.upgradeWait(upgradePoll) {
			return
		}
	}
}

// upgradeWait waits d, or less when the upgrade is poked; false when the
// service closed.
func (s *Service) upgradeWait(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-s.done:
		return false
	case <-s.up.kick:
	case <-t.C:
	}
	return true
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
	s.holdEngines(false) // what each engine held runs now
	sent := false
	for _, ses := range s.all() { // told to wrap up: told it is off
		ses.actMu.Lock()
		was := ses.noticed
		ses.noticed, ses.resume, ses.heldWake = false, "", false
		queued := ses.queued
		ses.queued = nil
		ses.actMu.Unlock()
		if was {
			_ = ses.sendLoaded(proto.NewCommand("", &proto.ServiceNotice{Quiet: true, Text: "The dun upgrade you were told about did not happen (" +
				err.Error() + "); dun keeps running as it was. Carry on."}))
		}
		for _, m := range queued { // the automatic messages it held go now
			if err := ses.send(proto.NewCommand("", &proto.User{Content: m, Auto: true})); err != nil {
				log.Printf("dund: could not deliver a held message to %s: %v", ses.id, err)
			}
			sent = true
		}
	}
	if sent {
		if err := s.saveRegistry(); err != nil {
			log.Printf("dund: registry: %v", err)
		}
	}
	s.sched.poke() // the held firings go now (schedule.go)
}

// holdEngines tells every running engine that an upgrade is pending (on)
// or no longer is: held, an engine starts no turn of its own accord
// (proto.UpgradeHold).
func (s *Service) holdEngines(on bool) {
	for _, ses := range s.all() {
		_ = ses.sendLoaded(proto.NewCommand("", &proto.UpgradeHold{On: on}))
	}
}

// queuedText is what the sender of an automatic message is told while an
// upgrade is pending.
const queuedText = "dund is upgrading; the message is kept and runs after the restart, on the new build"

// queueIfUpgrading holds an automatic message (automatic) while an upgrade
// is pending: kept in the registry, it is sent after the restart, on the
// new build (USER 2026-11-17). It reports whether it held c; the error is a
// message that could not be kept, and was not.
func (s *Service) queueIfUpgrading(ses *session, c proto.Command) (bool, error) {
	u, ok := c.Payload.(*proto.User)
	if !ok || s.UpgradeStatus() == nil || !automatic(ses, u) {
		return false, nil
	}
	ses.actMu.Lock()
	ses.queued = append(ses.queued, u.Content)
	ses.actMu.Unlock()
	if err := s.saveRegistry(); err != nil {
		ses.actMu.Lock()
		ses.queued = ses.queued[:len(ses.queued)-1]
		ses.actMu.Unlock()
		return true, fmt.Errorf("dund is upgrading and could not keep the message for after the restart: %w", err)
	}
	return true, nil
}

// automatic: no person typed u. Said by the sender (proto.User.Auto: dun
// mcp's dun_start and dun_send), or implied: a --worker session is
// dun_start's, and a dun mcp of an older build does not set Auto. A person
// typing into a worker session is taken for a program too.
func automatic(ses *session, u *proto.User) bool { return u.Auto || ses.worker() }

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
		inTurn := ses.inTurn
		ses.actMu.Unlock()
		mins := int(left.Minutes() + 0.5)
		text := fmt.Sprintf("dun is about to be upgraded to %s: dund restarts as soon as this session is idle — no turn "+
			"running, no background jobs or sub-agents — or in about %d minutes, when a running turn is interrupted and "+
			"background jobs are killed. Finish the step you are on, leave the work where it resumes cleanly, let your "+
			"background jobs finish or stop the ones you do not need, and end your turn. You will be told when it is done.",
			strings.TrimPrefix(orBuild(version), "build "), max(1, mins))
		// Quiet to a running turn, which reads it anyway: a wake would only
		// start one more turn after it. An idle session here has jobs or
		// sub-agents the upgrade waits for, and is woken, once (noticed), to
		// wrap them up (USER 2026-10-28; Carl 2026-11-10).
		_ = ses.sendLoaded(proto.NewCommand("", &proto.ServiceNotice{Quiet: inTurn, Text: text}))
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
// that is working, or holds news its engine kept (proto.WakeHeld), is
// flagged in the registry with the message that wakes it on the next start,
// and then its turn is interrupted with why as the reason. Idle sessions are
// left as they are. The caller then stops the service.
//
// The record is written BEFORE anything is interrupted (USER 2026-11-17):
// when it cannot be written, nothing is interrupted, the flags are taken
// back, and the error says why — an upgrade stays pending rather than kill
// engines with nothing to resume them by.
func (s *Service) PrepareStop(why string) error {
	type before struct {
		note        string
		quiet, held bool
		restored    *proto.Ask
		agents      []int
	}
	was := map[*session]before{}
	var cut []*session
	for _, ses := range s.all() {
		working := len(ses.waiting()) > 0 || ses.openAsk() != nil
		ses.actMu.Lock()
		held := ses.heldWake
		if working || held {
			was[ses] = before{note: ses.resume, quiet: ses.resumeQuiet, restored: ses.restored,
				agents: ses.cutAgents, held: held}
		}
		ses.actMu.Unlock()
		if working {
			ses.recordStop(why)
			cut = append(cut, ses)
		} else if held {
			ses.recordHeld(why)
		}
	}
	flagged := 0
	for _, ses := range s.all() { // also the ones told to wrap up, which did
		ses.actMu.Lock()
		if ses.resume != "" || ses.restored != nil || len(ses.queued) > 0 {
			flagged++
		}
		ses.actMu.Unlock()
	}
	if flagged > 0 {
		if err := s.saveRegistry(); err != nil {
			for ses, b := range was {
				ses.actMu.Lock()
				ses.resume, ses.resumeQuiet, ses.restored, ses.cutAgents, ses.heldWake = b.note, b.quiet, b.restored, b.agents, b.held
				ses.actMu.Unlock()
			}
			return fmt.Errorf("could not record the sessions to resume, so none was interrupted: %w", err)
		}
	}
	var wg sync.WaitGroup
	for _, ses := range cut {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ses.interruptForStop(why)
		}()
	}
	wg.Wait()
	if flagged > 0 {
		log.Printf("dund: %s: %d working session(s) will resume on the next start", why, flagged)
	}
	return nil
}

// recordStop sets what this session's next start does about the work a
// stop cuts: the message that continues its turn (resumeMessage), or its
// open ask_user question, saved to be shown again — then the agent is woken
// by its answer, not by the resume message, which waits for that answer
// (restoredReply). The sub-agents running now are resumed with their task.
// A turn that was itself dund's automatic continue gets no second one
// (resumeQuiet).
func (s *session) recordStop(why string) {
	s.actMu.Lock()
	defer s.actMu.Unlock()
	turn := s.inTurn || s.asking
	jobs, agents := slices.Clone(s.jobs), slices.Clone(s.agents)
	calls := make([]string, 0, len(s.calls))
	for _, c := range s.calls {
		calls = append(calls, c)
	}
	sort.Strings(calls)
	s.cutAgents = nil
	for _, a := range agents {
		s.cutAgents = append(s.cutAgents, a.ID)
	}
	held := s.heldWake
	s.heldWake = false
	if s.ask != nil && s.asking {
		note := ""
		if len(jobs) > 0 || len(agents) > 0 || held {
			note = resumeMessage(why, false, jobs, agents, nil)
			if held {
				note += heldNote
			}
		}
		s.restored, s.restoredEvs, s.resume = s.ask, nil, note
		return
	}
	s.resume = resumeMessage(why, turn, jobs, agents, calls)
	if held {
		s.resume += heldNote
	}
	s.resumeQuiet = s.continued && turn
}

// heldNote is said when the engine kept news for after the restart.
const heldNote = " News that came while dun waited for the upgrade (finished jobs, messages) is in the conversation above; act on it."

// recordHeld flags an idle session whose engine kept news while the
// upgrade was pending: the next start wakes it to act on it.
func (s *session) recordHeld(why string) {
	s.actMu.Lock()
	defer s.actMu.Unlock()
	s.heldWake = false
	if s.resume == "" {
		s.resume = resumePrefix + why + "." + heldNote
	} else {
		s.resume += heldNote
	}
}

// interruptForStop interrupts this session's turn (if any) and waits up to
// interruptWait for it to end, so its partial reply and its note are kept.
func (s *session) interruptForStop(why string) {
	s.actMu.Lock()
	turn := s.inTurn || s.asking
	s.actMu.Unlock()
	if !turn {
		return
	}
	if err := s.send(proto.NewCommand("", &proto.Interrupt{Reason: why + " while this turn was running"})); err != nil {
		return
	}
	deadline := time.Now().Add(interruptWait)
	for time.Now().Before(deadline) {
		s.actMu.Lock()
		still := s.inTurn
		s.actMu.Unlock()
		if !still {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
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
	// The state says it too, as the engine's would: a question is open.
	ask := q
	s.log.Append(proto.NewEvent(&proto.TurnState{Phase: proto.PhaseWaitingAnswer, Since: time.Now().UnixMilli(),
		Queued: []proto.QueuedMsg{}, Ask: &ask}))
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
		s.log.Append(proto.NewEvent(&proto.Answered{Value: answer.Value, AskID: q.AskID, By: answer.By}))
		err = s.send(proto.NewCommand("", &proto.User{Content: answerMessage(q.Question, answer.Value, note)}))
	} else {
		s.log.Append(proto.NewEvent(&proto.Done{Reason: "interrupted", Detail: "question dismissed"}))
		s.log.Append(proto.NewEvent(idleTurn()))
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

func (s *session) resumeNote() string {
	s.actMu.Lock()
	defer s.actMu.Unlock()
	return s.resume
}

// resumeMessage is what a session's agent is woken with after a stop that
// cut its work short: what happened, what was lost — the tool calls in
// flight (calls), the background jobs killed, with their commands to run
// again, the sub-agents cut mid-run, which dund resumes — and to carry on.
func resumeMessage(why string, turn bool, jobs []proto.JobInfo, agents []proto.AgentInfo, calls []string) string {
	var b strings.Builder
	b.WriteString(resumePrefix + why + " while you were working")
	if turn {
		b.WriteString("; your turn was interrupted at the point above")
	}
	b.WriteString(".")
	if len(calls) > 0 {
		b.WriteString(" The tool call in flight was cut: `" + strings.Join(calls, "`, `") + "`; check what it did before you run it again.")
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
			list[i] = fmt.Sprintf("#%d (%s)", a.ID, clipLine(a.Prompt, 80))
		}
		b.WriteString(" The restart cut these sub-agents mid-run: " + strings.Join(list, ", ") + "; dun resumes each with its task, and you hear from it as usual.")
	}
	b.WriteString(" Resume the task.")
	return b.String()
}

// agentContinue is what a sub-agent the restart cut mid-run is told after
// it, to go on with its task.
const agentContinue = resumePrefix + "dund restarted while you were working, and your run was cut off at the point above. " +
	"Continue your task from there; check what a cut tool call did before you run it again."

// notContinued ends a resume message that goes as a quiet note: the turn
// it is about was itself dund's automatic continue.
const notContinued = " This was not continued automatically, and no sub-agent was resumed: the turn dun continued after " +
	"the last restart was cut too. Carry on when the next message comes."

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
// starts and gets the resume message as a user message, and each sub-agent
// the stop cut is told to continue its task (proto.AgentCmd "continue";
// the parent hears of it in the message and from its engine). Then the
// automatic messages held while the upgrade was pending are sent, in
// order. Each flag is cleared once sent, so it is sent once. A session
// with a restored question is not woken: the question is shown again, and
// its answer wakes it. A resume marked quiet goes as a quiet note and wakes
// nothing (one continue per cut turn).
func (s *Service) resumeFlagged() {
	sent := 0
	for _, ses := range s.all() {
		ses.actMu.Lock()
		msg, quiet, agents, queued := ses.resume, ses.resumeQuiet, ses.cutAgents, ses.queued
		ses.actMu.Unlock()
		if ses.restoredAsk() != nil {
			ses.showRestored()
			msg = "" // it goes with the answer (restoredReply)
		}
		if msg != "" {
			var err error
			if quiet {
				agents = nil
				err = ses.send(proto.NewCommand("", &proto.ServiceNotice{Quiet: true,
					Text: strings.TrimPrefix(msg, resumePrefix) + notContinued}))
			} else {
				err = ses.send(proto.NewCommand("", &proto.User{Content: msg}))
			}
			if err != nil {
				log.Printf("dund: could not resume %s: %v", ses.id, err)
				continue
			}
			ses.actMu.Lock()
			ses.resume, ses.resumeQuiet, ses.continued = "", false, !quiet
			ses.actMu.Unlock()
			sent++
		}
		for _, n := range agents {
			c := proto.NewCommand("", &proto.AgentCmd{ID: strconv.Itoa(n), Action: "continue", Content: agentContinue})
			if err := ses.send(c); err != nil {
				log.Printf("dund: could not resume sub-agent #%d of %s: %v", n, ses.id, err)
			}
		}
		for _, m := range queued {
			if err := ses.send(proto.NewCommand("", &proto.User{Content: m, Auto: true})); err != nil {
				log.Printf("dund: could not deliver a held message to %s: %v", ses.id, err)
			}
		}
		if len(agents) > 0 || len(queued) > 0 {
			ses.actMu.Lock()
			ses.cutAgents, ses.queued = nil, nil
			ses.actMu.Unlock()
			sent++
		}
	}
	if sent > 0 {
		if err := s.saveRegistry(); err != nil {
			log.Printf("dund: registry: %v", err)
		}
		log.Printf("dund: resumed %d session(s) after the last stop", sent)
	}
}
