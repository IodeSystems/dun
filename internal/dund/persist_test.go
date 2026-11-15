package dund

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/iodesystems/dun"
	"github.com/iodesystems/dun/proto"
)

// realAsked puts the real rule back for one test: it reads transcripts the
// test writes itself.
func realAsked(t *testing.T) {
	askedMu.Lock()
	fake := askedIn
	askedIn = dun.SessionAsked
	askedMu.Unlock()
	t.Cleanup(func() {
		askedMu.Lock()
		askedIn = fake
		askedMu.Unlock()
	})
}

// writeTranscript writes one of a session's transcript files, aged past the
// prune's ten minutes.
func writeTranscript(t *testing.T, ws, name, body string) {
	t.Helper()
	dir := dun.RootDir(ws)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	os.Chtimes(p, old, old)
}

// After /clear the transcript holds notices only, and the conversation is in
// "<id>.cleared.<time>.jsonl". dund keeps that session on stop and on load,
// as the engine's prune keeps its files; one with nothing asked anywhere
// still goes.
func TestClearedSessionIsKeptOnStopAndLoad(t *testing.T) {
	realAsked(t)
	svc, sp, dir := newSvc(t)
	notice := `{"Kind":"notification","Content":"cleared"}` + "\n"
	asked := `{"Kind":"user","Content":"fix it"}` + "\n"

	ws := t.TempDir()
	cleared, _ := svc.Create(alice, ws, nil, "")
	writeTranscript(t, ws, cleared+".jsonl", notice)
	writeTranscript(t, ws, cleared+".cleared.20261114T100000.jsonl", asked)
	recapWS := t.TempDir()
	recapped, _ := svc.Create(alice, recapWS, nil, "")
	writeTranscript(t, recapWS, recapped+".jsonl", notice)
	writeTranscript(t, recapWS, recapped+".recap1.jsonl", asked)
	emptyWS := t.TempDir()
	empty, _ := svc.Create(alice, emptyWS, nil, "")
	writeTranscript(t, emptyWS, empty+".jsonl", notice)

	for _, id := range []string{cleared, recapped, empty} {
		if err := svc.Stop(alice, id); err != nil {
			t.Fatal(err)
		}
	}
	ids := func(ls []proto.SessionSummary) map[string]bool {
		m := map[string]bool{}
		for _, s := range ls {
			m[s.ID] = true
		}
		return m
	}
	if got := ids(svc.List(alice)); !got[cleared] || !got[recapped] || got[empty] || len(got) != 2 {
		t.Fatalf("after stop: %v", got)
	}
	svc.Close()
	svc2, err := New(dir, sp)
	if err != nil {
		t.Fatal(err)
	}
	defer svc2.Close()
	if got := ids(svc2.List(alice)); !got[cleared] || !got[recapped] || len(got) != 2 {
		t.Fatalf("after reload: %v", got)
	}
	// And the engine's prune agrees: nothing of either is removed.
	if removed := dun.PruneSessions(ws, ws, ""); len(removed) != 0 {
		t.Fatalf("prune removed %v", removed)
	}
	if _, err := os.Stat(filepath.Join(dun.RootDir(ws), cleared+".cleared.20261114T100000.jsonl")); err != nil {
		t.Fatal(err)
	}
}

// The fake engine writes no transcript; the tests that are not about empty
// sessions treat every session as asked in.
var askedMu sync.Mutex
var asked = map[string]bool{}

func init() {
	askedIn = func(_, id string) bool {
		askedMu.Lock()
		defer askedMu.Unlock()
		v, ok := asked[id]
		return !ok || v
	}
}

// setAsked marks a session for this test; ids are timestamps, so the mark is
// cleared before the next test can reuse one.
func setAsked(t *testing.T, id string, v bool) {
	askedMu.Lock()
	asked[id] = v
	askedMu.Unlock()
	t.Cleanup(func() {
		askedMu.Lock()
		delete(asked, id)
		askedMu.Unlock()
	})
}

