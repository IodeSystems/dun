package dund

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/dun/proto"
)

// The drain gate and the durable resume (USER 2026-11-17).

// restartInto waits for svc's restart, closes it and starts the next dund
// on dir.
func restartInto(t *testing.T, svc *Service, dir string, sp2 *fakeSpawner) *Service {
	t.Helper()
	select {
	case <-svc.UpgradeReady():
	case <-time.After(5 * time.Second):
		t.Fatal("never ready")
	}
	svc.Close()
	svc2, err := New(dir, sp2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc2.Close)
	return svc2
}

func count(sp *fakeSpawner, prefix string) int {
	n := 0
	for _, r := range sp.received() {
		if strings.HasPrefix(r, prefix) {
			n++
		}
	}
	return n
}

// While an upgrade is pending an automatic message (dun mcp's, Auto) is not
// delivered: the sender is told "queued", it is kept in the registry, and
// the next dund sends it. A person's message starts its turn at once, also
// in a --worker session, and the upgrade waits for it.
func TestUpgradeQueuesAutomaticMessagesAndNotTheHumans(t *testing.T) {
	fastUpgrade(t)
	svc, sp, dir := newSvc(t)
	sock, _ := serve(t, svc)
	busy := working(t, svc, false)
	idle, _ := svc.Create(alice, t.TempDir(), nil, "")
	_ = svc.RequestUpgrade("/x/dun", "v2")
	c := dial(t, sock)
	c.until(upgradeOf("pending"))
	// dun_start while pending: the session is made, its first message waits.
	worker, err := svc.Create(alice, t.TempDir(), []string{"--worker"}, "")
	if err != nil {
		t.Fatal(err)
	}
	waitRecord(t, sp, "hold "+worker+": true") // held from its start

	c.send("", idle, &proto.User{Content: "from a program", Auto: true})
	if a := c.until(typed("ack")).Payload.(*proto.Ack); a.Queued != queuedText || a.Error != "" {
		t.Fatalf("auto: %+v", a)
	}
	c.send("w", worker, &proto.User{Content: "build it", Auto: true})
	if a := c.until(ackOrReply("w")).Payload.(*proto.Ack); a.Queued == "" {
		t.Fatalf("worker: %+v", a)
	}
	// A person typing into the worker session is a person.
	c.send("p", worker, &proto.User{Content: "status?"})
	if a := c.until(ackOrReply("p")).Payload.(*proto.Ack); a.Queued != "" || a.Error != "" {
		t.Fatalf("person in a worker: %+v", a)
	}
	waitRecord(t, sp, "user "+worker+": status?")
	// The person's message starts now; the upgrade waits for its turn too.
	c.send("h", idle, &proto.User{Content: "slow"})
	if a := c.until(ackOrReply("h")).Payload.(*proto.Ack); a.Queued != "" || a.Error != "" {
		t.Fatalf("human: %+v", a)
	}
	waitRecord(t, sp, "user "+idle+": slow")
	is, _ := svc.Get(alice, idle)
	waitFor(t, func() bool { return len(is.waiting()) == 1 })
	time.Sleep(5 * upgradePoll)
	if n := count(sp, "user "+idle+": from a program") + count(sp, "user "+worker+": build it"); n != 0 {
		t.Fatalf("an automatic message was delivered while pending: %q", sp.received())
	}
	recs, _ := (&Service{dir: dir}).loadRegistry()
	for _, r := range recs {
		if (r.ID == idle && !slices.Equal(r.Queued, []string{"from a program"})) || (r.ID == worker && !slices.Equal(r.Queued, []string{"build it"})) {
			t.Fatalf("registry %s: %q", r.ID, r.Queued)
		}
	}

	_ = svc.Send(alice, busy, proto.NewCommand("", &proto.User{Content: "finish"}))
	_ = svc.Send(alice, idle, proto.NewCommand("", &proto.User{Content: "finish"}))
	sp2 := &fakeSpawner{}
	restartInto(t, svc, dir, sp2)
	waitRecord(t, sp2, "user "+idle+": from a program")
	waitRecord(t, sp2, "user "+worker+": build it") // kept although nothing was asked in it yet
	time.Sleep(50 * time.Millisecond)
	if n := count(sp2, "user "+worker+": build it"); n != 1 {
		t.Fatalf("sent %d times", n)
	}
	waitFor(t, func() bool {
		recs, _ := (&Service{dir: dir}).loadRegistry()
		for _, r := range recs {
			if len(r.Queued) > 0 {
				return false
			}
		}
		return true
	})
}

