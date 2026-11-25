package dund

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// An engine that keeps its question (harness replay.go): dund's stop does
// not interrupt it (an interrupt is the person cancelling it), saves no
// question, and the next dund starts its engine, which asks it again. No
// "[automatic]" message is sent: the answer continues the turn.
func TestUpgradeKeptQuestionStartsEngine(t *testing.T) {
	old := questionPending
	pending := true
	questionPending = func(string, string) bool { return pending }
	t.Cleanup(func() { questionPending = old })
	fastUpgrade(t)
	svc, sp, dir := newSvc(t)
	id := asking(t, svc, false)
	_ = svc.RequestUpgrade("/x/dun", "v2")
	select {
	case <-svc.UpgradeReady():
	case <-time.After(5 * time.Second):
		t.Fatal("an open question held the upgrade")
	}
	for _, r := range sp.received() {
		if strings.HasPrefix(r, "interrupt "+id) {
			t.Fatalf("the kept question was interrupted: %q", r)
		}
	}
	svc.Close()
	recs, _ := (&Service{dir: dir}).loadRegistry()
	for _, r := range recs {
		if r.ID == id && (r.Ask != nil || r.Resume != "") {
			t.Fatalf("saved for dund to show: %+v", r)
		}
	}

	sp2 := &fakeSpawner{}
	svc2, err := New(dir, sp2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc2.Close)
	waitFor(t, func() bool {
		return slices.ContainsFunc(sp2.spawnArgs(), func(a []string) bool { return slices.Contains(a, id) })
	})
	time.Sleep(50 * time.Millisecond)
	for _, r := range sp2.received() {
		if strings.HasPrefix(r, "user "+id) {
			t.Fatalf("woken with a message: %q", r)
		}
	}
	ses, _ := svc2.Get(alice, id)
	if ses.restoredAsk() != nil {
		t.Fatal("dund shows a question the engine asks")
	}
}
