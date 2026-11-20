package dund

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/dun/proto"
)

// upgradeIdle is Carl's sequence up to the new dund: a session whose
// engine (build v1, its ready says no build) is idle with a client
// attached, then /upgrade now: dund restarts into v2 and adopts it. before
// runs on the old dund first (a job left running, say).
func upgradeIdle(t *testing.T, before ...string) (sp2 *linkSpawner, ses *session, id string) {
	t.Helper()
	fastUpgrade(t)
	dir := t.TempDir()
	sp := newLinkSpawner(t, engDir(t))
	sp.version = "v1"
	svc, err := New(dir, sp)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)
	id, err = svc.Create(alice, t.TempDir(), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	old, _ := svc.Get(alice, id)
	for _, c := range append([]string{"hi"}, before...) {
		if err := svc.Send(alice, id, proto.NewCommand("", &proto.User{Content: c})); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, func() bool {
		return hasEvent(logEvents(t, old), func(ev proto.Event) bool { _, ok := ev.Payload.(*proto.Done); return ok }) > 0
	})
	waitFor(t, func() bool { // what before started is running
		old.actMu.Lock()
		defer old.actMu.Unlock()
		return len(old.jobs) == len(before)
	})
	_, _, sub, err := old.subscribe(0, "") // the TUI
	if err != nil {
		t.Fatal(err)
	}
	sub.Close()
	if err := svc.RequestUpgrade("/x/dun", "v2"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-svc.UpgradeReady():
	case <-time.After(5 * time.Second):
		t.Fatal("the upgrade waited")
	}
	l := sp.linkOf(id)
	svc.Detach()
	waitFor(t, func() bool { return !l.attached() }) // the old dund is gone, its acks taken
	sp2 = sp.next()
	sp2.build, sp2.version, sp2.ready = "v2", "v2", "v2"
	svc2, err := New(dir, sp2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc2.Close)
	ses, err = svc2.Get(alice, id)
	if err != nil {
		t.Fatal(err)
	}
	return sp2, ses, id
}

// readyBuilds is the builds the ready events a client attaching now gets
// say, in order: the snapshot's, then the live ones until want or d.
func readyBuilds(t *testing.T, ses *session, want string, d time.Duration) []string {
	t.Helper()
	snap, replay, sub, err := ses.subscribe(5, "another dund's epoch")
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	var got []string
	take := func(ev proto.Event) bool {
		if r, ok := ev.Payload.(*proto.Ready); ok {
			got = append(got, r.Version)
			return r.Version == want
		}
		return false
	}
	if snap != nil {
		for _, ev := range snap.Payload.(*proto.Snapshot).State {
			if take(ev) {
				return got
			}
		}
	}
	for _, ev := range replay {
		if take(ev) {
			return got
		}
	}
	end := time.After(d)
	for {
		select {
		case ev, ok := <-sub.C():
			if !ok || take(ev) {
				return got
			}
		case <-end:
			return got
		}
	}
}

// An idle adopted engine moves to dund's build at once, and a client
// attached through it sees that build (heldAfterUpgrade checks that it
// first sees the old one, from the link hello: its ready says none).
func TestUpgradeNowIdleEngineMovesAndClientsSeeIt(t *testing.T) {
	sp2, ses, _ := upgradeIdle(t)
	got := readyBuilds(t, ses, "v2", 5*time.Second)
	if len(got) == 0 || got[len(got)-1] != "v2" { // v1 first unless it already moved
		t.Fatalf("ready builds a client saw: %q", got)
	}
	if len(sp2.spawnArgs()) != 1 {
		t.Fatalf("spawns %v", sp2.spawnArgs())
	}
}

