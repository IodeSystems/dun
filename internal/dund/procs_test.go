package dund

import (
	"os"
	"testing"

	"github.com/iodesystems/dun/proto"
)

// dun ps asks dund for each running session's trees and jobs: the live
// sub-agents' trees from the agents event (running and idle, not stopped),
// the running jobs' groups; another user's sessions are not in it.
func TestProcs_NamesTreesAndJobs(t *testing.T) {
	svc, _, _ := newSvc(t)
	ws := t.TempDir()
	id, _ := svc.Create(alice, ws, nil, "")
	ses, _ := svc.Get(alice, id)
	ses.track(proto.NewEvent(&proto.Agents{Agents: []proto.AgentInfo{
		{ID: 1, State: "running", Dir: "/wt/1"},
		{ID: 2, State: "idle", Dir: "/wt/2"},
		{ID: 3, State: "stopped", Dir: "/wt/3"},
		{ID: 4, State: "running"}, // in its parent's tree
	}}))
	ses.track(proto.NewEvent(&proto.Jobs{Jobs: []proto.JobInfo{
		{ID: 5, State: "running", Command: "go test ./...", PGID: 777},
		{ID: 6, State: "exited", PGID: 778},
	}}))

	l := svc.Procs(alice)
	if l.DundPID != os.Getpid() || len(l.Sessions) != 1 {
		t.Fatalf("procs: %+v", l)
	}
	s := l.Sessions[0]
	if s.ID != id || s.Workspace != ws {
		t.Errorf("session: %+v", s)
	}
	if len(s.Agents) != 2 || s.Agents[0] != (proto.ProcAgent{ID: 1, Dir: "/wt/1"}) || s.Agents[1].ID != 2 {
		t.Errorf("agents: %+v", s.Agents)
	}
	if len(s.Jobs) != 1 || s.Jobs[0] != (proto.ProcJob{ID: 5, Command: "go test ./...", PGID: 777}) {
		t.Errorf("jobs: %+v", s.Jobs)
	}
	if l := svc.Procs(bob); len(l.Sessions) != 0 {
		t.Errorf("bob sees alice's session: %+v", l)
	}
}
