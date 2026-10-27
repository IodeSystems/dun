package dun

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
)

// treeCommand starts a child that starts a grandchild, and writes both pids
// to pidFile: the inner sh forks `sleep 31` into the background and execs
// `sleep 32` in its own place. `echo never` must never run.
func treeCommand(pidFile string) string {
	return fmt.Sprintf(`sh -c 'sleep 31 & echo "$! $$" > %s; exec sleep 32'; echo never`, shellQuote(pidFile))
}

// readPids waits for treeCommand's pid file.
func readPids(t *testing.T, pidFile string) []int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b, _ := os.ReadFile(pidFile)
		if f := strings.Fields(string(b)); len(f) == 2 {
			var pids []int
			for _, s := range f {
				n, err := strconv.Atoi(s)
				if err != nil {
					t.Fatal(err)
				}
				pids = append(pids, n)
			}
			return pids
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the command never wrote its pids")
	return nil
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// waitGone waits for every pid to be gone. A killed grandchild is reparented
// and reaped by init, which can take a moment.
func waitGone(t *testing.T, pids []int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		left := 0
		for _, p := range pids {
			if alive(p) {
				left++
			}
		}
		if left == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, p := range pids {
		if alive(p) {
			_ = syscall.Kill(p, syscall.SIGKILL)
			t.Errorf("pid %d survived the kill", p)
		}
	}
}

// KillJob ends the job's whole process tree — child AND grandchild — on both
// host backends, and the job says it was killed, by whom, in its result, its
// log and its completion notice.
func TestKillJob_EndsTheProcessGroup(t *testing.T) {
	for name, mk := range map[string]func(dir string) ExecBackend{
		"shell": func(dir string) ExecBackend { return &HostShell{Dir: dir} },
		"exec":  func(dir string) ExecBackend { return HostExec{Dir: dir} },
	} {
		t.Run(name, func(t *testing.T) {
			h := testHarness(t)
			dir := t.TempDir()
			be := mk(dir)
			if c, ok := be.(*HostShell); ok {
				t.Cleanup(c.Close)
			}
			pidFile := filepath.Join(dir, "pids")
			j := h.newJobStarted(be, treeCommand(pidFile))
			pids := readPids(t, pidFile)
			for _, p := range pids {
				if !alive(p) {
					t.Fatalf("pid %d not running before the kill", p)
				}
			}

			start := time.Now()
			msg, err := h.KillJob(j.id, "user")
			if err != nil {
				t.Fatal(err)
			}
			if took := time.Since(start); took > jobKillWait {
				t.Errorf("the kill took %s", took)
			}
			if !strings.Contains(msg, "killed by the user after") {
				t.Errorf("answer: %q", msg)
			}
			waitGone(t, pids)

			if info := j.info(); info.State != "killed" {
				t.Errorf("state %q, want killed", info.State)
			}
			if strings.Contains(j.res.Output, "never") {
				t.Errorf("the command went on after the kill: %q", j.res.Output)
			}
			log, _ := os.ReadFile(j.logPath)
			if !strings.Contains(string(log), "[dun: killed by the user after") {
				t.Errorf("log does not say it was killed: %q", log)
			}
			notes := strings.Join(h.notes(), "\n")
			if !strings.Contains(notes, "was killed by the user after") || !strings.Contains(notes, "did not fail on its own") {
				t.Errorf("completion notice: %q", notes)
			}
			if strings.Contains(notes, "FAILED") {
				t.Errorf("a killed job must not read as a failure of its own: %q", notes)
			}
		})
	}
}

// A process that ignores SIGTERM gets SIGKILL after the grace.
func TestKillJob_EscalatesToSIGKILL(t *testing.T) {
	h := testHarness(t)
	dir := t.TempDir()
	be := &HostShell{Dir: dir}
	t.Cleanup(be.Close)
	pidFile := filepath.Join(dir, "pids")
	// trap '' TERM is inherited: neither sh nor its sleeps can see SIGTERM.
	j := h.newJobStarted(be, fmt.Sprintf(`sh -c 'trap "" TERM; sleep 31 & echo "$! $$" > %s; sleep 32'`, shellQuote(pidFile)))
	pids := readPids(t, pidFile)
	start := time.Now()
	if msg, _ := h.KillJob(j.id, "user"); !strings.Contains(msg, "killed by the user") {
		t.Errorf("answer: %q", msg)
	}
	if took := time.Since(start); took < killTermGrace {
		t.Errorf("ended in %s: SIGTERM was not ignored, so this tested nothing", took)
	}
	waitGone(t, pids)
}

// Under Docker the kill stops the CONTAINER (docker stop), which ends every
// process in it; killing the `docker run` client alone would leave it
// running. Needs a daemon and alpine:latest, so opt-in: DUN_DOCKER_TEST=1.
func TestKillJob_Docker(t *testing.T) {
	if os.Getenv("DUN_DOCKER_TEST") == "" {
		t.Skip("set DUN_DOCKER_TEST=1 to run (needs docker and alpine:latest)")
	}
	containers := func() string {
		out, _ := exec.Command("docker", "ps", "-a", "--format", "{{.Names}}",
			"--filter", fmt.Sprintf("name=dun-%d-", os.Getpid())).Output()
		return strings.TrimSpace(string(out))
	}
	h := testHarness(t)
	j := h.newJobStarted(DockerExec{Dir: t.TempDir(), Image: "alpine:latest"}, "sh -c 'sleep 300 & sleep 301'")
	deadline := time.Now().Add(30 * time.Second)
	for containers() == "" {
		if time.Now().After(deadline) {
			t.Fatal("the container never started")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if msg, _ := h.KillJob(j.id, "user"); !strings.Contains(msg, "killed by the user") {
		t.Errorf("answer: %q", msg)
	}
	deadline = time.Now().Add(5 * time.Second)
	for containers() != "" {
		if time.Now().After(deadline) {
			t.Fatalf("container still there after the kill: %s", containers())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// A command still inside its grace has no number yet; /jobs kill all still
// finds it, and the exec call that is waiting on it returns the "killed"
// result inline at once, instead of at the end of its grace.
func TestKillJob_DuringGrace(t *testing.T) {
	h := testHarness(t)
	d := execDispatcher(t, h)
	pidFile := filepath.Join(t.TempDir(), "pids")

	type result struct {
		out string
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := d(context.Background(), execCall(fmt.Sprintf(`{"command":%q,"timeout":60}`, treeCommand(pidFile))))
		done <- result{out, err}
	}()
	pids := readPids(t, pidFile)
	if got := h.KillAllJobs("user"); !strings.Contains(got, "killed by the user") || !strings.Contains(got, "not yet backgrounded") {
		t.Errorf("kill all: %q", got)
	}
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatal(r.err)
		}
		if !strings.Contains(r.out, "[exit: killed by the user after") {
			t.Errorf("inline result: %q", r.out)
		}
		if strings.Contains(r.out, "background job") {
			t.Errorf("a job killed in its grace must not be handed over: %q", r.out)
		}
	case <-time.After(jobKillWait):
		t.Fatal("the exec call did not return after the kill")
	}
	waitGone(t, pids)
	if js := h.Jobs(); len(js) != 0 {
		t.Errorf("a job killed in its grace must not leave a row: %+v", js)
	}
}

// A finished job answers "already finished", every time; a second kill of a
// killed job does too; an unknown id is the only error.
func TestKillJob_AlreadyFinished(t *testing.T) {
	h := testHarness(t)
	be := &HostShell{Dir: t.TempDir()}
	t.Cleanup(be.Close)
	j := h.newJobStarted(be, "echo hi")
	<-j.doneCh
	for range 2 {
		msg, err := h.KillJob(j.id, "user")
		if err != nil || !strings.Contains(msg, "already finished") || !strings.Contains(msg, "succeeded") {
			t.Errorf("finished job: %q, %v", msg, err)
		}
	}
	if j.info().State != "ok" {
		t.Errorf("a kill after the end must not relabel the job: %q", j.info().State)
	}

	k := h.newJobStarted(be, "sleep 30")
	if msg, _ := h.KillJob(k.id, "user"); !strings.Contains(msg, "killed by the user") {
		t.Fatalf("first kill: %q", msg)
	}
	if msg, _ := h.KillJob(k.id, "model"); !strings.Contains(msg, "already finished") || !strings.Contains(msg, "killed by the user") {
		t.Errorf("second kill: %q", msg)
	}
	if _, err := h.KillJob(99, "user"); err == nil {
		t.Error("an unknown job must be an error")
	}
	if got := h.KillAllJobs("user"); got != "no background jobs are running" {
		t.Errorf("kill all with none running: %q", got)
	}
}

// The model kills a job with jobs kill:true (or the old exec_monitor); the
// job says the model did it.
func TestJobs_KillCommand(t *testing.T) {
	h := testHarness(t)
	be := &HostShell{Dir: t.TempDir()}
	t.Cleanup(be.Close)
	j := h.newJobStarted(be, "sleep 30")
	mon := withJobs(nil, h, nil)
	call := func(args string) string {
		var tc llm.ToolCall
		tc.Function.Name, tc.Function.Arguments = "jobs", args
		out, err := mon(context.Background(), tc)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if out := call(`{"kill":true}`); !strings.HasPrefix(out, "ERROR") {
		t.Errorf("kill without a job: %q", out)
	}
	out := call(fmt.Sprintf(`{"job":%d,"kill":true}`, j.id))
	if !strings.Contains(out, "killed by the model after") {
		t.Errorf("jobs kill: %q", out)
	}
	kill := jobsToolDef(true, false).Function.Parameters.(map[string]any)["properties"].(map[string]any)["kill"].(map[string]any)
	if !strings.Contains(kill["description"].(string), "ends a command and everything it started") {
		t.Error("the tool definition must say what kill does")
	}
}

// An interrupt does NOT kill the cut turn's jobs; it lists them, for the
// model in the marker and for the user in InterruptedJobs, with the kill hint.
// A job still inside its grace gets its number then.
func TestInterrupt_ListsRunningJobs(t *testing.T) {
	r := &scriptRunner{rounds: []roundFn{
		func(_ context.Context, _ []llm.Message, out chan<- llm.StreamChunk) {
			tc := &llm.ToolCall{ID: "c1", Type: "function"}
			tc.Function.Name, tc.Function.Arguments = "exec", `{"command":"sleep 30","timeout":60}`
			out <- llm.StreamChunk{ToolCall: tc}
			out <- llm.StreamChunk{Done: true, Usage: &llm.Usage{}}
		},
	}}
	dir := t.TempDir()
	be := &HostShell{Dir: dir}
	h, err := Start(context.Background(), Config{
		Workspace: dir, Servers: []Server{}, Client: r, Exec: be,
		SessionFile: filepath.Join(dir, "s.jsonl"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	t.Cleanup(func() { h.KillAllJobs("user") })

	done := askAsync(h, "run the tests")
	deadline := time.Now().Add(5 * time.Second)
	for len(h.liveJobs()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the exec never started")
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond) // the command is running in its grace
	h.Interrupt("")
	if err := waitErr(t, done); !errors.Is(err, ErrInterrupted) {
		t.Fatalf("err = %v", err)
	}

	running := h.RunningJobs()
	if len(running) != 1 || running[0].Command != "sleep 30" {
		t.Fatalf("the job must still run after the interrupt: %+v", running)
	}
	id := running[0].ID
	var marker string
	for _, e := range entries(t, h) {
		if e.Kind == agent.KindNotification && e.Tag == interruptTag {
			marker = e.Content
		}
	}
	if !strings.Contains(marker, "STILL RUNNING") || !strings.Contains(marker, fmt.Sprintf("#%d sleep 30 (", id)) ||
		!strings.Contains(marker, "/jobs kill N") {
		t.Errorf("marker: %q", marker)
	}
	line := h.InterruptedJobs()
	if !strings.HasPrefix(line, fmt.Sprintf("still running: #%d sleep 30 (", id)) || !strings.Contains(line, fmt.Sprintf("/jobs kill %d", id)) {
		t.Errorf("user line: %q", line)
	}
}

func TestShortAge(t *testing.T) {
	for d, want := range map[time.Duration]string{
		42 * time.Second: "42s", 2*time.Minute + 5*time.Second: "2m", 65 * time.Minute: "1h5m",
	} {
		if got := ShortAge(d); got != want {
			t.Errorf("%s: %q, want %q", d, got, want)
		}
	}
}
