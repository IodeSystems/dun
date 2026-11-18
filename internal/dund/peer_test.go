package dund

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/dun/proto"
)

func init() {
	sessionTask = func(_, _ string) string { return "" } // never the user's dun home
}

// waitRecord waits for an engine to have received something starting with
// prefix, and returns it.
func waitRecord(t *testing.T, sp *fakeSpawner, prefix string) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, r := range sp.received() {
			if strings.HasPrefix(r, prefix) {
				return r
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no %q among %q", prefix, sp.received())
	return ""
}

func peerIDs(ps []proto.PeerSession) []string {
	var out []string
	for _, p := range ps {
		out = append(out, p.ID)
	}
	return out
}

// dun_list: the owner's sessions, only the loaded ones, the asker marked.
func TestPeersListOnlyTheOwnersLoadedSessions(t *testing.T) {
	svc, _, _ := newSvc(t)
	a1, _ := svc.Create(alice, t.TempDir(), nil, "")
	a2, _ := svc.Create(alice, t.TempDir(), nil, "")
	a3, _ := svc.Create(alice, t.TempDir(), nil, "")
	b1, _ := svc.Create(bob, t.TempDir(), nil, "")
	// a3 is registered but its engine stopped: resumable, not live.
	if err := svc.Stop(alice, a3); err != nil {
		t.Fatal(err)
	}
	ps := svc.Peers(alice, a1)
	if got := strings.Join(peerIDs(ps), ","); got != a1+","+a2 {
		t.Fatalf("listed %s; want %s,%s (not %s: stopped, not %s: bob's)", got, a1, a2, a3, b1)
	}
	if !ps[0].You || ps[1].You {
		t.Errorf("you marker: %+v", ps)
	}
	if ps[0].State != "idle" || ps[0].Name == "" || ps[0].Workspace == "" {
		t.Errorf("row: %+v", ps[0])
	}
	if len(svc.Peers(bob, b1)) != 1 {
		t.Errorf("bob sees %v", peerIDs(svc.Peers(bob, b1)))
	}
}

func TestPeerRowShowsBusyAndTheLastMessage(t *testing.T) {
	svc, _, _ := newSvc(t)
	a1, _ := svc.Create(alice, t.TempDir(), nil, "")
	a2, _ := svc.Create(alice, t.TempDir(), nil, "")
	ses, _ := svc.Get(alice, a2)
	_, sub, _ := ses.log.Subscribe(ses.log.Last(), 100)
	defer sub.Close()
	_ = svc.Send(alice, a2, proto.NewCommand("", &proto.User{Content: "slow"}))
	next(t, sub, "turn")
	row := svc.Peers(alice, a1)[1]
	if row.State != "busy" || row.Task != "slow" {
		t.Fatalf("row: %+v", row)
	}
}

func TestNameSessionsDisambiguatesFolders(t *testing.T) {
	rows := []proto.PeerSession{
		{ID: "20261021-222836", Workspace: "/home/x/src/dun"},
		{ID: "20261022-101500", Workspace: "/home/x/old/dun"},
		{ID: "20261022-101501", Workspace: "/home/x/src/raglit"},
		{ID: "20261022-111111", Workspace: "/a/b/src/dun"},     // shares "src/dun" with the first
		{ID: "20261022-121212", Workspace: "/home/x/src/dun/"}, // the first's workspace again
		// One workspace, one time of day, two dates: only the id tells them apart.
		{ID: "20261023-090000", Workspace: "/w/p"},
		{ID: "20261024-090000", Workspace: "/w/p"},
	}
	nameSessions(rows)
	want := []string{"x/src/dun:222836", "old/dun:101500", "raglit:101501", "b/src/dun:111111",
		"x/src/dun:121212", "20261023-090000", "20261024-090000"}
	for i, r := range rows {
		if r.Name != want[i] {
			t.Errorf("%s in %s: %q, want %q", r.ID, r.Workspace, r.Name, want[i])
		}
	}
}

func TestNameSessionsPlainWhenUnique(t *testing.T) {
	rows := []proto.PeerSession{{ID: "20261021-222836", Workspace: "/x/dun"}, {ID: "20261021-222900-2", Workspace: "/x/dun"}}
	nameSessions(rows)
	if rows[0].Name != "dun:222836" || rows[1].Name != "dun:222900-2" {
		t.Fatalf("%+v", rows)
	}
}

func TestGitBranch(t *testing.T) {
	dir := t.TempDir()
	if gitBranch(dir) != "" {
		t.Fatal("branch without a repo")
	}
	_ = os.MkdirAll(filepath.Join(dir, "real", ".git"), 0o700)
	_ = os.WriteFile(filepath.Join(dir, "real", ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o600)
	if b := gitBranch(filepath.Join(dir, "real")); b != "main" {
		t.Fatalf("repo: %q", b)
	}
	wt := filepath.Join(dir, "wt")
	_ = os.MkdirAll(filepath.Join(dir, "gd"), 0o700)
	_ = os.WriteFile(filepath.Join(dir, "gd", "HEAD"), []byte("ref: refs/heads/feat/x\n"), 0o600)
	_ = os.MkdirAll(wt, 0o700)
	_ = os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: ../gd\n"), 0o600)
	if b := gitBranch(wt); b != "feat/x" {
		t.Fatalf("worktree: %q", b)
	}
}

func TestPeerSendDeliversToALoadedSession(t *testing.T) {
	svc, sp, _ := newSvc(t)
	a1, _ := svc.Create(alice, t.TempDir(), nil, "")
	a2, _ := svc.Create(alice, t.TempDir(), nil, "")
	d, err := svc.PeerSend(alice, a1, a2, "ping", true)
	if err != nil {
		t.Fatal(err)
	}
	if d.ID != a2 || d.Busy || !d.Started {
		t.Fatalf("idle, wake: %+v", d)
	}
	r := waitRecord(t, sp, "peer "+a2+":")
	if !strings.Contains(r, "("+a1+") wake=true: ping") {
		t.Fatalf("delivered %q", r)
	}
	// By name too, and wake=false starts nothing.
	name := svc.Peers(alice, a1)[1].Name
	d, err = svc.PeerSend(alice, a1, name, "later", false)
	if err != nil || d.Started {
		t.Fatalf("by name %q, no wake: %+v %v", name, d, err)
	}
}

func TestPeerSendToABusySessionDoesNotStartATurn(t *testing.T) {
	svc, _, _ := newSvc(t)
	a1, _ := svc.Create(alice, t.TempDir(), nil, "")
	a2, _ := svc.Create(alice, t.TempDir(), nil, "")
	ses, _ := svc.Get(alice, a2)
	_, sub, _ := ses.log.Subscribe(ses.log.Last(), 100)
	defer sub.Close()
	_ = svc.Send(alice, a2, proto.NewCommand("", &proto.User{Content: "slow"}))
	next(t, sub, "turn")
	d, err := svc.PeerSend(alice, a1, a2, "ping", true)
	if err != nil || !d.Busy || d.Started {
		t.Fatalf("busy: %+v %v", d, err)
	}
}

func TestPeerSendRefusals(t *testing.T) {
	svc, sp, _ := newSvc(t)
	a1, _ := svc.Create(alice, t.TempDir(), nil, "")
	a2, _ := svc.Create(alice, t.TempDir(), nil, "")
	b1, _ := svc.Create(bob, t.TempDir(), nil, "")
	a3, _ := svc.Create(alice, t.TempDir(), nil, "")
	_ = svc.Stop(alice, a3)
	n := len(sp.spawnArgs())
	for _, c := range []struct{ to, want string }{
		{a1, "this session"},
		{"", "needs `to`"},
		{"nope", "no live session"},
		{b1, "no live session"}, // bob's: invisible, not forbidden
		{a3, "not loaded"},
	} {
		_, err := svc.PeerSend(alice, a1, c.to, "hi", true)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("to %q: %v, want %q", c.to, err, c.want)
		}
	}
	if _, err := svc.PeerSend(alice, a1, a2, " ", true); err == nil {
		t.Error("empty text accepted")
	}
	if len(sp.spawnArgs()) != n {
		t.Error("a message started an engine")
	}
	if strings.Contains(strings.Join(sp.received(), "\n"), "peer "+a3) {
		t.Error("the stopped session got it")
	}
}

func TestPeerSendRateLimit(t *testing.T) {
	svc, _, _ := newSvc(t)
	a1, _ := svc.Create(alice, t.TempDir(), nil, "")
	a2, _ := svc.Create(alice, t.TempDir(), nil, "")
	for i := 0; i < peerRateMax; i++ {
		if _, err := svc.PeerSend(alice, a1, a2, "hi", false); err != nil {
			t.Fatalf("message %d: %v", i+1, err)
		}
	}
	if _, err := svc.PeerSend(alice, a1, a2, "hi", false); err == nil || !strings.Contains(err.Error(), "rate limit") {
		t.Fatalf("over the limit: %v", err)
	}
	// Per pair: the other way round is its own count.
	if _, err := svc.PeerSend(alice, a2, a1, "hi", false); err != nil {
		t.Fatal(err)
	}
	// And the window slides.
	if !svc.peerAllow(a1, a2, time.Now().Add(peerRateWindow+time.Second)) {
		t.Error("still limited after the window")
	}
}

// The whole path: an engine's peer_request, dund's peer to the target and
// peer_reply to the asker; nothing of it in the asker's log.
func TestPeerRequestIsAnsweredOverStdio(t *testing.T) {
	svc, sp, _ := newSvc(t)
	a1, _ := svc.Create(alice, t.TempDir(), nil, "")
	a2, _ := svc.Create(alice, t.TempDir(), nil, "")
	send := func(js string) {
		if err := svc.Send(alice, a1, proto.NewCommand("", &proto.User{Content: "peer-request " + js})); err != nil {
			t.Fatal(err)
		}
	}
	send(`{"rid":"r1","op":"list"}`)
	r := waitRecord(t, sp, "reply "+a1+": ")
	if !strings.Contains(r, `"rid":"r1"`) || !strings.Contains(r, `"id":"`+a2+`"`) || !strings.Contains(r, `"you":true`) {
		t.Fatalf("list reply %s", r)
	}
	send(`{"rid":"r2","op":"msg","to":"` + a2 + `","text":"hello","wake":true}`)
	waitRecord(t, sp, "peer "+a2+":")
	r = waitRecord(t, sp, `reply `+a1+`: {"rid":"r2"`)
	if strings.Contains(r, "error") || !strings.Contains(r, `"started":true`) {
		t.Fatalf("msg reply %s", r)
	}
	send(`{"rid":"r3","op":"msg","to":"` + a1 + `","text":"me"}`)
	if r := waitRecord(t, sp, `reply `+a1+`: {"rid":"r3"`); !strings.Contains(r, "this session") {
		t.Fatalf("self reply %s", r)
	}
	ses, _ := svc.Get(alice, a1)
	replay, sub, _ := ses.log.Subscribe(0, 1000)
	sub.Close()
	for _, ev := range replay {
		if ev.Type == "peer_request" {
			t.Fatal("peer_request logged")
		}
	}
}

// A session in a dun worktree is named for its source checkout, not the
// dun-worktree-XXXX directory, and shows the worktree beside it; one in
// place on the same repo differs by its time.
func TestPeerNameOfAWorktreeSession(t *testing.T) {
	svc, _, _ := newSvc(t)
	repo := filepath.Join(t.TempDir(), "myrepo")
	_ = os.MkdirAll(repo, 0o700)
	inPlace, _ := svc.Create(alice, repo, nil, "")
	inWt, _ := svc.Create(alice, repo, []string{"--worktree"}, "")
	wt := filepath.Join(t.TempDir(), "worktrees", "dun-worktree-abcd")
	ses, _ := svc.Get(alice, inWt)
	_, sub, _ := ses.log.Subscribe(ses.log.Last(), 100)
	defer sub.Close()
	_ = svc.Send(alice, inWt, proto.NewCommand("", &proto.User{Content: "moved " + wt + " dun/1761234567"}))
	next(t, sub, "workspace")
	ps := svc.Peers(alice, inPlace)
	if len(ps) != 2 {
		t.Fatalf("%+v", ps)
	}
	if ps[0].Name != "myrepo:"+timeOfID(inPlace) || ps[0].Worktree != "" {
		t.Errorf("in place: %+v", ps[0])
	}
	if ps[1].Name != "myrepo:"+timeOfID(inWt) || ps[1].Worktree != wt || ps[1].Branch != "dun/1761234567" {
		t.Errorf("worktree: %+v", ps[1])
	}
	// The in-place Workspace event a session without --worktree sends names the workspace: no worktree.
	_ = svc.Send(alice, inWt, proto.NewCommand("", &proto.User{Content: "moved " + repo + " main"}))
	next(t, sub, "workspace")
	if p := svc.Peers(alice, inPlace)[1]; p.Worktree != "" || p.Branch != "main" {
		t.Errorf("in-place worktree: %+v", p)
	}
}

// Each engine hears how many other live sessions its owner has, when one
// starts or stops; another owner's sessions do not count.

// A global server change made in one session reaches the owner's other
// loaded sessions, and nobody else's: the engine that wrote mcp.json asks
// with Op "servers", and dund forwards proto.ServersChanged.
func TestServersChangedReachesTheOwnersOtherSessions(t *testing.T) {
	svc, sp, _ := newSvc(t)
	a1, _ := svc.Create(alice, t.TempDir(), nil, "")
	a2, _ := svc.Create(alice, t.TempDir(), nil, "")
	b1, _ := svc.Create(bob, t.TempDir(), nil, "")
	for _, id := range []string{a1, a2} { // both engines up
		_ = svc.Send(alice, id, proto.NewCommand("", &proto.User{Content: "hello"}))
		waitRecord(t, sp, "user "+id+": hello")
	}
	js, _ := json.Marshal(proto.PeerRequest{RID: "r", Op: "servers"})
	if err := svc.Send(alice, a1, proto.NewCommand("", &proto.User{Content: "peer-request " + string(js)})); err != nil {
		t.Fatal(err)
	}
	waitRecord(t, sp, "servers "+a2)
	waitRecord(t, sp, "reply "+a1+": ")
	for _, r := range sp.received() {
		if r == "servers "+a1 || r == "servers "+b1 {
			t.Errorf("told the wrong session: %q", r)
		}
	}
}

// A pair's wakes: peerWakeMax an hour; past that the message is delivered
// with wake false, and the sender told why. The window slides.
func TestPeerSendWakeHourlyCap(t *testing.T) {
	svc, _, _ := newSvc(t)
	a1, _ := svc.Create(alice, t.TempDir(), nil, "")
	a2, _ := svc.Create(alice, t.TempDir(), nil, "")
	s1, _ := svc.Get(alice, a1)
	s2, _ := svc.Get(alice, a2)
	now := time.Now()
	for i := 0; i < peerWakeMax; i++ {
		if why := svc.peerWake(s1, s2, now); why != "" {
			t.Fatalf("wake %d refused: %s", i+1, why)
		}
	}
	if why := svc.peerWake(s1, s2, now); !strings.Contains(why, "the most it may") {
		t.Fatalf("wake past the cap: %q", why)
	}
	// Through PeerSend: delivered, not woken, and why.
	d, err := svc.PeerSend(alice, a1, a2, "ping", true)
	if err != nil || d.Started || !strings.Contains(d.Unwoken, "the most it may") {
		t.Fatalf("over the cap: %+v %v", d, err)
	}
	if why := svc.peerWake(s1, s2, now.Add(peerWakeWindow+time.Second)); why != "" {
		t.Errorf("still capped an hour later: %s", why)
	}
}

// A wakes B, B's reply to A does not wake A while no person wrote in either
// session; once a person writes, it does.
func TestPeerSendPingPongReplyIsQuiet(t *testing.T) {
	svc, sp, _ := newSvc(t)
	a, _ := svc.Create(alice, t.TempDir(), nil, "")
	b, _ := svc.Create(alice, t.TempDir(), nil, "")
	if d, err := svc.PeerSend(alice, a, b, "can you check X?", true); err != nil || !d.Started {
		t.Fatalf("A to B: %+v %v", d, err)
	}
	d, err := svc.PeerSend(alice, b, a, "X is fine", true)
	if err != nil || d.Started || !strings.Contains(d.Unwoken, "a reply") {
		t.Fatalf("B's reply woke A: %+v %v", d, err)
	}
	if r := waitRecord(t, sp, "peer "+a+":"); !strings.Contains(r, "wake=false: X is fine") {
		t.Fatalf("delivered %q", r)
	}
	// A person writes in B (a scheduled firing is not one): B's next
	// message to A wakes it again.
	if d, err := svc.PeerSend(alice, a, b, "and Y?", true); err != nil || !d.Started {
		t.Fatalf("A to B again: %+v %v", d, err)
	}
	sb, _ := svc.Get(alice, b)
	sb.track(proto.NewEvent(&proto.UserEcho{Content: "[scheduled 10:00 for 10:05] not a person"}))
	if d, _ := svc.PeerSend(alice, b, a, "Y is fine?", true); d.Started {
		t.Fatalf("a scheduled firing counted as a person: %+v", d)
	}
	if d, err := svc.PeerSend(alice, a, b, "and Z?", true); err != nil || !d.Started {
		t.Fatalf("A to B a third time: %+v %v", d, err)
	}
	sb.track(proto.NewEvent(&proto.UserEcho{Content: "tell A about Z"}))
	if d, err := svc.PeerSend(alice, b, a, "Z is fine", true); err != nil || !d.Started || d.Unwoken != "" {
		t.Fatalf("after a person's message: %+v %v", d, err)
	}
}