// A failed upgrade lets the engines' own wakes go again and sends the
// automatic messages it held, at once.
func TestUpgradeFailureReleasesTheHold(t *testing.T) {
	fastUpgrade(t)
	svc, sp, _ := newSvc(t)
	busy := working(t, svc, false)
	idle, _ := svc.Create(alice, t.TempDir(), nil, "")
	svc.SetUpgradeInstall(func(string, string) error { return errors.New("no room on disk") })
	_ = svc.RequestUpgrade("/x/dun", "v2")
	waitRecord(t, sp, "hold "+idle+": true")
	is, _ := svc.Get(alice, idle)
	if held, err := svc.queueIfUpgrading(is, proto.NewCommand("", &proto.User{Content: "later", Auto: true})); !held || err != nil {
		t.Fatalf("held %v, %v", held, err)
	}
	_ = svc.Send(alice, busy, proto.NewCommand("", &proto.User{Content: "finish"})) // the install fails
	waitRecord(t, sp, "hold "+idle+": false")
	waitRecord(t, sp, "user "+idle+": later")
}

// The registry write that records the resume comes before any interrupt;
// when it fails, nothing is interrupted, the upgrade is pending again with
// the error, and it goes once the write can be made.
func TestUpgradeRecordFailureAborts(t *testing.T) {
	fastUpgrade(t)
	old := upgradeRetry
	upgradeRetry = 20 * time.Millisecond
	t.Cleanup(func() { upgradeRetry = old })
	svc, sp, dir := newSvc(t)
	sock, _ := serve(t, svc)
	c := dial(t, sock)
	busy := working(t, svc, false)
	orig := writeFileAtomic
	writeFileAtomic = func(string, []byte) error { return errors.New("no room") }
	t.Cleanup(func() { writeFileAtomic = orig })
	_ = svc.RequestUpgrade("/x/dun", "v2")
	_ = svc.UpgradeNow()
	ev := c.until(func(ev proto.Event) bool {
		u, ok := ev.Payload.(*proto.UpgradeStatus)
		return ok && u.State == "pending" && u.Error != ""
	})
	if u := ev.Payload.(*proto.UpgradeStatus); !strings.Contains(u.Error, "none was interrupted") {
		t.Fatalf("error: %q", u.Error)
	}
	select {
	case <-svc.UpgradeReady():
		t.Fatal("restarted without the record")
	case <-time.After(50 * time.Millisecond):
	}
	if n := count(sp, "interrupt "+busy); n != 0 {
		t.Fatalf("interrupted without the record: %q", sp.received())
	}
	if b, _ := svc.Get(alice, busy); b.resumeNote() != "" || len(b.waiting()) != 1 {
		t.Fatalf("after the failed write: resume %q, waiting %v", b.resumeNote(), b.waiting())
	}
	writeFileAtomic = orig // the write works again
	select {
	case <-svc.UpgradeReady():
	case <-time.After(5 * time.Second):
		t.Fatal("never ready once the write works")
	}
	waitRecord(t, sp, "interrupt "+busy)
	recs, _ := (&Service{dir: dir}).loadRegistry()
	for _, r := range recs {
		if r.ID == busy && !strings.Contains(r.Resume, "your turn was interrupted") {
			t.Fatalf("resume %q", r.Resume)
		}
	}
}