// A session nothing was asked in is not kept: stopping it forgets it.
func TestEmptySessionIsForgottenWhenStopped(t *testing.T) {
	svc, _, _ := newSvc(t)
	id, _ := svc.Create(alice, t.TempDir(), nil, "")
	setAsked(t, id, false)
	if err := svc.Stop(alice, id); err != nil {
		t.Fatal(err)
	}
	if len(svc.List(alice)) != 0 {
		t.Fatalf("an empty session was kept: %+v", svc.List(alice))
	}
}

// ...nor across a restart of the service.
func TestEmptySessionIsDroppedOnLoad(t *testing.T) {
	svc, sp, dir := newSvc(t)
	keep, _ := svc.Create(alice, t.TempDir(), nil, "")
	drop, _ := svc.Create(alice, t.TempDir(), nil, "")
	setAsked(t, drop, false)
	svc.Close()
	svc2, err := New(dir, sp)
	if err != nil {
		t.Fatal(err)
	}
	defer svc2.Close()
	if ls := svc2.List(alice); len(ls) != 1 || ls[0].ID != keep {
		t.Fatalf("after reload: %+v", ls)
	}
}

// /persist: bare turns it on when off, else reports; on/off/status as named.
func TestPersistModes(t *testing.T) {
	svc, _, _ := newSvc(t)
	id, _ := svc.Create(alice, t.TempDir(), nil, "")
	for _, c := range []struct {
		mode        string
		on, changed bool
	}{{"status", false, false}, {"", true, true}, {"", true, false}, {"off", false, true}, {"on", true, true}, {"status", true, false}} {
		on, changed, err := svc.SetPersist(alice, id, c.mode)
		if err != nil || on != c.on || changed != c.changed {
			t.Fatalf("mode %q: on=%v changed=%v err=%v, want on=%v changed=%v", c.mode, on, changed, err, c.on, c.changed)
		}
	}
	if _, _, err := svc.SetPersist(alice, id, "maybe"); err == nil {
		t.Error("a bad mode was accepted")
	}
	if !svc.List(alice)[0].Persist {
		t.Error("the listing does not say it persists")
	}
}

// /exit: the last client leaving stops the engine — unless it persists, or
// another client is still attached.
func TestLeaveStopsUnlessPersistOrAttached(t *testing.T) {
	svc, _, _ := newSvc(t)
	id, _ := svc.Create(alice, t.TempDir(), nil, "")
	running := func() bool { return svc.List(alice)[0].Running }

	// Another client attached: still running.
	ses, _ := svc.Get(alice, id)
	_, sub, _ := ses.log.Subscribe(ses.log.Last(), 1000)
	if err := svc.Leave(alice, id); err != nil || !running() {
		t.Fatalf("stopped under another client: %v %v", err, running())
	}
	sub.Close()

	// Persisting: still running.
	_, _, _ = svc.SetPersist(alice, id, "on")
	if err := svc.Leave(alice, id); err != nil || !running() {
		t.Fatalf("stopped although it persists: %v %v", err, running())
	}

	// Neither: stopped, and still listed (it has messages).
	_, _, _ = svc.SetPersist(alice, id, "off")
	if err := svc.Leave(alice, id); err != nil || running() {
		t.Fatalf("not stopped: %v %v", err, running())
	}
	_ = proto.Version
}

// An empty session nobody follows is stopped and forgotten after emptyStop;
// one with messages waits for the full idle stop.
func TestReap_EmptySessionGoesAfterEmptyStop(t *testing.T) {
	svc, _, _ := newSvc(t)
	empty, _ := svc.Create(alice, t.TempDir(), nil, "")
	asked, _ := svc.Create(alice, t.TempDir(), nil, "")
	if empty == asked {
		t.Skip("same-second ids")
	}
	setAsked(t, empty, false)
	for _, id := range []string{empty, asked} {
		ses, _ := svc.Get(alice, id)
		ses.actMu.Lock()
		ses.lastActive = time.Now().Add(-emptyStop - time.Minute)
		ses.actMu.Unlock()
	}
	svc.reapOnce()
	ls := svc.List(alice)
	if len(ls) != 1 || ls[0].ID != asked || !ls[0].Running {
		t.Fatalf("after the sweep: %+v", ls)
	}
}
