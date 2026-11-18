package dund

import (
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/dun/proto"
)

// engDir is a short directory for engine sockets (sun_path is ~104 bytes).
func engDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "de")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func logEvents(t *testing.T, ses *session) []proto.Event {
	t.Helper()
	evs, sub, err := ses.log.Subscribe(0, 100000)
	if err != nil {
		t.Fatal(err)
	}
	sub.Close()
	return evs
}

func hasEvent(evs []proto.Event, match func(proto.Event) bool) int {
	n := 0
	for _, ev := range evs {
		if match(ev) {
			n++
		}
	}
	return n
}

func tokenIs(text string) func(proto.Event) bool {
	return func(ev proto.Event) bool {
		tk, ok := ev.Payload.(*proto.Token)
		return ok && tk.Text == text
	}
}

func recordOf(t *testing.T, dir, id string) record {
	t.Helper()
	recs, err := (&Service{dir: dir}).loadRegistry()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("no record of %s", id)
	return record{}
}

// upgradeWorking starts a service on linked engines, puts a session in a
// turn, and runs an upgrade to the point where serve detaches.
func upgradeWorking(t *testing.T) (dir string, sp *linkSpawner, id string, l *fakeLink) {
	t.Helper()
	fastUpgrade(t)
	dir = t.TempDir()
	sp = newLinkSpawner(t, engDir(t))
	svc, err := New(dir, sp)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)
	id, err = svc.Create(alice, t.TempDir(), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Send(alice, id, proto.NewCommand("", &proto.User{Content: "slow"})); err != nil {
		t.Fatal(err)
	}
	l = sp.linkOf(id)
	ses, _ := svc.Get(alice, id)
	// The turn's own events first, then its turn_state, as an engine sends.
	waitFor(t, func() bool {
		return hasEvent(logEvents(t, ses), func(ev proto.Event) bool { _, ok := ev.Payload.(*proto.Turn); return ok }) > 0
	})
	l.inject(&proto.TurnState{Phase: proto.PhaseWorking})
	waitFor(t, func() bool {
		ses.actMu.Lock()
		defer ses.actMu.Unlock()
		return ses.turnStated && ses.inTurn // the injected turn_state is taken
	})
	if !ses.adoptable() {
		t.Fatal("a linked engine is not adoptable")
	}
	// The upgrade does not wait for it, holds nothing, interrupts nothing.
	if w := svc.Waiting(); len(w) != 0 {
		t.Fatalf("upgrade waits for %v", w)
	}
	if err := svc.RequestUpgrade("/x/dun", "v2"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-svc.UpgradeReady():
	case <-time.After(5 * time.Second):
		t.Fatal("the upgrade waited for an adoptable engine")
	}
	for _, g := range sp.received() {
		if strings.HasPrefix(g, "interrupt ") || strings.HasPrefix(g, "hold ") {
			t.Fatalf("an adoptable engine got %q", g)
		}
	}
	fb := recordOf(t, dir, id).Fallback
	if fb == nil || !strings.HasPrefix(fb.Resume, "[automatic] dund restarted into build v2; the new dund could not take back") {
		t.Fatalf("fallback = %+v", fb)
	}
	// Also where an older dund reads it, so a downgrade resumes as before.
	if r := recordOf(t, dir, id); r.Resume != fb.Resume {
		t.Fatalf("resume for an older dund: %q", r.Resume)
	}
	svc.Detach()
	waitFor(t, func() bool { return !l.attached() })
	// Everything the engine wrote was taken (it writes nothing more: its
	// turn waits), and acked exactly.
	if got, took := l.ackedSeq(), l.seq(); got != took {
		t.Fatalf("detach acked %d, the engine wrote %d", got, took)
	}
	return dir, sp, id, l
}

// An upgrade's restart leaves a working engine running; the next dund takes
// it back: no resume, the gap's events once, what the old dund took not
// again, its state current, and commands reach it.
func TestAdoptAcrossRestart(t *testing.T) {
	dir, sp, id, l := upgradeWorking(t)
	l.inject(&proto.Token{Text: "in the gap"}) // the turn runs on

	sp2 := sp.next()
	svc2, err := New(dir, sp2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc2.Close)
	ses, err := svc2.Get(alice, id)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return hasEvent(logEvents(t, ses), tokenIs("in the gap")) > 0 })
	evs := logEvents(t, ses)
	if n := hasEvent(evs, tokenIs("in the gap")); n != 1 {
		t.Fatalf("gap event %d times", n)
	}
	if n := hasEvent(evs, func(ev proto.Event) bool { u, ok := ev.Payload.(*proto.UserEcho); return ok && u.Content == "slow" }); n != 0 {
		t.Fatal("an event the old dund took came again")
	}
	if len(ses.waiting()) == 0 || !ses.adoptable() {
		t.Fatalf("adopted session not busy (waiting %v)", ses.waiting())
	}
	if len(sp2.spawnArgs()) != 0 {
		t.Fatalf("spawned %v", sp2.spawnArgs())
	}
	for _, g := range sp.received() {
		if strings.Contains(g, resumePrefix) {
			t.Fatalf("an adopted session was resumed: %q", g)
		}
	}
	if r := recordOf(t, dir, id); r.Fallback != nil || r.Resume != "" {
		t.Fatalf("record kept: %+v", r)
	}
	if err := svc2.Send(alice, id, proto.NewCommand("", &proto.User{Content: "finish"})); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		return hasEvent(logEvents(t, ses), func(ev proto.Event) bool { _, ok := ev.Payload.(*proto.Done); return ok }) > 0
	})
}