// heldAfterUpgrade is upgradeIdle with a job left running: the adopted
// engine is live and has not moved.
func heldAfterUpgrade(t *testing.T) (*linkSpawner, *session, string) {
	t.Helper()
	sp2, ses, id := upgradeIdle(t, "job")
	waitFor(t, func() bool {
		ses.actMu.Lock()
		defer ses.actMu.Unlock()
		return ses.live
	})
	time.Sleep(50 * time.Millisecond)
	if len(sp2.spawnArgs()) != 0 {
		t.Fatalf("moved with a job running: %v", sp2.spawnArgs())
	}
	if got := readyBuilds(t, ses, "v2", 0); len(got) != 1 || got[0] != "v1" {
		t.Fatalf("before the move a client saw %q", got)
	}
	return sp2, ses, id
}

// move_engine on a held engine moves nothing and says what holds it; the
// move at idle takes it when the job ends, and a client sees the new build.
func TestMoveEngineBusyWaitsAndSaysWhy(t *testing.T) {
	sp2, ses, id := heldAfterUpgrade(t)
	err := ses.svc.MoveEngine(alice, id, false)
	if err == nil || !strings.Contains(err.Error(), "this session's engine is busy (1 job running): it moves when they finish; /upgrade now --force ends them and moves now") {
		t.Fatalf("busy: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if len(sp2.spawnArgs()) != 0 {
		t.Fatalf("moved though busy: %v", sp2.spawnArgs())
	}
	if err := ses.svc.Send(alice, id, proto.NewCommand("", &proto.User{Content: "endjob"})); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return len(sp2.spawnArgs()) == 1 })
	if got := readyBuilds(t, ses, "v2", 5*time.Second); got[len(got)-1] != "v2" {
		t.Fatalf("after the job a client saw %q", got)
	}
}

// Force moves a held engine now.
func TestMoveEngineForceMovesNow(t *testing.T) {
	sp2, ses, id := heldAfterUpgrade(t)
	if err := ses.svc.MoveEngine(alice, id, true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return len(sp2.spawnArgs()) == 1 })
	if got := readyBuilds(t, ses, "v2", 5*time.Second); got[len(got)-1] != "v2" {
		t.Fatalf("after --force a client saw %q", got)
	}
}

// A held engine dund did not adopt (no move pending) is marked to move:
// it moves when its job ends.
func TestMoveEngineBusyNotAdoptedMovesAtIdle(t *testing.T) {
	dir := t.TempDir()
	sp := newLinkSpawner(t, engDir(t))
	svc, err := New(dir, sp)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)
	id, err := svc.Create(alice, t.TempDir(), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	_ = svc.Send(alice, id, proto.NewCommand("", &proto.User{Content: "job"}))
	ses, _ := svc.Get(alice, id)
	waitFor(t, func() bool { ses.actMu.Lock(); defer ses.actMu.Unlock(); return len(ses.jobs) == 1 })
	if err := svc.MoveEngine(alice, id, false); err == nil {
		t.Fatal("moved though busy")
	}
	_ = svc.Send(alice, id, proto.NewCommand("", &proto.User{Content: "endjob"}))
	waitFor(t, func() bool { return len(sp.spawnArgs()) == 2 })
}

// The TUI's line for it is the command the server takes.
func TestMoveEngineCommandDecodes(t *testing.T) {
	var c proto.Command
	if err := json.Unmarshal([]byte(`{"type":"move_engine","force":true,"v":`+strconv.Itoa(proto.Version)+`,"session":"s1"}`), &c); err != nil {
		t.Fatal(err)
	}
	if p, ok := c.Payload.(*proto.MoveEngine); !ok || !p.Force || c.Session != "s1" {
		t.Fatalf("decoded %#v", c)
	}
}

// What holds an engine, as the busy answer names it.
func TestHoldersNamesWhatHoldsTheEngine(t *testing.T) {
	s := &session{inTurn: true, jobs: []proto.JobInfo{{ID: 3}, {ID: 4}}, agents: []proto.AgentInfo{{ID: 2}}}
	if got := strings.Join(s.holders(), ", "); got != "a turn running, 2 jobs running, agent #2 working" {
		t.Errorf("holders %q", got)
	}
}