// A record that keeps failing ends the upgrade as failed: the hold ends.
func TestUpgradeRecordFailureGivesUp(t *testing.T) {
	fastUpgrade(t)
	oldR, oldG := upgradeRetry, upgradeRecordGiveUp
	upgradeRetry, upgradeRecordGiveUp = 10*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { upgradeRetry, upgradeRecordGiveUp = oldR, oldG })
	svc, sp, _ := newSvc(t)
	busy := working(t, svc, false)
	orig := writeFileAtomic
	writeFileAtomic = func(string, []byte) error { return errors.New("no room") }
	t.Cleanup(func() { writeFileAtomic = orig })
	_ = svc.RequestUpgrade("/x/dun", "v2")
	_ = svc.UpgradeNow()
	waitRecord(t, sp, "hold "+busy+": false")
	if svc.UpgradeStatus() != nil {
		t.Fatal("still pending")
	}
	if n := count(sp, "interrupt "+busy); n != 0 {
		t.Fatalf("interrupted: %q", sp.received())
	}
}

// A cut turn is continued once: the message names the tool call in flight,
// and the turn runs. Cut again before it ends, the next start leaves a
// quiet note and wakes nothing.
func TestUpgradeContinuesACutTurnOnce(t *testing.T) {
	fastUpgrade(t)
	svc, _, dir := newSvc(t)
	id, _ := svc.Create(alice, t.TempDir(), nil, "")
	_ = svc.Send(alice, id, proto.NewCommand("", &proto.User{Content: "slowtool"}))
	ses, _ := svc.Get(alice, id)
	waitFor(t, func() bool { return len(ses.waiting()) == 1 })
	_ = svc.RequestUpgrade("/x/dun", "v2")
	_ = svc.UpgradeNow()

	sp2 := &fakeSpawner{}
	sp2.slowAuto.Store(true) // the continued turn runs, and is still running
	svc2 := restartInto(t, svc, dir, sp2)
	got := waitRecord(t, sp2, "user "+id+": [automatic] dund restarted into build v2")
	if !strings.Contains(got, "your turn was interrupted") || !strings.Contains(got, "`exec {\"cmd\":\"make deploy\"}`") {
		t.Fatalf("continue: %q", got)
	}
	s2, _ := svc2.Get(alice, id)
	waitFor(t, func() bool { return len(s2.waiting()) == 1 }) // the turn runs
	_ = svc2.RequestUpgrade("/x/dun", "v3")
	_ = svc2.UpgradeNow()

	sp3 := &fakeSpawner{}
	restartInto(t, svc2, dir, sp3)
	note := waitRecord(t, sp3, "notice "+id+": ")
	if !strings.Contains(note, "dund restarted into build v3") || !strings.Contains(note, "not continued automatically") {
		t.Fatalf("note: %q", note)
	}
	time.Sleep(50 * time.Millisecond)
	if n := count(sp3, "user "+id); n != 0 {
		t.Fatalf("woken again: %q", sp3.received())
	}
	if n := count(sp2, "user "+id+": [automatic]"); n != 1 {
		t.Fatalf("continued %d times", n)
	}
}

// A sub-agent the restart cut mid-run is told to continue its task after
// the restart, and the parent's message says so.
func TestUpgradeResumesACutSubAgent(t *testing.T) {
	fastUpgrade(t)
	svc, _, dir := newSvc(t)
	id := working(t, svc, false)
	_ = svc.Send(alice, id, proto.NewCommand("", &proto.User{Content: "agent"}))
	ses, _ := svc.Get(alice, id)
	waitFor(t, func() bool { return len(ses.waiting()) == 2 })
	_ = svc.RequestUpgrade("/x/dun", "v2")
	_ = svc.UpgradeNow()
	sp2 := &fakeSpawner{}
	restartInto(t, svc, dir, sp2)
	msg := waitRecord(t, sp2, "user "+id+": [automatic]")
	if !strings.Contains(msg, "#2 (fix the build)") || !strings.Contains(msg, "resumes each with its task") {
		t.Fatalf("parent told: %q", msg)
	}
	waitRecord(t, sp2, "agent "+id+": continue 2")
	waitFor(t, func() bool {
		recs, _ := (&Service{dir: dir}).loadRegistry()
		for _, r := range recs {
			if r.ID == id && len(r.Agents) > 0 {
				return false
			}
		}
		return true
	})
}

