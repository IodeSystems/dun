package dund

import (
	"io"
	"os"
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
	l.inject(&proto.TurnState{Phase: proto.PhaseWorking})
	ses, _ := svc.Get(alice, id)
	waitFor(t, func() bool { return len(ses.waiting()) > 0 })
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
	if r := recordOf(t, dir, id); r.Resume != "" {
		t.Fatalf("resume set for an adoptable session: %q", r.Resume)
	}
	took := l.seq()
	svc.Detach()
	waitFor(t, func() bool { return !l.attached() })
	if got := l.ackedSeq(); got != took {
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
	if fb := recordOf(t, dir, id).Fallback; fb != nil {
		t.Fatalf("fallback kept: %+v", fb)
	}
	if err := svc2.Send(alice, id, proto.NewCommand("", &proto.User{Content: "finish"})); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		return hasEvent(logEvents(t, ses), func(ev proto.Event) bool { _, ok := ev.Payload.(*proto.Done); return ok }) > 0
	})
}

// Adoption that fails falls back to the record: the session resumes on a
// new engine, as a stop's did. An engine that is provably ours is killed;
// one whose hello does not match its entry is left alone.
func TestAdoptFallsBack(t *testing.T) {
	for _, tc := range []struct {
		name   string
		breaks func(*fakeLink)
		killed bool
	}{
		{"proto mismatch", func(l *fakeLink) { l.mu.Lock(); l.proto = 99; l.mu.Unlock() }, true},
		{"dead socket", func(l *fakeLink) { l.die() }, false},
		{"session mismatch", func(l *fakeLink) { l.mu.Lock(); l.session = "another"; l.mu.Unlock() }, false},
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
			if tc.killed {
				waitFor(t, l.ended)
			} else if l.ended() {
				t.Fatal("an engine not proven ours was killed")
			}
			if tc.name == "dead socket" {
				if _, err := os.Stat(l.info); !os.IsNotExist(err) {
					t.Fatal("a dead engine's entry was kept")
				}
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
