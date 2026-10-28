package dund

import (
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/iodesystems/dun/proto"
)

// dund answers what a session's job's processes are doing, from the group
// the engine named in its jobs event; a job it does not know is said so.
func TestJobUsage_SamplesTheJobsGroup(t *testing.T) {
	svc, _, _ := newSvc(t)
	id, _ := svc.Create(alice, t.TempDir(), nil, "")
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); _ = cmd.Wait() })
	ses, _ := svc.Get(alice, id)
	ses.track(proto.NewEvent(&proto.Jobs{Jobs: []proto.JobInfo{{ID: 3, State: "running", PGID: cmd.Process.Pid}}}))

	u := svc.JobUsage(alice, id, 3)
	if u.Error != "" || len(u.Usage.Procs) != 1 || u.Usage.Procs[0].Name != "sleep" {
		t.Fatalf("job 3: %+v", u)
	}
	time.Sleep(200 * time.Millisecond)
	if u = svc.JobUsage(alice, id, 3); !u.Usage.Rated {
		t.Errorf("a second ask should have rates: %+v", u.Usage)
	}
	if u = svc.JobUsage(alice, id, 9); !strings.Contains(u.Error, "not running here") {
		t.Errorf("an unknown job: %+v", u)
	}
	if u = svc.JobUsage(bob, id, 3); u.Error == "" {
		t.Error("another user's session must not answer")
	}
}
