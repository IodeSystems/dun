package dund

import (
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/iodesystems/dun/internal/harness"
	"github.com/iodesystems/dun/proto"
)

// Engine adoption (docs/engine-adoption.md, USER 2026-11-17): an upgrade's
// restart leaves the engines on the engine link running (Detach), and the
// next dund takes them back (adoptAll) instead of resuming their sessions
// from the transcript. Adoption that fails falls back to the resume record
// the old dund wrote (fallbackRec), as a stop always did.

// fallbackRec is the resume record a stop would have written for a working
// session whose engine an upgrade left running: applied by the next dund
// only when it cannot adopt that engine.
type fallbackRec struct {
	Resume string     `json:"resume,omitempty"`
	Ask    *proto.Ask `json:"ask,omitempty"`
	Quiet  bool       `json:"quiet,omitempty"`
	Agents []int      `json:"agents,omitempty"`
}

// adoptable: the engine is on the link, so an upgrade's restart leaves it
// running and the next dund adopts it.
func (s *session) adoptable() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.eng.(*linkEngine)
	return ok && s.running
}

// waitsUpgrade: a pending upgrade waits for this session (adoptable ones it
// does not: they run on through it).
func (s *session) waitsUpgrade() []proto.UpgradeWait {
	if s.adoptable() {
		return nil
	}
	return s.waiting()
}

// recordFallback writes what recordStop would, into fallback instead of
// the resume fields: the engine is not stopped, and the record is used
// only if the next dund cannot adopt it.
func (s *session) recordFallback(why string) {
	s.actMu.Lock()
	resume, restored, evs, quiet, agents, held := s.resume, s.restored, s.restoredEvs, s.resumeQuiet, s.cutAgents, s.heldWake
	s.actMu.Unlock()
	s.recordStop(why + "; the new dund could not take back this session's running engine, so it was stopped")
	s.actMu.Lock()
	s.fallback = &fallbackRec{Resume: s.resume, Ask: s.restored, Quiet: s.resumeQuiet, Agents: s.cutAgents}
	s.resume, s.restored, s.restoredEvs, s.resumeQuiet, s.cutAgents, s.heldWake = resume, restored, evs, quiet, agents, held
	s.actMu.Unlock()
}

// adopt makes eng, an engine an earlier dund left running, this session's.
// build is the one dund starts engines on: an engine of another build, or
// one holding gateway grants that died with the old dund, moves to a new
// engine at its next idle (checkStale).
func (s *session) adopt(eng Engine, h proto.LinkHello, build string) {
	why := ""
	switch {
	case build != "" && h.Version != "" && h.Version != build:
		why = fmt.Sprintf("moving from build %s to %s", h.Version, build)
	case h.Shared > 0:
		why = "reconnecting its shared tool servers"
	}
	s.actMu.Lock()
	s.stale, s.live, s.moving = why, false, false
	s.actMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.eng, s.engDone = eng, make(chan struct{})
	s.running, s.stopping, s.gaveUp, s.shown = true, false, false, true
	// This log starts mid-conversation: the engine sent its history to the
	// dund before. A client from the start gets a snapshot (subscribe).
	s.logPartial = true
	s.log.Append(proto.NewEvent(&proto.EngineState{State: "started", Reason: "adopted"}))
	go s.readEvents(eng, s.engDone, newScanner(eng.Stdout()), proto.HistoryRestart)
}

// checkStale moves an adopted engine that is to move once its replay is
// over and nothing occupies it.
func (s *session) checkStale() {
	s.actMu.Lock()
	why := s.stale
	move := why != "" && s.live && !s.moving && !s.occupied()
	if move {
		s.moving = true
	}
	s.actMu.Unlock()
	if move {
		go s.moveEngine(why)
	}
}

// moveEngine stops the engine and starts a new one on the conversation
// (--resume), as an idle stop and the next command would.
func (s *session) moveEngine(why string) {
	s.log.Append(proto.NewEvent(&proto.EngineState{State: "stopping", Reason: why}))
	s.stop()
	s.actMu.Lock()
	s.stale, s.moving = "", false
	s.actMu.Unlock()
	if err := s.ensureRunning(); err != nil {
		log.Printf("dund: session %s: %s: %v", s.id, why, err)
	}
}

// reattachHistory answers a link_gap: the events the engine kept while no
// dund was there overflowed, so the conversation comes from the transcript
// (the snapshot's source) and replaces what clients show.
func (s *session) reattachHistory() {
	items := harness.SessionHistory(harness.SessionFile(s.workspace, s.id))
	s.log.Append(proto.NewEvent(&proto.History{Items: historyItems(items), Reason: proto.HistoryReattach}))
}

// detach lets go of an adoptable engine, exactly: no event is taken after
// it, every event taken is in the log, and the engine is told so (ack).
// Another engine is stopped.
func (s *session) detach() {
	s.mu.Lock()
	eng, done := s.eng, s.engDone
	s.mu.Unlock()
	le, ok := eng.(*linkEngine)
	if !ok {
		s.stop()
		return
	}
	le.stopReading()
	select {
	case <-done:
	case <-time.After(stopGrace):
		log.Printf("dund: session %s: its reader did not end; detaching anyway", s.id)
	}
	le.finishDetach()
}

