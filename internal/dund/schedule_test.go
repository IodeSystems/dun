package dund

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iodesystems/dun/proto"
)

// fakeClock is a settable now.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// newSched is a scheduler on a fake clock that records what it delivers.
func newSched(t *testing.T) (*scheduler, *fakeClock, *[]schedEntry) {
	t.Helper()
	clk := &fakeClock{t: time.Date(2026, 10, 27, 14, 0, 0, 0, time.Local)}
	sc, err := loadScheduler(filepath.Join(t.TempDir(), "schedule.json"), clk.now)
	if err != nil {
		t.Fatal(err)
	}
	var got []schedEntry
	sc.deliver = func(e schedEntry) error { got = append(got, e); return nil }
	return sc, clk, &got
}

func msg(owner, session string, at time.Time, text string) schedEntry {
	return schedEntry{Owner: owner, SchedEntry: proto.SchedEntry{Session: session, At: at, Text: text}}
}

func texts(es []schedEntry) string {
	var out []string
	for _, e := range es {
		out = append(out, e.Text)
	}
	return strings.Join(out, ",")
}

// Due entries fire earliest first, ties in the order they were made; a
// one-shot goes once; nothing fires early.
func TestSchedFiresInOrder(t *testing.T) {
	sc, clk, got := newSched(t)
	t0 := clk.now()
	sc.add(msg(alice, "s1", t0.Add(3*time.Minute), "c"))
	sc.add(msg(alice, "s1", t0.Add(time.Minute), "a"))
	sc.add(msg(alice, "s2", t0.Add(3*time.Minute), "d"))
	sc.add(msg(alice, "s1", t0.Add(2*time.Minute), "b"))
	if f := sc.fireDue(); len(f) != 0 {
		t.Fatalf("fired early: %v", texts(f))
	}
	if at, _ := sc.earliest(); !at.Equal(t0.Add(time.Minute)) {
		t.Errorf("earliest %v", at)
	}
	clk.add(5 * time.Minute)
	sc.fireDue()
	if g := texts(*got); g != "a,b,c,d" {
		t.Fatalf("fired %s; want a,b,c,d", g)
	}
	if sc.fireDue() != nil || len(sc.list(alice)) != 0 {
		t.Errorf("a one-shot fired twice or stayed: %v", sc.list(alice))
	}
}

