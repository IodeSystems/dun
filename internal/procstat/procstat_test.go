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
	// How much CPU the two yes get is the host's: on a loaded box (go test
	// runs packages in parallel; loadavg 80 on 24 cores) they got 30-50%,
	// under the Verdict's busy line. So the live read asserts what load
	// does not change: every member found, rated, CPU read, both yes
	// runnable; TestVerdict_Busy holds the busy line. A sample comes each
	// 100ms until the group is rated with CPU, so a slow start is waited for.
	s.Sample(busy)
	var u Usage
	for deadline := time.Now().Add(10 * time.Second); ; {
		time.Sleep(100 * time.Millisecond)
		u = s.Sample(busy)
		if (len(u.Procs) == 3 && u.Rated && u.CPU > 0) || time.Now().After(deadline) {
			break
		}
	}
	if len(u.Procs) != 3 || !u.Rated {
		t.Fatalf("busy group: %d procs, rated %v: %+v", len(u.Procs), u.Rated, u.Procs)
	}
	if runtime.GOOS == "linux" {
		running := 0
		for _, p := range u.Procs {
			if p.State == "R" {
				running++
			}
		}
		if u.CPU <= 0 || running < 2 {
			t.Errorf("two yes processes should read as on CPU: %.0f%%, %d runnable (%s)", u.CPU, running, u.Line())
		}
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

func TestIsNetworkWchan(t *testing.T) {
	net := []string{
		"inet_csk_accept",
		"tcp_recvmsg",
		"tcp_sendmsg",
		"udp_recvmsg",
		"sk_wait_data",
		"napi_poll",
		"sock_wait_for_connect",
		"netfilter_queue",
		"nf_conntrack_in",
	}
	for _, w := range net {
		if !isNetworkWchan(w) {
			t.Errorf("isNetworkWchan(%q) = false, want true", w)
		}
	}
	nonet := []string{"hrtimer_nanosleep", "futex_wait_queue", "0", "-", ""}
	for _, w := range nonet {
		if isNetworkWchan(w) {
			t.Errorf("isNetworkWchan(%q) = true, want false", w)
		}
	}
}

// A group at or over half a core reads as busy, and quiet for a minute
// says so; under it, it is not called busy.
func TestVerdict_Busy(t *testing.T) {
	busy := Usage{Rated: true, CPU: 150, Procs: []Proc{{State: "R", CPU: 75}, {State: "R", CPU: 75}, {State: "S", CPU: 0}}}
	if v := busy.Verdict(0); v != "busy on CPU" {
		t.Errorf("busy verdict %q", v)
	}
	if v := busy.Verdict(5 * time.Minute); !strings.Contains(v, "busy on CPU") || !strings.Contains(v, "printing nothing") {
		t.Errorf("busy and quiet verdict %q", v)
	}
	under := Usage{Rated: true, CPU: 30, Procs: []Proc{{State: "R", CPU: 30}}}
	if v := under.Verdict(0); strings.Contains(v, "busy") {
		t.Errorf("30%% read as busy: %q", v)
	}
}

func TestVerdict_NetworkWait(t *testing.T) {
	u := Usage{Rated: true, Procs: []Proc{{State: "S", CPU: 0, WChan: "tcp_recvmsg"}}}
	v := u.Verdict(0)
	if !strings.Contains(v, "waiting on network") {
		t.Errorf("network-wait verdict %q, want 'waiting on network'", v)
	}
}

func TestVerdict_NotNetworkWait(t *testing.T) {
	u := Usage{Rated: true, Procs: []Proc{{State: "S", CPU: 0, WChan: "hrtimer_nanosleep"}}}
	v := u.Verdict(0)
	if strings.Contains(v, "waiting on network") {
		t.Errorf("non-network verdict %q should not say 'waiting on network'", v)
	}
}

// A group with nothing in it says so.
func TestSample_Gone(t *testing.T) {
	var s Sampler
	u := s.Sample(1 << 30)
	if len(u.Procs) != 0 || u.Line() != "no processes left in its group" {
		t.Errorf("%+v %q", u, u.Line())
	}
}
