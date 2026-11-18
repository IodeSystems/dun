package dund

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/dun/proto"
)

func peersGot(sp *fakeSpawner, prefix string) int {
	n := 0
	for _, g := range sp.received() {
		if strings.HasPrefix(g, prefix) {
			n++
		}
	}
	return n
}

// A peer message sent again (an engine resends its open requests when a
// dund attaches; a crashed dund's replay repeats it) is delivered once,
// and every copy gets the first answer, across a dund restart too.
func TestPeerMsgOnceAcrossResendAndRestart(t *testing.T) {
	dir := t.TempDir()
	sp := &fakeSpawner{}
	svc, err := New(dir, sp)
	if err != nil {
		t.Fatal(err)
	}
	a1, _ := svc.Create(alice, t.TempDir(), nil, "")
	a2, _ := svc.Create(alice, t.TempDir(), nil, "")
	from, _ := svc.Get(alice, a1)
	req := proto.PeerRequest{Op: "msg", RID: "p4242.17.1", To: a2, Text: "ping", Wake: false}
	first := svc.peerDone.once(a1, req, func() proto.PeerReply { return svc.peerOp(from, nil, req) })
	again := svc.peerDone.once(a1, req, func() proto.PeerReply { return svc.peerOp(from, nil, req) })
	if first.Error != "" || again.ID != first.ID || again.Error != "" {
		t.Fatalf("first %+v, again %+v", first, again)
	}
	waitFor(t, func() bool { return peersGot(sp, "peer "+a2+":") > 0 })
	svc.Close()

	sp2 := &fakeSpawner{}
	svc2, err := New(dir, sp2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc2.Close)
	from2, _ := svc2.Get(alice, a1)
	rep := svc2.peerDone.once(a1, req, func() proto.PeerReply { return svc2.peerOp(from2, nil, req) })
	if rep.ID != first.ID || rep.Error != "" {
		t.Fatalf("after the restart: %+v", rep)
	}
	time.Sleep(50 * time.Millisecond)
	if n := peersGot(sp, "peer "+a2+":") + peersGot(sp2, "peer "+a2+":"); n != 1 {
		t.Fatalf("delivered %d times", n)
	}
	// An id of an older engine ("p3") is not remembered: it repeats across
	// engines. A read (list) is answered again freely.
	old := proto.PeerRequest{Op: "msg", RID: "p3", To: a2, Text: "x"}
	calls := 0
	for range 2 {
		svc2.peerDone.once(a1, old, func() proto.PeerReply { calls++; return proto.PeerReply{} })
		svc2.peerDone.once(a1, proto.PeerRequest{Op: "list", RID: "p1.2.3"}, func() proto.PeerReply { calls++; return proto.PeerReply{} })
	}
	if calls != 4 {
		t.Fatalf("calls = %d", calls)
	}
}

// A request a restart cut while it was being done is not done again: its
// copy is told it may have been done.
func TestPeerRequestCutByARestartIsNotDoneAgain(t *testing.T) {
	dir := t.TempDir()
	d := loadPeerDone(dir)
	d.entries = append(d.entries, &peerDoneEntry{Key: "s1\x00p1.2.3"}) // taken, not done
	d.saveLocked()
	d2 := loadPeerDone(dir)
	ran := false
	rep := d2.once("s1", proto.PeerRequest{Op: "msg", RID: "p1.2.3"}, func() proto.PeerReply { ran = true; return proto.PeerReply{} })
	if ran || !strings.Contains(rep.Error, "may or may not have been done") || rep.RID != "p1.2.3" {
		t.Fatalf("ran %v, rep %+v", ran, rep)
	}
}

// endEngine kills an engine only once its process is proven an engine on
// that dir: by its argv, and its start time where it can be read.
func TestVerifyEngine(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("/proc")
	}
	dir := engDir(t)
	// "; true": sh must not exec sleep in its place.
	cmd := exec.Command("sh", "-c", "sleep 30; true", "sh", "--engine", "--listen", dir)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	waitFor(t, func() bool { // until the child has exec'd sh
		argv, _ := procArgv(cmd.Process.Pid)
		return len(argv) > 0 && argv[0] == "sh"
	})
	now := time.Now().UTC().Format(time.RFC3339)
	pid := cmd.Process.Pid
	if got := verifyEngine(pid, dir, now); got != engineIs {
		t.Fatalf("an engine: %v", got)
	}
	if got := verifyEngine(pid, dir+"x", now); got != engineNot {
		t.Fatalf("another dir: %v", got)
	}
	if got := verifyEngine(pid, dir, time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)); got != engineNot {
		t.Fatalf("started long after its entry: %v", got)
	}
	if got := verifyEngine(os.Getpid(), dir, now); got != engineNot {
		t.Fatalf("this test: %v", got)
	}
	if pidAlive(pid) != true {
		t.Fatal("alive")
	}
}
