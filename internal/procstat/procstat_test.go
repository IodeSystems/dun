package procstat

import (
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// group starts sh -c script as the leader of its own process group.
func group(t *testing.T, script string) int {
	t.Helper()
	cmd := exec.Command("sh", "-c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
	time.Sleep(200 * time.Millisecond)
	return pid
}

// A group burning a core reads as busy; one asleep reads as idle, with no
// CPU and no IO. Every member is found, the leader and its children.
func TestSample_BusyAndIdle(t *testing.T) {
	var s Sampler
	busy := group(t, "yes > /dev/null & yes > /dev/null & wait")
	s.Sample(busy)
	time.Sleep(500 * time.Millisecond)
	u := s.Sample(busy)
	if len(u.Procs) != 3 || !u.Rated {
		t.Fatalf("busy group: %d procs, rated %v: %+v", len(u.Procs), u.Rated, u.Procs)
	}
	if runtime.GOOS == "linux" && u.CPU < 100 {
		t.Errorf("two yes processes should use over a core: %.0f%%", u.CPU)
	}
	if v := u.Verdict(0); runtime.GOOS == "linux" && !strings.Contains(v, "busy on CPU") {
		t.Errorf("busy verdict %q (%s)", v, u.Line())
	}

	idle := group(t, "exec sleep 30")
	s.Sample(idle)
	time.Sleep(300 * time.Millisecond)
	u = s.Sample(idle)
	if len(u.Procs) != 1 || u.Procs[0].State != "S" {
		t.Fatalf("idle group: %+v", u.Procs)
	}
	if v := u.Verdict(5 * time.Minute); runtime.GOOS == "linux" && (!strings.Contains(v, "idle") || !strings.Contains(v, "looks stuck")) {
		t.Errorf("idle verdict %q (%s)", v, u.Line())
	}
	if runtime.GOOS == "linux" && (u.FDs < 3 || u.ReadBps < 0 || u.Threads < 1) {
		t.Errorf("linux reads fds, io and threads: %s", u.Line())
	}
	t.Log(u.Line(), "—", u.Verdict(5*time.Minute))
}

// A group with nothing in it says so.
func TestSample_Gone(t *testing.T) {
	var s Sampler
	u := s.Sample(1 << 30)
	if len(u.Procs) != 0 || u.Line() != "no processes left in its group" {
		t.Errorf("%+v %q", u, u.Line())
	}
}