// push (absolute, and +shift past the current time), edit and cancel, and
// only on one's own entries.
func TestSchedPushEditCancel(t *testing.T) {
	sc, clk, got := newSched(t)
	t0 := clk.now()
	a := sc.add(msg(alice, "s1", t0.Add(time.Minute), "first"))
	b := sc.add(msg(alice, "s1", t0.Add(2*time.Minute), "second"))
	if _, err := sc.push(alice, a.ID, time.Time{}, time.Hour); err != nil {
		t.Fatal(err)
	}
	if e, _ := sc.get(alice, a.ID); !e.At.Equal(t0.Add(61 * time.Minute)) {
		t.Errorf("+1h: at %v", e.At)
	}
	if _, err := sc.push(alice, a.ID, t0.Add(10*time.Minute), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := sc.edit(alice, b.ID, "second, edited"); err != nil {
		t.Fatal(err)
	}
	if _, err := sc.edit(bob, b.ID, "bob's"); !errors.Is(err, errNoEntry) {
		t.Errorf("bob edited alice's: %v", err)
	}
	if _, err := sc.cancel(bob, a.ID); !errors.Is(err, errNoEntry) {
		t.Errorf("bob cancelled alice's: %v", err)
	}
	clk.add(5 * time.Minute)
	sc.fireDue()
	if g := texts(*got); g != "second, edited" {
		t.Fatalf("at +5m fired %q", g)
	}
	if _, err := sc.cancel(alice, a.ID); err != nil {
		t.Fatal(err)
	}
	clk.add(time.Hour)
	if f := sc.fireDue(); len(f) != 0 {
		t.Errorf("a cancelled entry fired: %v", texts(f))
	}
}

// A fixed loop counts its runs and comes back every interval, measured from
// the run that went: a dund down for an hour does not fire a burst.
func TestSchedFixedLoop(t *testing.T) {
	sc, clk, got := newSched(t)
	e := msg(alice, "s1", clk.now(), "check the deploy")
	e.Every = 10 * time.Minute
	l := sc.add(e)
	sc.fireDue()
	clk.add(10 * time.Minute)
	sc.fireDue()
	clk.add(time.Hour)
	sc.fireDue()
	if len(*got) != 3 || (*got)[0].Iteration != 1 || (*got)[2].Iteration != 3 {
		t.Fatalf("runs %+v", *got)
	}
	if cur, _ := sc.get(alice, l.ID); !cur.At.Equal(clk.now().Add(10 * time.Minute)) {
		t.Errorf("next %v, want now+10m", cur.At)
	}
}

// A loop whose session is in a turn waits for it (schedRetry), not stacked
// onto it; a one-shot goes anyway — the engine queues it into the turn.
func TestSchedLoopWaitsForTheTurn(t *testing.T) {
	sc, clk, got := newSched(t)
	busy := true
	sc.busy = func(string) bool { return busy }
	e := msg(alice, "s1", clk.now(), "loop")
	e.Every = 5 * time.Minute
	sc.add(e)
	sc.add(msg(alice, "s1", clk.now(), "once"))
	sc.fireDue()
	if g := texts(*got); g != "once" {
		t.Fatalf("busy: fired %q", g)
	}
	busy = false
	clk.add(schedRetry)
	sc.fireDue()
	if g := texts(*got); g != "once,loop" || (*got)[1].Iteration != 1 {
		t.Fatalf("after the turn: %q %+v", g, *got)
	}
}

// A self-paced loop has no next time while its turn runs; loop_ctl gives it
// one (clamped), the turn's end without loop_ctl gives it the default, and
// stop ends it.
func TestSchedSelfPaced(t *testing.T) {
	sc, clk, got := newSched(t)
	e := msg(alice, "s1", clk.now(), "watch the queue")
	e.Every = proto.SelfPaced
	l := sc.add(e)
	sc.fireDue()
	if cur, _ := sc.get(alice, l.ID); !cur.Awaiting || !cur.At.IsZero() {
		t.Fatalf("after firing: %+v", cur)
	}
	if out := sc.turnEnded("s1"); len(out) != 1 || !out[0].At.Equal(clk.now().Add(selfPaceDefault)) {
		t.Fatalf("turn ended without loop_ctl: %+v", out)
	}
	clk.add(selfPaceDefault)
	sc.fireDue()
	if _, err := sc.loopCtl(alice, "s1", 10*time.Second, false); err != nil {
		t.Fatal(err)
	}
	if cur, _ := sc.get(alice, l.ID); !cur.At.Equal(clk.now().Add(loopMin)) || cur.Awaiting {
		t.Errorf("next_in 10s not clamped to %s: %+v", loopMin, cur)
	}
	if out := sc.turnEnded("s1"); len(out) != 0 {
		t.Errorf("default applied after loop_ctl: %+v", out)
	}
	if _, err := sc.loopCtl(alice, "s2", time.Hour, false); err == nil {
		t.Error("loop_ctl in a session with no loop")
	}
	if _, err := sc.loopCtl(alice, "s1", 0, true); err != nil || sc.loops("s1") != 0 {
		t.Errorf("stop: %v, %d loops left", err, sc.loops("s1"))
	}
	if len(*got) != 2 || (*got)[1].Iteration != 2 {
		t.Errorf("runs %+v", *got)
	}
}

// The entries survive a restart; a self-paced loop that was mid-turn gets
// its default, since the new dund will never see that turn end.
func TestSchedPersists(t *testing.T) {
	sc, clk, _ := newSched(t)
	a := sc.add(msg(alice, "s1", clk.now().Add(time.Hour), "later\nwith a second line"))
	e := msg(alice, "s1", clk.now(), "paced")
	e.Every = proto.SelfPaced
	sc.add(e)
	sc.fireDue()
	re, err := loadScheduler(sc.path, clk.now)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(re.list(alice)); n != 2 {
		t.Fatalf("reloaded %d entries", n)
	}
	if m, _ := re.get(alice, a.ID); m.Text != "later\nwith a second line" || !m.At.Equal(a.At) || m.Owner != alice {
		t.Errorf("message reloaded as %+v", m)
	}
	if p, _ := re.get(alice, a.ID+1); p.Awaiting || !p.At.Equal(clk.now().Add(selfPaceDefault)) || p.Iteration != 1 {
		t.Errorf("mid-turn loop reloaded as %+v", p)
	}
	if n := re.add(msg(alice, "s1", clk.now(), "x")); n.ID <= a.ID+1 {
		t.Errorf("an id reused: %d", n.ID)
	}
}

// A firing whose engine would not start comes back schedRetry later; one
// whose session is gone is dropped.
func TestSchedDeliveryFailure(t *testing.T) {
	sc, clk, _ := newSched(t)
	sc.deliver = func(e schedEntry) error {
		if e.Session == "gone" {
			return ErrNoSession
		}
		return errors.New("engine would not start")
	}
	sc.add(msg(alice, "gone", clk.now(), "a"))
	b := sc.add(msg(alice, "s1", clk.now(), "b"))
	sc.fireDue()
	es := sc.list(alice)
	if len(es) != 1 || es[0].ID != b.ID || !es[0].At.Equal(clk.now().Add(schedRetry)) {
		t.Fatalf("after failures: %+v", es)
	}
}

// --- through the Service, with the fake engine ---

// schedReq makes session from's engine ask dund for op, as /schedule does,
// and returns dund's reply.
func schedReq(t *testing.T, svc *Service, sp *fakeSpawner, from string, req proto.SchedReq) proto.PeerReply {
	t.Helper()
	before := len(sp.received())
	js, _ := json.Marshal(proto.PeerRequest{RID: "r", Op: "sched", Sched: &req})
	if err := svc.Send(alice, from, proto.NewCommand("", &proto.User{Content: "peer-request " + string(js)})); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, r := range sp.received()[before:] {
			if b, ok := strings.CutPrefix(r, "reply "+from+": "); ok {
				var rep proto.PeerReply
				_ = json.Unmarshal([]byte(b), &rep)
				return rep
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no reply among %q", sp.received())
	return proto.PeerReply{}
}

// A message scheduled into a session in a turn is delivered as a user
// message anyway — the engine queues it into the turn, as it does one typed
// — marked as scheduled; a loop into a busy session waits.
func TestSchedDeliversToABusySession(t *testing.T) {
	svc, sp, _ := newSvc(t)
	a, _ := svc.Create(alice, t.TempDir(), nil, "")
	ses, _ := svc.Get(alice, a)
	_, sub, _ := ses.log.Subscribe(ses.log.Last(), 100)
	defer sub.Close()
	_ = svc.Send(alice, a, proto.NewCommand("", &proto.User{Content: "slow"}))
	next(t, sub, "turn")
	rep := schedReq(t, svc, sp, a, proto.SchedReq{Action: "add", At: time.Now().Add(-time.Second), Text: "the deploy is done?"})
	if rep.Error != "" || rep.Sched.Entry.Session != a {
		t.Fatalf("add: %+v", rep)
	}
	svc.sched.fireDue()
	got := waitRecord(t, sp, "user "+a+": [scheduled ")
	if !strings.HasSuffix(got, "] the deploy is done?") || strings.Contains(got, "from session") {
		t.Errorf("delivered %q", got)
	}
	// The fake engine answers it as a turn of its own (the real one queues
	// it into the running turn): back into a long turn.
	next(t, sub, "done")
	_ = svc.Send(alice, a, proto.NewCommand("", &proto.User{Content: "slow"}))
	next(t, sub, "turn")
	rep = schedReq(t, svc, sp, a, proto.SchedReq{Action: "add", At: time.Now().Add(-time.Second), Every: proto.SelfPaced, Text: "poll"})
	if rep.Sched.Loops != 1 {
		t.Errorf("loops after a /loop: %+v", rep.Sched)
	}
	svc.sched.fireDue()
	time.Sleep(50 * time.Millisecond)
	for _, r := range sp.received() {
		if strings.Contains(r, "[loop ") {
			t.Fatalf("a loop fired into a busy session: %q", r)
		}
	}
	_ = svc.Send(alice, a, proto.NewCommand("", &proto.User{Content: "finish"}))
	next(t, sub, "done")
	if e, _ := svc.sched.get(alice, rep.Sched.Entry.ID); e.At.IsZero() || e.Iteration != 0 {
		t.Errorf("deferred loop: %+v", e)
	}
}

// A self-paced loop's firing tells the engine it has a loop first (so
// loop_ctl is listed for the turn), and the turn ending without loop_ctl
// sets the default and says so in the session.
func TestSchedLoopFiringAndDefault(t *testing.T) {
	svc, sp, _ := newSvc(t)
	a, _ := svc.Create(alice, t.TempDir(), nil, "")
	ses, _ := svc.Get(alice, a)
	_, sub, _ := ses.log.Subscribe(ses.log.Last(), 100)
	defer sub.Close()
	rep := schedReq(t, svc, sp, a, proto.SchedReq{Action: "add", At: time.Now(), Every: proto.SelfPaced, Text: "poll"})
	id := rep.Sched.Entry.ID
	svc.sched.fireDue()
	waitRecord(t, sp, "user "+a+": [loop ")
	rs := sp.received()
	if i, j := indexOf(rs, "loops "+a+": 1"), indexPrefix(rs, "user "+a+": [loop "); i < 0 || i > j {
		t.Errorf("loops not before the firing: %q", rs)
	}
	ev := next(t, sub, "notification")
	if n := ev.Payload.(*proto.Notification); n.Kind != "schedule" || !strings.Contains(n.Text, "loop_ctl was not called") {
		t.Errorf("notice %+v", n)
	}
	if e, _ := svc.sched.get(alice, id); e.Awaiting || e.At.IsZero() || e.Iteration != 1 {
		t.Errorf("after the default: %+v", e)
	}
}

func indexOf(ss []string, s string) int {
	for i, x := range ss {
		if x == s {
			return i
		}
	}
	return -1
}

func indexPrefix(ss []string, p string) int {
	for i, x := range ss {
		if strings.HasPrefix(x, p) {
			return i
		}
	}
	return -1
}

// A message to another session by name goes there, says where it came
// from, wakes a stopped engine, and keeps an empty session from being
// forgotten meanwhile.
func TestSchedToAnotherSession(t *testing.T) {
	svc, sp, _ := newSvc(t)
	a, _ := svc.Create(alice, t.TempDir(), nil, "")
	b, _ := svc.Create(alice, t.TempDir(), nil, "")
	name := svc.sessionNames(alice)[b]
	rep := schedReq(t, svc, sp, a, proto.SchedReq{Action: "add", At: time.Now().Add(-time.Second), To: name, Text: "rebase on main"})
	if rep.Error != "" || rep.Sched.Entry.Session != b || rep.Sched.Entry.Name != name {
		t.Fatalf("add: %+v", rep)
	}
	if err := svc.Stop(alice, b); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Get(alice, b); err != nil {
		t.Fatalf("a session with a message on its way was forgotten: %v", err)
	}
	svc.sched.fireDue()
	got := waitRecord(t, sp, "user "+b+": [scheduled ")
	if !strings.Contains(got, "from session ") || !strings.HasSuffix(got, "] rebase on main") {
		t.Errorf("delivered %q", got)
	}
	if rep := schedReq(t, svc, sp, a, proto.SchedReq{Action: "add", At: time.Now(), To: "nobody", Text: "x"}); !strings.Contains(rep.Error, "no session") {
		t.Errorf("unknown target: %+v", rep)
	}
}

func TestScheduledText(t *testing.T) {
	now := time.Date(2026, 10, 27, 15, 0, 0, 0, time.Local)
	e := schedEntry{SchedEntry: proto.SchedEntry{ID: 3, Session: "s1", By: "s1", Created: now.Add(-time.Hour), At: now, Text: "go"}}
	if got := scheduledText(e, now, true); got != "[scheduled 14:00 for 15:00] go" {
		t.Errorf("here: %q", got)
	}
	e.ByName = "dun:120000"
	if got := scheduledText(e, now.Add(42*time.Minute), false); got != "[scheduled 14:00 from session dun:120000, due 15:00, sent 15:42] go" {
		t.Errorf("late, from elsewhere: %q", got)
	}
	e.Every, e.Iteration = 10*time.Minute, 2
	if got := scheduledText(e, now, true); got != "[loop 3, run 2, every 10m: loop_ctl can stop it] go" {
		t.Errorf("loop: %q", got)
	}
	e.Every = proto.SelfPaced
	if got := scheduledText(e, now, true); got != "[loop 3, run 2, self-paced: loop_ctl sets the next run (else 20m) or stops it] go" {
		t.Errorf("self-paced: %q", got)
	}
}