// Adoption that fails falls back to the record, once the engine is
// provably gone: it is killed first (endEngine), so the session never runs
// on two engines, then resumed on a new one, as a stop's was.
func TestAdoptFallsBack(t *testing.T) {
	for _, tc := range []struct {
		name   string
		breaks func(*fakeLink)
	}{
		{"proto mismatch", func(l *fakeLink) { l.mu.Lock(); l.proto = 99; l.mu.Unlock() }},
		{"dead socket", func(l *fakeLink) { l.die() }},
		{"session mismatch", func(l *fakeLink) { l.mu.Lock(); l.session = "another"; l.mu.Unlock() }},
		{"wrong token", func(l *fakeLink) { l.mu.Lock(); l.token = "other"; l.mu.Unlock() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, sp, id, l := upgradeWorking(t)
			tc.breaks(l)
			sp2 := sp.next()
			svc2, err := New(dir, sp2)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(svc2.Close)
			waitFor(t, func() bool {
				for _, g := range sp2.received() {
					if strings.HasPrefix(g, "user "+id+": [automatic] dund restarted into build v2; the new dund could not take back") {
						return true
					}
				}
				return false
			})
			if a := sp2.spawnArgs(); len(a) != 1 || !slices.Contains(a[0], "--resume") {
				t.Fatalf("spawns %v", a)
			}
			// Ended before the new engine started.
			if !l.ended() {
				t.Fatal("the old engine runs beside the new one")
			}
			if _, err := os.Stat(l.info); !os.IsNotExist(err) {
				t.Fatal("an ended engine's entry was kept")
			}
		})
	}
}

// An engine whose session the registry does not hold is ours (it answered
// on its socket) and is ended.
func TestAdoptRefusesAnEngineWithNoSession(t *testing.T) {
	dir := t.TempDir()
	sp := newLinkSpawner(t, engDir(t))
	e, err := sp.Spawn(t.TempDir(), []string{"--resume", "ghost"})
	if err != nil {
		t.Fatal(err)
	}
	l := sp.linkOf("")
	waitFor(t, func() bool { l = sp.linkOf("ghost"); return l != nil })
	go func() { _, _ = io.Copy(io.Discard, e.Stdout()) }() // as a session's reader would
	e.(*linkEngine).stopReading()
	e.(*linkEngine).finishDetach()
	svc, err := New(dir, sp.next())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)
	waitFor(t, l.ended)
}

// Events the engine could not keep while no dund was there: the new dund
// replaces the conversation from the transcript (history, reattach).
func TestAdoptGapSendsReattachHistory(t *testing.T) {
	dir, sp, id, l := upgradeWorking(t)
	l.inject(&proto.Token{Text: "lost"})
	l.lose()
	l.inject(&proto.Token{Text: "kept"})
	svc2, err := New(dir, sp.next())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc2.Close)
	ses, _ := svc2.Get(alice, id)
	waitFor(t, func() bool { return hasEvent(logEvents(t, ses), tokenIs("kept")) > 0 })
	evs := logEvents(t, ses)
	reattach := slices.IndexFunc(evs, func(ev proto.Event) bool {
		h, ok := ev.Payload.(*proto.History)
		return ok && h.Reason == proto.HistoryReattach
	})
	kept := slices.IndexFunc(evs, tokenIs("kept"))
	if reattach < 0 || reattach > kept || hasEvent(evs, tokenIs("lost")) != 0 {
		t.Fatalf("reattach at %d, kept at %d: %v", reattach, kept, evs)
	}
}

// An adopted engine of an older build moves to the new build at its next
// idle, not before.
func TestAdoptedOldBuildMovesAtIdle(t *testing.T) {
	dir, sp, id, l := upgradeWorking(t)
	sp2 := sp.next()
	sp2.build = "v2"
	l.mu.Lock()
	l.version = "v1"
	l.mu.Unlock()
	svc2, err := New(dir, sp2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc2.Close)
	ses, _ := svc2.Get(alice, id)
	waitFor(t, func() bool {
		ses.actMu.Lock()
		defer ses.actMu.Unlock()
		return ses.live
	})
	time.Sleep(50 * time.Millisecond)
	if len(sp2.spawnArgs()) != 0 || l.ended() {
		t.Fatal("moved while working")
	}
	_ = svc2.Send(alice, id, proto.NewCommand("", &proto.User{Content: "finish"}))
	l.inject(&proto.TurnState{Phase: proto.PhaseIdle})
	waitFor(t, func() bool { return len(sp2.spawnArgs()) == 1 })
	if a := sp2.spawnArgs()[0]; !slices.Contains(a, "--resume") || a[slices.Index(a, "--resume")+1] != id {
		t.Fatalf("spawn %v", a)
	}
	waitFor(t, l.ended)
	if s := recordOf(t, dir, id); s.Resume != "" {
		t.Fatalf("a move left a resume: %q", s.Resume)
	}
}