// Detach is Close for an upgrade's restart: engines on the link are left
// running for the next dund (detach), the others are stopped. The logs stay
// open: the process is about to exec or exit.
func (s *Service) Detach() {
	s.mu.Lock()
	if !s.closing {
		close(s.done)
	}
	s.closing = true
	all := make([]*session, 0, len(s.sessions))
	for _, ses := range s.sessions {
		all = append(all, ses)
	}
	s.mu.Unlock()
	var wg sync.WaitGroup
	for _, ses := range all {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ses.adoptable() {
				ses.detach()
			} else {
				ses.stop()
			}
		}()
	}
	wg.Wait()
	if gw := s.gwh.get(false); gw != nil {
		gw.Close()
	}
}

// adoptAll takes back the engines an earlier dund left (called by New,
// before the crash check and resumeFlagged) and settles the fallback
// records. An adopted session drops its record. A session whose engine is
// not adopted gets it only once that engine is provably gone (endEngine):
// never two engines on one transcript. One whose engine may still run is
// blocked (blockPid): not resumed, no engine started for it, shown by dun
// ps, until that process is gone. It returns the sessions the crash check
// must leave alone: the adopted and the blocked.
func (s *Service) adoptAll(ad Adopter) map[string]bool {
	skip := map[string]bool{}
	adopted := map[string]bool{}
	oks, fails := ad.Adopt()
	for _, a := range oks {
		ses := s.sessions[a.Hello.Session]
		why := ""
		switch {
		case ses == nil:
			why = "its session is not in the registry"
		case adopted[ses.id]:
			why = "its session has another engine"
		}
		if why == "" {
			eng, err := a.Accept()
			if err == nil {
				ses.adopt(eng, a.Hello, ad.EngineBuild())
				adopted[ses.id], skip[ses.id] = true, true
				log.Printf("dund: adopted engine %d of session %s (build %q)", a.Hello.Pid, ses.id, a.Hello.Version)
				if a.Hello.Shared > 0 {
					ses.sharedGone()
				}
				continue
			}
			why = err.Error()
		}
		a.conn.Close()
		fails = append(fails, AdoptFailure{Info: a.Info, Why: why, Dir: a.dir})
	}
	blocked := map[string]string{}
	for _, f := range fails {
		gone := endEngine(f.Dir, f.Info, f.Why)
		if ses := s.sessions[f.Info.Session]; ses != nil && !gone && !adopted[ses.id] {
			blocked[ses.id] = f.Why
			ses.actMu.Lock()
			ses.blockPid, ses.blockWhy = f.Info.Pid, f.Why
			ses.actMu.Unlock()
		}
	}
	changed := false
	for _, ses := range s.sessions {
		ses.actMu.Lock()
		if fb := ses.fallback; fb != nil {
			changed = true
			// The registry holds the record twice: in fallback, and in the
			// fields an older dund reads (saveRegistry), loaded as ours.
			fromFallback := ses.resume == fb.Resume && (ses.restored == nil) == (fb.Ask == nil)
			switch {
			case adopted[ses.id] || blocked[ses.id] != "":
				if fromFallback {
					ses.resume, ses.restored, ses.resumeQuiet, ses.cutAgents = "", nil, false, nil
				}
			case ses.resume == "" && ses.restored == nil:
				ses.resume, ses.restored, ses.resumeQuiet, ses.cutAgents = fb.Resume, fb.Ask, fb.Quiet, fb.Agents
			}
			if blocked[ses.id] == "" {
				ses.fallback = nil
			}
		}
		if blocked[ses.id] != "" {
			skip[ses.id] = true
		}
		ses.actMu.Unlock()
	}
	if changed {
		if err := s.saveRegistry(); err != nil {
			log.Printf("dund: registry: %v", err)
		}
	}
	return skip
}

// sharedGone tells an adopted engine's agent, and its clients, that the
// shared MCP servers it used through the old dund's gateway are gone until
// its next idle, when it moves to a new engine and they come back (USER
// review 2026-11-18).
func (s *session) sharedGone() {
	const text = "dund restarted: the shared MCP servers this session used through dund are stopped. " +
		"A call to one fails until this session is next idle, when dun restarts its engine and they come back; retry then."
	// The engine shows its clients every service notice it is sent (a
	// notification event, cmd/dun/peers.go); a copy logged here too showed
	// twice. Logged here only when the engine could not be told.
	if err := s.sendLoaded(proto.NewCommand("", &proto.ServiceNotice{Quiet: true, Text: text})); err != nil {
		s.log.Append(proto.NewEvent(&proto.Notification{Kind: "service", Text: text}))
	}
}

// pidAliveFn is pidAlive (a variable: the tests' engines are no processes).
var pidAliveFn = pidAlive

// blocked is why this session may not start an engine: one from before
// dund's restart may still run on its transcript. Cleared once that
// process is gone. Called with mu held (actMu is a leaf).
func (s *session) blocked() error {
	s.actMu.Lock()
	defer s.actMu.Unlock()
	if s.blockPid == 0 {
		return nil
	}
	if pidAliveFn(s.blockPid) {
		return fmt.Errorf("this session's engine from before dund's restart (pid %d) may still be running (%s), "+
			"and two engines must not share a conversation: end it (kill %d), then try again", s.blockPid, s.blockWhy, s.blockPid)
	}
	log.Printf("dund: session %s: engine %d is gone; the session runs again", s.id, s.blockPid)
	s.blockPid, s.blockWhy, s.fallback = 0, "", nil
	return nil
}
