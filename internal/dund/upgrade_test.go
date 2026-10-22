package dund

import (
	"net"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/dun/proto"
)

func fastUpgrade(t *testing.T) {
	t.Helper()
	oldPoll, oldGrace := upgradePoll, upgradeGrace
	upgradePoll = 10 * time.Millisecond
	t.Cleanup(func() { upgradePoll, upgradeGrace = oldPoll, oldGrace })
}

// serve runs a Server for svc and returns its socket.
func serve(t *testing.T, svc *Service) (string, *Server) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "dund.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	srv := NewServer(svc, func(net.Conn) (string, error) { return alice, nil })
	go srv.Serve(ln)
	return sock, srv
}

// working starts a session and puts it in a slow turn (and a job, if job).
func working(t *testing.T, svc *Service, job bool) string {
	t.Helper()
	id, err := svc.Create(alice, t.TempDir(), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Send(alice, id, proto.NewCommand("", &proto.User{Content: "slow"})); err != nil {
		t.Fatal(err)
	}
	if job {
		_ = svc.Send(alice, id, proto.NewCommand("", &proto.User{Content: "job"}))
	}
	ses, _ := svc.Get(alice, id)
	waitFor(t, func() bool {
		w := ses.waiting()
		return len(w) > 0 && w[0].What == "turn" && (!job || len(w) == 2)
	})
	return id
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func upgradeOf(state string) func(proto.Event) bool {
	return func(ev proto.Event) bool {
		u, ok := ev.Payload.(*proto.UpgradeStatus)
		return ok && u.State == state
	}
}

func whats(ws []proto.UpgradeWait) []string {
	var out []string
	for _, w := range ws {
		out = append(out, w.What)
	}
	return out
}

// A pending upgrade reaches a connection that follows nothing, names what it
// waits for, updates as each thing ends, and says "restarting" before the
// restart is handed to the serve loop.
func TestUpgradePendingIsBroadcastAndFollowsTheWork(t *testing.T) {
	fastUpgrade(t)
	svc, _, _ := newSvc(t)
	sock, _ := serve(t, svc)
	id := working(t, svc, true)
	other := dial(t, sock) // follows no session
	other.send("l", "", &proto.ListSessions{})
	other.until(ackOrReply("l"))

	if err := svc.RequestUpgrade("/x/dun", "v2"); err != nil {
		t.Fatal(err)
	}
	ev := other.until(upgradeOf("pending"))
	u := ev.Payload.(*proto.UpgradeStatus)
	if ev.Session != "" || u.Version != "v2" || u.Exe != "/x/dun" || !slices.Equal(whats(u.Waiting), []string{"turn", "job"}) ||
		u.Waiting[0].Session != id || u.Waiting[1].ID != 3 || u.Waiting[1].Detail != "go test ./..." || u.Deadline == 0 {
		t.Fatalf("pending: %+v", u)
	}

	// The turn ends: the list now names only the job.
	_ = svc.Send(alice, id, proto.NewCommand("", &proto.User{Content: "finish"}))
	ev = other.until(func(ev proto.Event) bool {
		u, ok := ev.Payload.(*proto.UpgradeStatus)
		return ok && slices.Equal(whats(u.Waiting), []string{"job"})
	})
	select {
	case <-svc.UpgradeReady():
		t.Fatal("restarted while a job ran")
	default:
	}

	// A connection made now gets the status at once, and hello carries it.
	late := dial(t, sock)
	late.until(upgradeOf("pending"))
	late.send("h", "", &proto.Hello{Version: "v1"})
	if r := late.until(typed("hello")).Payload.(*proto.HelloReply); !r.Coordinated || r.Upgrade == nil || r.Upgrade.State != "pending" {
		t.Fatalf("hello: %+v", r)
	}

	// The job ends: restarting is sent, then the serve loop gets the binary.
	_ = svc.Send(alice, id, proto.NewCommand("", &proto.User{Content: "endjob"}))
	other.until(upgradeOf("restarting"))
	select {
	case exe := <-svc.UpgradeReady():
		if exe != "/x/dun" {
			t.Fatalf("ready with %q", exe)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("never ready")
	}
	// Nothing was cut short: nothing is flagged to resume.
	ses, _ := svc.Get(alice, id)
	if ses.resumeNote() != "" {
		t.Fatalf("an idle restart flagged %s: %q", id, ses.resumeNote())
	}
}

func ackOrReply(cid string) func(proto.Event) bool {
	return func(ev proto.Event) bool { return ev.Re == cid }
}

// A request while one is pending changes the target; the restart goes to the
// latest.
func TestUpgradeRequestWhilePendingRetargets(t *testing.T) {
	fastUpgrade(t)
	svc, _, _ := newSvc(t)
	sock, _ := serve(t, svc)
	id := working(t, svc, false)
	c := dial(t, sock)
	_ = svc.RequestUpgrade("/x/dun", "v2")
	c.until(upgradeOf("pending"))
	_ = svc.RequestUpgrade("/y/dun", "v3")
	c.until(func(ev proto.Event) bool {
		u, ok := ev.Payload.(*proto.UpgradeStatus)
		return ok && u.Exe == "/y/dun" && u.Version == "v3"
	})
	_ = svc.Send(alice, id, proto.NewCommand("", &proto.User{Content: "finish"}))
	select {
	case exe := <-svc.UpgradeReady():
		if exe != "/y/dun" {
			t.Fatalf("ready with %q", exe)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("never ready")
	}
	if err := svc.RequestUpgrade("/z/dun", "v4"); err == nil {
		t.Fatal("a request while restarting was taken")
	}
}

// "Now" interrupts the running turn with the upgrade as the reason, flags
// that session (not an idle one) in the registry, and the next service wakes
// only it with the resume message, once.
func TestUpgradeNowInterruptsRecordsAndResumes(t *testing.T) {
	fastUpgrade(t)
	svc, sp, dir := newSvc(t)
	busy := working(t, svc, true)
	idle, _ := svc.Create(alice, t.TempDir(), nil, "")
	_ = svc.RequestUpgrade("/x/dun", "v2")
	if err := svc.UpgradeNow(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-svc.UpgradeReady():
	case <-time.After(5 * time.Second):
		t.Fatal("never ready")
	}
	if !slices.Contains(sp.received(), "interrupt "+busy+": dund restarted into build v2 while this turn was running") {
		t.Fatalf("interrupt reason: %q", sp.received())
	}
	svc.Close()

	recs, err := (&Service{dir: dir}).loadRegistry()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		switch r.ID {
		case busy:
			if !strings.HasPrefix(r.Resume, "[automatic] dund restarted into build v2 while you were working; your turn was interrupted") ||
				!strings.Contains(r.Resume, "#3 `go test ./...`") {
				t.Fatalf("resume note: %q", r.Resume)
			}
		case idle:
			if r.Resume != "" {
				t.Fatalf("idle session flagged: %q", r.Resume)
			}
		}
	}

	sp2 := &fakeSpawner{}
	svc2, err := New(dir, sp2)
	if err != nil {
		t.Fatal(err)
	}
	defer svc2.Close()
	waitFor(t, func() bool {
		for _, g := range sp2.received() {
			if strings.HasPrefix(g, "user "+busy+": [automatic] dund restarted into build v2") {
				return true
			}
		}
		return false
	})
	for _, g := range sp2.received() {
		if strings.HasPrefix(g, "user "+idle) {
			t.Fatalf("an idle session was woken: %q", g)
		}
	}
	waitFor(t, func() bool {
		recs, _ := (&Service{dir: dir}).loadRegistry()
		for _, r := range recs {
			if r.Resume != "" {
				return false
			}
		}
		return true
	})
}

// Pending past the grace period, it restarts anyway, interrupting the turn.
func TestUpgradeGraceRestartsAnyway(t *testing.T) {
	fastUpgrade(t)
	upgradeGrace = 100 * time.Millisecond
	svc, sp, _ := newSvc(t)
	busy := working(t, svc, false)
	_ = svc.RequestUpgrade("/x/dun", "")
	select {
	case <-svc.UpgradeReady():
	case <-time.After(5 * time.Second):
		t.Fatal("the grace period did not restart it")
	}
	if !slices.Contains(sp.received(), "interrupt "+busy+": dund restarted into a new build while this turn was running") {
		t.Fatalf("interrupt reason: %q", sp.received())
	}
}

// A planned stop (SIGTERM) flags the working sessions the same way.
func TestPrepareStopFlagsWorkingSessions(t *testing.T) {
	svc, _, _ := newSvc(t)
	busy := working(t, svc, false)
	idle, _ := svc.Create(alice, t.TempDir(), nil, "")
	svc.PrepareStop("dund was stopped (terminated)")
	b, _ := svc.Get(alice, busy)
	i, _ := svc.Get(alice, idle)
	if !strings.HasPrefix(b.resumeNote(), "[automatic] dund was stopped (terminated) while you were working") || i.resumeNote() != "" {
		t.Fatalf("busy %q idle %q", b.resumeNote(), i.resumeNote())
	}
	if len(b.waiting()) != 0 {
		t.Fatalf("the turn still runs: %+v", b.waiting())
	}
}

// UpgradeNow with nothing pending is refused.
func TestUpgradeNowWithNothingPending(t *testing.T) {
	svc, _, _ := newSvc(t)
	if err := svc.UpgradeNow(); err == nil {
		t.Fatal("accepted")
	}
}

// A message for a session cut by a crash says so.
func TestResumeMessage(t *testing.T) {
	m := resumeMessage("dund stopped unexpectedly", true, true, nil, []proto.AgentInfo{{ID: 2}})
	for _, want := range []string{"[automatic] dund stopped unexpectedly while you were working; your turn was interrupted",
		"ask it again", "sub-agents: #2", "Resume the task."} {
		if !strings.Contains(m, want) {
			t.Errorf("%q lacks %q", m, want)
		}
	}
}
