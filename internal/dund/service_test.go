package dund

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/iodesystems/dun/proto"
)

const alice, bob = "unix:1000", "unix:1001"

func init() {
	restartDelay = 10 * time.Millisecond
	stopGrace = time.Second
}

func newSvc(t *testing.T) (*Service, *fakeSpawner, string) {
	t.Helper()
	dir := t.TempDir()
	sp := &fakeSpawner{}
	svc, err := New(dir, sp)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)
	return svc, sp, dir
}

// next waits for the next event of type typ on sub, failing after a while.
func next(t *testing.T, sub *proto.Subscription, typ string) proto.Event {
	t.Helper()
	timeout := time.After(3 * time.Second)
	for {
		select {
		case ev, ok := <-sub.C():
			if !ok {
				t.Fatalf("subscription closed waiting for %s: %v", typ, sub.Err())
			}
			if ev.Type == typ {
				return ev
			}
		case <-timeout:
			t.Fatalf("no %s event", typ)
		}
	}
}

func types(evs []proto.Event) []string {
	var out []string
	for _, e := range evs {
		out = append(out, e.Type)
	}
	return out
}

func TestCreateRegistersTheEnginesSession(t *testing.T) {
	svc, _, _ := newSvc(t)
	id, err := svc.Create(alice, t.TempDir(), []string{"--model", "m"}, "")
	if err != nil {
		t.Fatal(err)
	}
	ls := svc.List(alice)
	if len(ls) != 1 || ls[0].ID != id || ls[0].Owner != alice || !ls[0].Running || !slices.Equal(ls[0].Members, []string{alice}) {
		t.Fatalf("list: %+v", ls)
	}
	ses, _ := svc.Get(alice, id)
	replay, sub, err := ses.log.Subscribe(0, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	// What the engine said before naming the session is kept, in order.
	if got := types(replay); len(got) < 2 || got[0] != "session" {
		t.Fatalf("replay: %v", got)
	}
	if replay[0].Seq != 1 || replay[0].Session != id {
		t.Fatalf("first event: %+v", replay[0])
	}
}

func TestCreateRejectsFlagsOffTheAllowlist(t *testing.T) {
	svc, sp, _ := newSvc(t)
	for _, args := range [][]string{{"--key", "sk"}, {"--resume", "x"}, {"-model", "m"}, {"--model"}} {
		if _, err := svc.Create(alice, t.TempDir(), args, ""); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
	if _, err := svc.Create(alice, "relative/dir", nil, ""); err == nil {
		t.Error("relative workspace accepted")
	}
	if n := len(sp.spawnArgs()); n != 0 {
		t.Errorf("%d engines spawned for rejected creates", n)
	}
}

// A session is only for its members: to anyone else it does not exist.
func TestOnlyMembersSeeASession(t *testing.T) {
	svc, _, _ := newSvc(t)
	id, err := svc.Create(alice, t.TempDir(), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Get(bob, id); err != ErrNoSession {
		t.Errorf("bob: %v", err)
	}
	if len(svc.List(bob)) != 0 {
		t.Error("bob lists alice's session")
	}
	if _, err := svc.UI(bob, id); err != ErrNoSession {
		t.Errorf("bob ui: %v", err)
	}
}

func TestSendReachesTheEngineAndEventsComeBackNumbered(t *testing.T) {
	svc, _, _ := newSvc(t)
	id, _ := svc.Create(alice, t.TempDir(), nil, "")
	ses, _ := svc.Get(alice, id)
	_, sub, _ := ses.log.Subscribe(ses.log.Last(), 100)
	defer sub.Close()
	if err := ses.send(proto.NewCommand("c1", &proto.User{Content: "hi"})); err != nil {
		t.Fatal(err)
	}
	u := next(t, sub, "user")
	m := next(t, sub, "message")
	if u.Payload.(*proto.UserEcho).Content != "hi" || m.Payload.(*proto.Message).Content != "re: hi" || m.Seq <= u.Seq {
		t.Fatalf("user %+v message %+v", u, m)
	}
}

// An engine that dies is restarted on the same conversation; a crash loop is
// capped, and a person's next command is the retry.
func TestCrashedEngineRestartsThenGivesUp(t *testing.T) {
	svc, sp, _ := newSvc(t)
	id, _ := svc.Create(alice, t.TempDir(), []string{"--model", "m"}, "")
	ses, _ := svc.Get(alice, id)
	_, sub, _ := ses.log.Subscribe(ses.log.Last(), 1000)
	defer sub.Close()
	for i := 1; i <= restartMax; i++ {
		_ = ses.send(proto.NewCommand("", &proto.User{Content: "crash"}))
		if st := next(t, sub, "engine").Payload.(*proto.EngineState); st.State != "restarting" || st.Restarts != i {
			t.Fatalf("crash %d: %+v", i, st)
		}
		next(t, sub, "ready") // restarted
	}
	_ = ses.send(proto.NewCommand("", &proto.User{Content: "crash"}))
	if st := next(t, sub, "engine").Payload.(*proto.EngineState); st.State != "gave_up" {
		t.Fatalf("after the cap: %+v", st)
	}
	args := sp.spawnArgs()
	last := args[len(args)-1]
	if !slices.Equal(last, []string{"--model", "m", "--resume", id}) {
		t.Errorf("restart args: %v", last)
	}
	// The next command starts it again.
	if err := ses.send(proto.NewCommand("", &proto.User{Content: "back?"})); err != nil {
		t.Fatal(err)
	}
	next(t, sub, "message")
}

func TestStopDoesNotRestartAndASendStartsAgain(t *testing.T) {
	svc, sp, _ := newSvc(t)
	id, _ := svc.Create(alice, t.TempDir(), nil, "")
	ses, _ := svc.Get(alice, id)
	_, sub, _ := ses.log.Subscribe(ses.log.Last(), 1000)
	defer sub.Close()
	if err := svc.Stop(alice, id); err != nil {
		t.Fatal(err)
	}
	if st := next(t, sub, "engine").Payload.(*proto.EngineState); st.State != "stopped" {
		t.Fatalf("stop: %+v", st)
	}
	time.Sleep(5 * restartDelay)
	if n := len(sp.spawnArgs()); n != 1 || svc.List(alice)[0].Running {
		t.Fatalf("restarted after a stop: %d spawns", n)
	}
	_ = ses.send(proto.NewCommand("", &proto.User{Content: "again"}))
	next(t, sub, "message")
}

// Sessions outlive the service: a new one loads the registry and starts the
// engine on first use, reattached to the conversation.
func TestRegistrySurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	sp := &fakeSpawner{}
	svc, _ := New(dir, sp)
	ws := t.TempDir()
	id, err := svc.Create(alice, ws, []string{"--rephrase"}, "")
	if err != nil {
		t.Fatal(err)
	}
	svc.Close()

	sp2 := &fakeSpawner{}
	svc2, err := New(dir, sp2)
	if err != nil {
		t.Fatal(err)
	}
	defer svc2.Close()
	ls := svc2.List(alice)
	if len(ls) != 1 || ls[0].ID != id || ls[0].Workspace != ws || ls[0].Running {
		t.Fatalf("reloaded: %+v", ls)
	}
	ses, _ := svc2.Get(alice, id)
	_, sub, _ := ses.log.Subscribe(0, 100)
	defer sub.Close()
	_ = ses.send(proto.NewCommand("", &proto.User{Content: "still here"}))
	next(t, sub, "message")
	if a := sp2.spawnArgs(); len(a) != 1 || !slices.Equal(a[0], []string{"--rephrase", "--resume", id}) {
		t.Fatalf("lazy start args: %v", a)
	}
}

// A client too far behind — here, one that knows a seq from before the
// service restarted — gets a snapshot, then the live stream.
func TestSubscribeFromTheFutureGetsASnapshot(t *testing.T) {
	svc, _, _ := newSvc(t)
	id, _ := svc.Create(alice, t.TempDir(), nil, "")
	ses, _ := svc.Get(alice, id)
	snap, _, sub, err := ses.subscribe(10_000)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	if snap == nil {
		t.Fatal("no snapshot")
	}
	sp := snap.Payload.(*proto.Snapshot)
	if sp.After == 0 || sp.After > ses.log.Last() || len(sp.State) == 0 || snap.Session != id {
		t.Fatalf("snapshot: %+v", sp)
	}
	_ = ses.send(proto.NewCommand("", &proto.User{Content: "x"}))
	if m := next(t, sub, "message"); m.Seq <= sp.After {
		t.Fatalf("live event %d not after the snapshot's %d", m.Seq, sp.After)
	}
}

func TestUIStateIsPerMember(t *testing.T) {
	svc, _, dir := newSvc(t)
	id, _ := svc.Create(alice, t.TempDir(), nil, "")
	if st, err := svc.UI(alice, id); err != nil || len(st) != 0 {
		t.Fatalf("initial: %v %v", st, err)
	}
	if err := svc.PutUI(alice, id, map[string]any{"draft": "half a thought"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.PutUI(bob, id, map[string]any{"draft": "x"}); err != ErrNoSession {
		t.Fatalf("bob put: %v", err)
	}
	svc2, _ := New(dir, &fakeSpawner{})
	defer svc2.Close()
	st, err := svc2.UI(alice, id)
	if err != nil || st["draft"] != "half a thought" {
		t.Fatalf("after reload: %v %v", st, err)
	}
}

// An existing conversation is adopted under its own id; asking again gets
// the same session, and someone else cannot take it.
func TestCreateAdoptsAConversation(t *testing.T) {
	svc, sp, _ := newSvc(t)
	ws := t.TempDir()
	id, err := svc.Create(alice, ws, nil, "20260101-000000")
	if err != nil || id != "20260101-000000" {
		t.Fatalf("adopt: %q %v", id, err)
	}
	if a := sp.spawnArgs(); !slices.Equal(a[0], []string{"--resume", id}) {
		t.Fatalf("spawn: %v", a)
	}
	if again, err := svc.Create(alice, ws, nil, id); err != nil || again != id || len(sp.spawnArgs()) != 1 {
		t.Fatalf("again: %q %v, %d spawns", again, err, len(sp.spawnArgs()))
	}
	if _, err := svc.Create(bob, ws, nil, id); err != ErrNoSession {
		t.Fatalf("bob adopting alice's: %v", err)
	}
}

func TestRestartSwapsFlagsOnTheSameConversation(t *testing.T) {
	svc, sp, dir := newSvc(t)
	id, _ := svc.Create(alice, t.TempDir(), []string{"--model", "a"}, "")
	ses, _ := svc.Get(alice, id)
	_, sub, _ := ses.log.Subscribe(ses.log.Last(), 1000)
	defer sub.Close()
	if err := svc.Restart(alice, id, []string{"--model", "b"}); err != nil {
		t.Fatal(err)
	}
	next(t, sub, "ready")
	a := sp.spawnArgs()
	if !slices.Equal(a[len(a)-1], []string{"--model", "b", "--resume", id}) {
		t.Fatalf("restart args: %v", a[len(a)-1])
	}
	if err := svc.Restart(alice, id, []string{"--key", "x"}); err == nil {
		t.Fatal("--key accepted")
	}
	svc2, _ := New(dir, &fakeSpawner{})
	defer svc2.Close()
	if s2, _ := svc2.Get(alice, id); !slices.Equal(s2.args, []string{"--model", "b"}) {
		t.Fatalf("persisted args: %v", s2.args)
	}
}

func TestForgetRemovesTheSession(t *testing.T) {
	svc, _, dir := newSvc(t)
	id, _ := svc.Create(alice, t.TempDir(), nil, "")
	_ = svc.PutUI(alice, id, map[string]any{"draft": "x"})
	if err := svc.Forget(bob, id); err != ErrNoSession {
		t.Fatalf("bob forgetting: %v", err)
	}
	if err := svc.Forget(alice, id); err != nil {
		t.Fatal(err)
	}
	if len(svc.List(alice)) != 0 {
		t.Fatal("still listed")
	}
	svc2, _ := New(dir, &fakeSpawner{})
	defer svc2.Close()
	if len(svc2.List(alice)) != 0 {
		t.Fatal("still in the registry")
	}
	if _, err := os.Stat(filepath.Join(dir, "ui", id)); !os.IsNotExist(err) {
		t.Fatalf("ui state left: %v", err)
	}
}

// An engine nobody follows and nothing occupies is stopped; one in a turn,
// asking, or followed is not.
func TestIdleEnginesStop(t *testing.T) {
	svc, _, _ := newSvc(t)
	id, _ := svc.Create(alice, t.TempDir(), nil, "")
	ses, _ := svc.Get(alice, id)
	time.Sleep(20 * time.Millisecond) // the fake's startup events land
	d := 10 * time.Millisecond
	time.Sleep(2 * d)
	if !ses.idle(d) {
		t.Fatal("an unfollowed, unoccupied engine should be idle")
	}
	_, sub, _ := ses.log.Subscribe(ses.log.Last(), 100)
	if ses.idle(d) {
		t.Error("followed, it is not idle")
	}
	sub.Close()
	for _, busy := range []any{&proto.Turn{Kind: "user"}, &proto.Ask{Question: "?"},
		&proto.Jobs{Jobs: []proto.JobInfo{{State: "running"}}}, &proto.Agents{Agents: []proto.AgentInfo{{State: "running"}}}} {
		ses.track(proto.NewEvent(busy))
		time.Sleep(2 * d)
		if ses.idle(d) {
			t.Errorf("idle during %T", busy)
		}
		ses.track(proto.NewEvent(&proto.EngineState{State: "stopped"})) // clear
		ses.mu.Lock()
		ses.running = true
		ses.mu.Unlock()
	}
	ses.track(proto.NewEvent(&proto.Turn{Kind: "user"}))
	ses.track(proto.NewEvent(&proto.Done{}))
	time.Sleep(2 * d)
	if !ses.idle(d) {
		t.Error("after done it is idle again")
	}
}

// A rebuild restarts dund only when nothing is busy.
func TestBusyHoldsBackARestart(t *testing.T) {
	svc, _, _ := newSvc(t)
	id, _ := svc.Create(alice, t.TempDir(), nil, "")
	ses, _ := svc.Get(alice, id)
	if svc.Busy() {
		t.Fatal("busy with nothing running")
	}
	ses.track(proto.NewEvent(&proto.Jobs{Jobs: []proto.JobInfo{{State: "running"}}}))
	if !svc.Busy() {
		t.Fatal("a running job is busy")
	}
	ses.track(proto.NewEvent(&proto.Jobs{Jobs: []proto.JobInfo{{State: "ok"}}}))
	if svc.Busy() {
		t.Fatal("a finished job is not")
	}
}

// A model switch replaces --model and --url and keeps every other flag.
func TestSwitchModelKeepsOtherFlags(t *testing.T) {
	svc, sp, _ := newSvc(t)
	id, _ := svc.Create(alice, t.TempDir(), []string{"--worktree", "--model", "a", "--url=http://old"}, "")
	ses, _ := svc.Get(alice, id)
	_, sub, _ := ses.log.Subscribe(ses.log.Last(), 1000)
	defer sub.Close()
	if err := svc.SwitchModel(alice, id, "http://new", "b"); err != nil {
		t.Fatal(err)
	}
	next(t, sub, "ready")
	a := sp.spawnArgs()
	if got := a[len(a)-1]; !slices.Equal(got, []string{"--worktree", "--model", "b", "--url", "http://new", "--resume", id}) {
		t.Fatalf("args: %v", got)
	}
	if err := svc.SwitchModel(bob, id, "http://x", "c"); err != ErrNoSession {
		t.Fatalf("bob: %v", err)
	}
}

func TestSetFlag(t *testing.T) {
	for _, tc := range []struct {
		in   []string
		want []string
	}{
		{nil, []string{"--model", "m"}},
		{[]string{"--model", "x", "--pr"}, []string{"--pr", "--model", "m"}},
		{[]string{"--model=x"}, []string{"--model", "m"}},
	} {
		if got := setFlag(tc.in, "--model", "m"); !slices.Equal(got, tc.want) {
			t.Errorf("%v: %v", tc.in, got)
		}
	}
	if got := setFlag([]string{"--model", "x", "--pr"}, "--model", ""); !slices.Equal(got, []string{"--pr"}) {
		t.Errorf("remove: %v", got)
	}
}

func TestSignInTokens(t *testing.T) {
	svc, _, _ := newSvc(t)
	tok, err := svc.MintSignIn(alice, "a@example.com", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := svc.CheckSignIn(tok); !ok || p != alice {
		t.Fatalf("check: %q %v", p, ok)
	}
	for _, bad := range []string{"", "dun_s_", tok + "x", "dun_" + tok[6:]} {
		if _, ok := svc.CheckSignIn(bad); ok {
			t.Errorf("%q accepted", bad)
		}
	}
	old, _ := svc.MintSignIn(alice, "a@example.com", -time.Second)
	if _, ok := svc.CheckSignIn(old); ok {
		t.Error("a lapsed token was accepted")
	}
	if err := svc.RevokeSignIn(tok); err != nil {
		t.Fatal(err)
	}
	if _, ok := svc.CheckSignIn(tok); ok {
		t.Error("a revoked token was accepted")
	}
}
