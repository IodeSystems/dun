package dund

import (
	"slices"
	"strings"
	"testing"

	"github.com/iodesystems/dun/proto"
)

// Persist carries the sub-agents: an engine started on a persisted session
// is told so (--persist), and resumes its live children; toggled off, the
// next start is not, and they come back stopped. A client cannot pass it.
func TestPersistIsPassedToEveryEngineStart(t *testing.T) {
	svc, sp, _ := newSvc(t)
	id, _ := svc.Create(alice, t.TempDir(), nil, "")
	ses, _ := svc.Get(alice, id)
	last := func() []string { a := sp.spawnArgs(); return a[len(a)-1] }

	_, _, _ = svc.SetPersist(alice, id, "on")
	ses.stop()
	if err := ses.ensureRunning(); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(last(), "--persist") {
		t.Fatalf("persisted session started without --persist: %v", last())
	}

	_, _, _ = svc.SetPersist(alice, id, "off")
	ses.stop()
	if err := ses.ensureRunning(); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(last(), "--persist") {
		t.Fatalf("persist off, still passed: %v", last())
	}
	if _, err := svc.Create(alice, t.TempDir(), []string{"--persist"}, ""); err == nil {
		t.Error("a client passed --persist")
	}
}

// A persisted session's restart message does not call its sub-agents
// stopped: the engine restarts them and says so itself.
func TestPersistedRestartDoesNotCallAgentsStopped(t *testing.T) {
	svc, _, _ := newSvc(t)
	id, _ := svc.Create(alice, t.TempDir(), nil, "")
	ses, _ := svc.Get(alice, id)
	ses.track(proto.NewEvent(&proto.Agents{Agents: []proto.AgentInfo{{ID: 2, State: "running"}}}))
	ses.prepareStop("dund restarted")
	if !strings.Contains(ses.resumeNote(), "sub-agents: #2") {
		t.Fatalf("not persisted: %q", ses.resumeNote())
	}
	_, _, _ = svc.SetPersist(alice, id, "on")
	ses.track(proto.NewEvent(&proto.Agents{Agents: []proto.AgentInfo{{ID: 2, State: "running"}}}))
	ses.prepareStop("dund restarted")
	if strings.Contains(ses.resumeNote(), "sub-agents") {
		t.Errorf("persisted: %q", ses.resumeNote())
	}
}
