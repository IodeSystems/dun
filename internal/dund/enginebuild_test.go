package dund

import (
	"encoding/json"
	"strconv"
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
	svc.Detach()
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
			take(ev)
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

// The adopted engine's build reaches the client at once (from its link
// hello: its ready says none), then the build it moves to at idle.
func TestUpgradeNowIdleEngineMovesAndClientsSeeIt(t *testing.T) {
	sp2, ses, _ := upgradeIdle(t)
	got := readyBuilds(t, ses, "v2", 5*time.Second)
	if len(got) < 2 || got[0] != "v1" || got[len(got)-1] != "v2" {
		t.Fatalf("ready builds a client saw: %q", got)
	}
	if len(sp2.spawnArgs()) != 1 {
		t.Fatalf("spawns %v", sp2.spawnArgs())
	}
}

// A job left running holds the move at idle; move_engine (/upgrade now on
// a dund already of the TUI's build) moves it at once, and a client sees
// the new build.
func TestUpgradeNowBusyEngineMovesOnAsk(t *testing.T) {
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
	if err := ses.svc.MoveEngine(alice, id); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return len(sp2.spawnArgs()) == 1 })
	got := readyBuilds(t, ses, "v2", 5*time.Second)
	if got[len(got)-1] != "v2" {
		t.Fatalf("after move_engine a client saw %q", got)
	}
}

// The TUI's line for it is the command the server takes.
func TestMoveEngineCommandDecodes(t *testing.T) {
	var c proto.Command
	if err := json.Unmarshal([]byte(`{"type":"move_engine","v":`+strconv.Itoa(proto.Version)+`,"session":"s1"}`), &c); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Payload.(*proto.MoveEngine); !ok || c.Session != "s1" {
		t.Fatalf("decoded %#v", c)
	}
}