// An engine that is not adopted and cannot be proven gone (or killed): its
// session is not resumed, starts no engine, and dun ps shows it, until the
// process is gone.
func TestAdoptUnprovableEngineBlocksTheSession(t *testing.T) {
	dir, sp, id, l := upgradeWorking(t)
	l.unprovable.Store(true)
	l.die()
	sp2 := sp.next()
	svc2, err := New(dir, sp2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc2.Close)
	time.Sleep(100 * time.Millisecond)
	if len(sp2.spawnArgs()) != 0 {
		t.Fatalf("a second engine started: %v", sp2.spawnArgs())
	}
	err = svc2.Send(alice, id, proto.NewCommand("", &proto.User{Content: "hi"}))
	if err == nil || !strings.Contains(err.Error(), "may still be running") {
		t.Fatalf("send: %v", err)
	}
	ps := svc2.Procs(alice)
	if len(ps.Sessions) != 1 || ps.Sessions[0].EnginePID != l.pid || ps.Sessions[0].Blocked == "" {
		t.Fatalf("dun ps: %+v", ps.Sessions)
	}
	l.kill() // a person ends it
	<-l.gone
	if err := svc2.Send(alice, id, proto.NewCommand("", &proto.User{Content: "hi"})); err != nil {
		t.Fatalf("after it is gone: %v", err)
	}
	if len(sp2.spawnArgs()) != 1 {
		t.Fatalf("spawns %v", sp2.spawnArgs())
	}
}

// dund acks what it took though fewer than linkAckEvery events came (a
// few large ones fill the engine's ring): after linkAckIdle, or at
// linkAckBytes.
func TestLinkAcksFewOrLargeEvents(t *testing.T) {
	for _, tc := range []struct {
		name  string
		idle  time.Duration
		bytes int
		text  string
	}{
		{"idle", 50 * time.Millisecond, 1 << 30, "small"},
		{"bytes", time.Hour, 100, strings.Repeat("x", 200)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oldIdle, oldBytes := linkAckIdle, linkAckBytes
			linkAckIdle, linkAckBytes = tc.idle, tc.bytes
			t.Cleanup(func() { linkAckIdle, linkAckBytes = oldIdle, oldBytes })
			svc, sp, _ := newLinkSvc(t)
			id, err := svc.Create(alice, t.TempDir(), nil, "")
			if err != nil {
				t.Fatal(err)
			}
			l := sp.linkOf(id)
			l.inject(&proto.Token{Text: tc.text})
			want := l.seq()
			waitFor(t, func() bool { return l.ackedSeq() >= want }) // the engine's own events may come after
		})
	}
}

func newLinkSvc(t *testing.T) (*Service, *linkSpawner, string) {
	t.Helper()
	dir := t.TempDir()
	sp := newLinkSpawner(t, engDir(t))
	svc, err := New(dir, sp)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)
	return svc, sp, dir
}

// An adopted engine that held shared MCP servers: its agent and its clients
// are told they are gone until its next idle.
func TestAdoptedSharedServersAreSaid(t *testing.T) {
	dir, sp, id, l := upgradeWorking(t)
	l.mu.Lock()
	l.shared = 2
	l.mu.Unlock()
	svc2, err := New(dir, sp.next())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc2.Close)
	waitFor(t, func() bool {
		for _, g := range sp.received() {
			if strings.HasPrefix(g, "notice "+id+": dund restarted: the shared MCP servers") {
				return true
			}
		}
		return false
	})
	ses, _ := svc2.Get(alice, id)
	if hasEvent(logEvents(t, ses), func(ev proto.Event) bool {
		n, ok := ev.Payload.(*proto.Notification)
		return ok && strings.Contains(n.Text, "shared MCP servers")
	}) != 1 {
		t.Fatal("clients were not told")
	}
}

// A --docker engine is not on the link: its container would outlive the
// process the next dund adopts.
func TestDockerEngineIsNotLinked(t *testing.T) {
	sp := ExecSpawner{Bin: "/nonexistent/dun", EnginesDir: engDir(t)}
	_, err := sp.Spawn(t.TempDir(), []string{"--docker", "dun:local"})
	if err == nil || strings.Contains(err.Error(), "listen") {
		t.Fatalf("spawn: %v", err)
	}
	if logs, _ := filepath.Glob(filepath.Join(sp.EnginesDir, "*.log")); len(logs) != 0 {
		t.Fatalf("a linked spawn: %v", logs)
	}
}