// News an engine held while the upgrade was pending (a finished job, a
// peer's wake) wakes the session after the restart; a peer's dun_msg to an
// idle session says "queued".
func TestUpgradeWakesHeldNewsAfterTheRestart(t *testing.T) {
	fastUpgrade(t)
	svc, sp, dir := newSvc(t)
	busy := working(t, svc, false)
	idle, _ := svc.Create(alice, t.TempDir(), nil, "")
	_ = svc.Send(alice, idle, proto.NewCommand("", &proto.User{Content: "hi"}))
	waitRecord(t, sp, "user "+idle+": hi")
	_ = svc.RequestUpgrade("/x/dun", "v2")
	waitRecord(t, sp, "hold "+idle+": true")
	waitRecord(t, sp, "hold "+busy+": true")
	d, err := svc.PeerSend(alice, busy, idle, "the deploy is done", true)
	if err != nil || d.Started || !d.Queued {
		t.Fatalf("peer: %+v, %v", d, err)
	}
	_ = svc.Send(alice, idle, proto.NewCommand("", &proto.User{Content: "news"})) // its engine held a wake
	is, _ := svc.Get(alice, idle)
	waitFor(t, func() bool { is.actMu.Lock(); defer is.actMu.Unlock(); return is.heldWake })
	_ = svc.Send(alice, busy, proto.NewCommand("", &proto.User{Content: "finish"}))
	sp2 := &fakeSpawner{}
	restartInto(t, svc, dir, sp2)
	got := waitRecord(t, sp2, "user "+idle+": [automatic] ")
	if !strings.Contains(got, "News that came while dun waited") {
		t.Fatalf("woken with %q", got)
	}
}

// Each change of the hold is numbered: a failure's Off is newer than the
// On it ends, so an engine that gets them out of order keeps the Off.
func TestUpgradeHoldGenerations(t *testing.T) {
	fastUpgrade(t)
	svc, sp, _ := newSvc(t)
	busy := working(t, svc, false)
	svc.SetUpgradeInstall(func(string, string) error { return errors.New("no room on disk") })
	_ = svc.RequestUpgrade("/x/dun", "v2")
	waitRecord(t, sp, "hold "+busy+": true gen 1")
	late, _ := svc.Create(alice, t.TempDir(), nil, "") // started while pending
	waitRecord(t, sp, "hold "+late+": true gen 1")
	_ = svc.Send(alice, busy, proto.NewCommand("", &proto.User{Content: "finish"}))
	waitRecord(t, sp, "hold "+busy+": false gen 2")
	waitRecord(t, sp, "hold "+late+": false gen 2")
	_ = svc.RequestUpgrade("/x/dun", "v3")
	waitRecord(t, sp, "hold "+busy+": true gen 3")
}

// A held message that cannot be delivered after the restart (its engine
// does not start) stays in the registry for the next try.
func TestUpgradeKeepsUndeliveredQueuedMessages(t *testing.T) {
	fastUpgrade(t)
	svc, _, dir := newSvc(t)
	busy := working(t, svc, false)
	idle, _ := svc.Create(alice, t.TempDir(), nil, "")
	_ = svc.RequestUpgrade("/x/dun", "v2")
	is, _ := svc.Get(alice, idle)
	for _, m := range []string{"one", "two"} {
		if held, err := svc.queueIfUpgrading(is, proto.NewCommand("", &proto.User{Content: m, Auto: true})); !held || err != nil {
			t.Fatalf("held %v, %v", held, err)
		}
	}
	_ = svc.Send(alice, busy, proto.NewCommand("", &proto.User{Content: "finish"}))
	sp2 := &fakeSpawner{fail: errors.New("no engine")}
	restartInto(t, svc, dir, sp2)
	time.Sleep(50 * time.Millisecond) // resumeFlagged ran, and failed
	recs, _ := (&Service{dir: dir}).loadRegistry()
	for _, r := range recs {
		if r.ID == idle && !slices.Equal(r.Queued, []string{"one", "two"}) {
			t.Fatalf("queued after a failed delivery: %q", r.Queued)
		}
	}
}

