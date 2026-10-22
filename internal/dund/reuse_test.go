package dund

import (
	"encoding/json"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/iodesystems/dun/proto"
)

// noHold lets a test hand out the same session again without waiting for the
// claim to lapse.
func noHold(t *testing.T) {
	t.Helper()
	old := reuseHold
	reuseHold = 0
	t.Cleanup(func() { reuseHold = old })
}

// A plain `dun` lands in the newest session of its workspace that nobody is
// attached to; an attached one, another workspace's, another user's and one
// with other flags are passed over; with none left a session is created.
func TestCreateOrReuseTakesTheNewestIdleSession(t *testing.T) {
	noHold(t)
	svc, sp, _ := newSvc(t)
	ws, other := t.TempDir(), t.TempDir()
	old, _ := svc.Create(alice, ws, nil, "")
	idle, _ := svc.Create(alice, ws, nil, "")
	attached, _ := svc.Create(alice, ws, nil, "")
	_, _ = svc.Create(alice, other, nil, "")
	_, _ = svc.Create(alice, ws, []string{"--model", "m"}, "")
	_, _ = svc.Create(bob, ws, nil, "")
	ses, _ := svc.Get(alice, attached)
	_, sub, _ := ses.log.Subscribe(0, 10)
	spawned := len(sp.spawnArgs())

	id, reused, err := svc.CreateOrReuse(alice, ws+"/", nil) // the same directory, spelled differently
	if err != nil || !reused || id != idle {
		t.Fatalf("got %q reused=%v err=%v, want %q (old %q)", id, reused, err, idle, old)
	}
	if len(sp.spawnArgs()) != spawned {
		t.Error("a reuse spawned an engine")
	}

	// Attach to idle and old too: nothing idle is left, so a new one.
	for _, x := range []string{idle, old} {
		s, _ := svc.Get(alice, x)
		_, sb, _ := s.log.Subscribe(0, 10)
		defer sb.Close()
	}
	id, reused, err = svc.CreateOrReuse(alice, ws, nil)
	if err != nil || reused || id == idle || id == old || id == attached {
		t.Fatalf("all attached: got %q reused=%v err=%v", id, reused, err)
	}

	// When attached's client leaves, the session just created is still the
	// newest one nobody is attached to.
	sub.Close()
	if again, reused, _ := svc.CreateOrReuse(alice, ws, nil); !reused || again != id {
		t.Fatalf("after detach: got %q reused=%v, want the newest %q", again, reused, id)
	}
}

// No sessions at all: a create, as before.
func TestCreateOrReuseCreatesWhenThereIsNone(t *testing.T) {
	svc, sp, _ := newSvc(t)
	id, reused, err := svc.CreateOrReuse(alice, t.TempDir(), nil)
	if err != nil || reused || id == "" || len(sp.spawnArgs()) != 1 {
		t.Fatalf("got %q reused=%v err=%v spawns=%d", id, reused, err, len(sp.spawnArgs()))
	}
}

// Two clients starting at once with one idle session: one gets it, the other
// a new session — never both the same one, although neither has subscribed
// yet when the other asks.
func TestCreateOrReuseIsAtomic(t *testing.T) {
	svc, _, _ := newSvc(t)
	ws := t.TempDir()
	idle, _ := svc.Create(alice, ws, nil, "")
	const n = 4
	ids := make([]string, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, _, err := svc.CreateOrReuse(alice, ws, nil)
			if err != nil {
				t.Error(err)
			}
			ids[i] = id
		}()
	}
	wg.Wait()
	seen, got := map[string]bool{}, 0
	for _, id := range ids {
		if seen[id] {
			t.Fatalf("two clients got session %s: %v", id, ids)
		}
		seen[id] = true
		if id == idle {
			got++
		}
	}
	if got != 1 {
		t.Fatalf("the idle session %s went to %d clients: %v", idle, got, ids)
	}
}

// The claim lapses: a client that never subscribed does not keep the session
// from the next one for good.
func TestCreateOrReuseClaimLapses(t *testing.T) {
	old := reuseHold
	reuseHold = 50 * time.Millisecond
	t.Cleanup(func() { reuseHold = old })
	svc, _, _ := newSvc(t)
	ws := t.TempDir()
	first, _, _ := svc.CreateOrReuse(alice, ws, nil)
	if id, reused, _ := svc.CreateOrReuse(alice, ws, nil); reused || id == first {
		t.Fatalf("within the hold: got %q reused=%v", id, reused)
	}
	time.Sleep(60 * time.Millisecond)
	if id, reused, _ := svc.CreateOrReuse(alice, ws, nil); !reused {
		t.Fatalf("after the hold: got %q reused=%v", id, reused)
	}
}

// Over the socket: create with reuse_idle answers reused, and the session
// list says how many clients are attached.
func TestServerReuseIdleAndAttachedCount(t *testing.T) {
	noHold(t)
	svc, _, _ := newSvc(t)
	sock := filepath.Join(t.TempDir(), "dund.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go NewServer(svc, UnixPeer).Serve(ln)
	ws := t.TempDir()

	a := dial(t, sock)
	a.send("c1", "", &proto.Create{Workspace: ws, ReuseIdle: true})
	ack := a.until(ackOf("c1"))
	if p := ack.Payload.(*proto.Ack); p.Error != "" || p.Reused {
		t.Fatalf("first: %+v", p)
	}
	id := ack.Session
	a.send("c2", id, &proto.Subscribe{})
	a.until(ackOf("c2"))

	a.send("c3", "", &proto.ListSessions{})
	items := a.until(typed("sessions")).Payload.(*proto.SessionList).Items
	if len(items) != 1 || items[0].Attached != 1 {
		t.Fatalf("attached: %+v", items)
	}

	// Attached: a second client gets a new session.
	b := dial(t, sock)
	b.send("c1", "", &proto.Create{Workspace: ws, ReuseIdle: true})
	if ack := b.until(ackOf("c1")); ack.Payload.(*proto.Ack).Reused || ack.Session == id {
		t.Fatalf("second while attached: %+v %+v", ack, ack.Payload)
	}

	// a leaves; b never subscribed and noHold lapses its claim at once, so a
	// third client gets an existing session (b's, the newer).
	a.send("c4", id, &proto.Detach{})
	a.until(ackOf("c4"))
	c := dial(t, sock)
	c.send("c1", "", &proto.Create{Workspace: ws, ReuseIdle: true})
	if ack := c.until(ackOf("c1")); !ack.Payload.(*proto.Ack).Reused || ack.Session == "" {
		t.Fatalf("third: %+v %+v", ack, ack.Payload)
	}
}

// The new field survives the wire.
func TestSessionSummaryAttachedRoundTrips(t *testing.T) {
	ev := proto.NewEvent(&proto.SessionList{Items: []proto.SessionSummary{{ID: "x", Attached: 2}}})
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	var back proto.Event
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if it := back.Payload.(*proto.SessionList).Items; len(it) != 1 || it[0].Attached != 2 {
		t.Fatalf("round trip: %s → %+v", b, it)
	}
	cmd := proto.NewCommand("c", &proto.Create{Workspace: "/w", ReuseIdle: true})
	b, _ = json.Marshal(cmd)
	var c proto.Command
	if err := json.Unmarshal(b, &c); err != nil || !c.Payload.(*proto.Create).ReuseIdle {
		t.Fatalf("create round trip: %s %v", b, err)
	}
}
