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

// A restart message never calls a cut sub-agent stopped, persisted or not:
// dund resumes each one with its task (USER 2026-11-17), and records which.
func TestRestartResumesCutAgents(t *testing.T) {
	svc, _, _ := newSvc(t)
	id, _ := svc.Create(alice, t.TempDir(), nil, "")
	ses, _ := svc.Get(alice, id)
	for _, persist := range []string{"off", "on"} {
		_, _, _ = svc.SetPersist(alice, id, persist)
		ses.track(proto.NewEvent(&proto.Agents{Agents: []proto.AgentInfo{{ID: 2, State: "running", Prompt: "fix the build"}}}))
		ses.recordStop("dund restarted")
		n := ses.resumeNote()
		if !strings.Contains(n, "#2 (fix the build)") || !strings.Contains(n, "resumes each with its task") || strings.Contains(n, "stopped") {
			t.Errorf("persist %s: %q", persist, n)
		}
		if !slices.Equal(ses.cutAgents, []int{2}) {
			t.Errorf("persist %s: cut %v", persist, ses.cutAgents)
		}
	}
}