// Once a stop has begun, a peer's message is refused (its record is
// written), and news an engine reports after the record is recorded
// itself, in the registry.
func TestStopRefusesPeerSendsAndRecordsLateHeldNews(t *testing.T) {
	svc, sp, dir := newSvc(t)
	a, _ := svc.Create(alice, t.TempDir(), nil, "")
	b, _ := svc.Create(alice, t.TempDir(), nil, "")
	_ = svc.Send(alice, b, proto.NewCommand("", &proto.User{Content: "hi"}))
	waitRecord(t, sp, "user "+b+": hi")
	if err := svc.PrepareStop("dund was stopped (terminated)"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PeerSend(alice, a, b, "ping", true); !errors.Is(err, errRestarting) {
		t.Fatalf("peer send while stopping: %v", err)
	}
	if _, err := svc.MsgSend(alice, proto.MsgSend{To: b, Text: "ping"}); !errors.Is(err, errRestarting) {
		t.Fatalf("dun msg while stopping: %v", err)
	}
	_ = svc.Send(alice, b, proto.NewCommand("", &proto.User{Content: "news"})) // a wake_held after the record
	waitFor(t, func() bool {
		recs, _ := (&Service{dir: dir}).loadRegistry()
		for _, r := range recs {
			if r.ID == b && strings.Contains(r.Resume, "News that came while dun waited") {
				return true
			}
		}
		return false
	})
}

// dun msg is a person at a shell: its wake is marked human, which an
// engine's upgrade hold lets through.
func TestDunMsgIsHuman(t *testing.T) {
	fastUpgrade(t)
	svc, sp, _ := newSvc(t)
	busy := working(t, svc, false)
	b, _ := svc.Create(alice, t.TempDir(), nil, "")
	_ = svc.RequestUpgrade("/x/dun", "v2")
	waitRecord(t, sp, "hold "+b+": true")
	sent, err := svc.MsgSend(alice, proto.MsgSend{To: b, Text: "look"})
	if err != nil || sent.Detail != "idle: woken" {
		t.Fatalf("dun msg: %+v, %v", sent, err)
	}
	waitRecord(t, sp, "human peer "+b+": look")
	_ = svc.Send(alice, busy, proto.NewCommand("", &proto.User{Content: "finish"}))
	select { // the upgrade's loop must not outlive the test
	case <-svc.UpgradeReady():
	case <-time.After(5 * time.Second):
		t.Fatal("never ready")
	}
}

// The continued mark ends with any turn end the person caused or saw: a
// turn that ended, failed, or that they interrupted or stopped. dund's own
// stop interrupt keeps it.
func TestContinuedClearsOnAPersonsTurnEnd(t *testing.T) {
	svc, _, _ := newSvc(t)
	id, _ := svc.Create(alice, t.TempDir(), nil, "")
	ses, _ := svc.Get(alice, id)
	set := func() { ses.actMu.Lock(); ses.continued = true; ses.actMu.Unlock() }
	set()
	ses.track(proto.NewEvent(&proto.Done{Reason: "interrupted"}))
	if !ses.isContinued() {
		t.Fatal("dund's interrupt cleared it")
	}
	ses.track(proto.NewEvent(&proto.Error{Error: "provider down"}))
	if ses.isContinued() {
		t.Fatal("an error kept it")
	}
	set()
	_ = svc.Send(alice, id, proto.NewCommand("", &proto.Interrupt{}))
	if ses.isContinued() {
		t.Fatal("the person's interrupt kept it")
	}
	set()
	_ = svc.Stop(alice, id)
	if ses.isContinued() {
		t.Fatal("the person's stop kept it")
	}
	set()
	ses.track(proto.NewEvent(&proto.Done{}))
	if ses.isContinued() {
		t.Fatal("a turn that ended kept it")
	}
}
