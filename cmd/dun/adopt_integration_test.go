//go:build unix

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/iodesystems/dun/proto"
)

// The real thing (docs/engine-adoption.md): a dund of build v1 runs a turn
// that streams slowly from a fake LLM; mid-turn it is upgraded to build v2
// and re-execs. The engine is not stopped: the turn's tokens reach the
// client across the restart in order, none lost and none twice, the turn
// completes, and at its idle the engine moves to build v2.
func TestAdoptAcrossDundReexec(t *testing.T) {
	if testing.Short() {
		t.Skip("builds dun twice and runs a dund")
	}
	t.Parallel() // its own processes and env: it overlaps the other builds (testDun)
	adoptAcrossRestart(t, false)
}

// The same through a real systemd user manager: dund runs as a transient
// service, the upgrade is the handover (the new build copied over the
// unit's binary, exit 75, systemd starts it again), and the engine, in a
// scope of its own, outlives the service's cgroup. Opt-in
// (DUN_TEST_SYSTEMD=1): it runs units in the user's own systemd manager.
func TestAdoptAcrossSystemdHandover(t *testing.T) {
	if os.Getenv("DUN_TEST_SYSTEMD") != "1" {
		t.Skip("DUN_TEST_SYSTEMD=1 runs it in the user's systemd manager")
	}
	if _, err := exec.LookPath("systemd-run"); err != nil {
		t.Skip("no systemd-run")
	}
	adoptAcrossRestart(t, true)
}

func adoptAcrossRestart(t *testing.T, viaSystemd bool) {
	const words = 30
	// A handover checks the new build (serve --check) before it exits: the
	// turn must outlast that, race builds being slower.
	wordGap := 100 * time.Millisecond
	if viaSystemd {
		wordGap = 400 * time.Millisecond
	}
	bins := t.TempDir()
	v1, v2 := filepath.Join(bins, "v1", "dun"), filepath.Join(bins, "v2", "dun")
	// The two builds differ only in the link: side by side, not one after
	// the other.
	var builds sync.WaitGroup
	buildErr := make(chan string, 2)
	for exe, ver := range map[string]string{v1: "v9.9.1", v2: "v9.9.2"} {
		args := []string{"build", "-o", exe, "-ldflags", "-X main.version=" + ver}
		if raceBuild {
			args = append(args, "-race")
		}
		build := exec.Command("go", append(args, ".")...)
		build.Env = append(os.Environ(), "GOWORK=off")
		builds.Add(1)
		go func() {
			defer builds.Done()
			if out, err := build.CombinedOutput(); err != nil {
				buildErr <- fmt.Sprintf("go build %s: %v\n%s", ver, err, out)
			}
		}()
	}
	builds.Wait()
	close(buildErr)
	for e := range buildErr {
		t.Fatal(e)
	}

	// The fake LLM: the turn's request streams w01..w30 slowly; anything
	// else (a suggestion, a title) is answered at once.
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		chunk := func(s string) {
			b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": s}}}})
			fmt.Fprintf(w, "data: %s\n\n", b)
			w.(http.Flusher).Flush()
		}
		if strings.Contains(string(body), "COUNT SLOWLY") && !strings.Contains(string(body), "w01") {
			for i := 1; i <= words; i++ {
				chunk(fmt.Sprintf("w%02d ", i))
				time.Sleep(wordGap)
			}
		} else {
			chunk("ok")
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer llm.Close()

	home, err := os.MkdirTemp("", "da") // short: socket paths
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(home)
	ws := t.TempDir()
	sock := filepath.Join(home, "d.sock")
	env := []string{"DUN_HOME=" + home, "HOME=" + home, "DUN_REGISTRY=off",
		"DUN_NO_AUTOBUILD=1", "DUN_NO_SELFUPDATE=1", "DUN_MEMORY=0", "DUN_CODE_SEARCH=0", "DUN_USER_RULES=0",
		"DUN_NO_BROWSER=1", "POLY_LSP_NO_DAEMON=1", "DUN_URL=" + llm.URL + "/v1", "DUN_MODEL=m", "DUN_LLM_KEY=k",
		"DUN_CHILD=", "DUN_DUND="}
	logPath := filepath.Join(home, "dund.log")
	logf, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer logf.Close()
	argv := []string{v1, "dund", "--home", home, "--socket", sock, "serve"}
	var stopDund func()
	if viaSystemd {
		// The unit as `dund service install` writes it, in what matters
		// here: restart on 75, output to a file.
		unit := fmt.Sprintf("dun-adopt-test-%d", os.Getpid())
		run := []string{"--user", "--unit=" + unit, "--collect", "-p", "Restart=on-failure", "-p", "RestartSec=1",
			"-p", "SuccessExitStatus=75", "-p", "RestartForceExitStatus=75",
			"-p", "StandardOutput=append:" + logPath, "-p", "StandardError=append:" + logPath}
		for _, e := range env {
			run = append(run, "--setenv="+e)
		}
		// The user manager's bus, which TestMain's XDG_RUNTIME_DIR hides.
		mgr := func(name string, args ...string) *exec.Cmd {
			c := exec.Command(name, args...)
			c.Env = append(os.Environ(), fmt.Sprintf("XDG_RUNTIME_DIR=/run/user/%d", os.Getuid()))
			return c
		}
		if out, err := mgr("systemd-run", append(append(run, "--"), argv...)...).CombinedOutput(); err != nil {
			t.Fatalf("systemd-run: %v\n%s", err, out)
		}
		stopDund = func() {
			_ = mgr("systemctl", "--user", "stop", unit).Run()
			_ = mgr("systemctl", "--user", "reset-failed", unit).Run()
		}
	} else {
		dund := exec.Command(argv[0], argv[1:]...)
		// Not a service: no handover, no scopes; XDG_RUNTIME_DIR off the
		// user's own.
		dund.Env = append(os.Environ(), append(env, "XDG_RUNTIME_DIR="+home, "INVOCATION_ID=", "XPC_SERVICE_NAME=")...)
		dund.Stdout, dund.Stderr = logf, logf
		if err := dund.Start(); err != nil {
			t.Fatal(err)
		}
		stopDund = func() {
			// SIGTERM: a stop, which ends the engines.
			_ = dund.Process.Signal(syscall.SIGTERM)
			exited := make(chan struct{})
			go func() { _ = dund.Wait(); close(exited) }()
			select {
			case <-exited:
			case <-time.After(30 * time.Second):
				_ = dund.Process.Kill()
			}
		}
	}
	engines := filepath.Join(home, "dund", "engines")
	defer func() {
		stopDund()
		// Then any engine left, by pid.
		for _, info := range engineInfos(engines) {
			_ = syscall.Kill(info.Pid, syscall.SIGKILL)
		}
		logs, _ := filepath.Glob(filepath.Join(engines, "*.log"))
		logs = append([]string{logPath}, logs...)
		for _, l := range logs {
			// The binaries are race builds under -race: a race in dund or
			// an engine is in its log.
			if b, _ := os.ReadFile(l); strings.Contains(string(b), "WARNING: DATA RACE") {
				t.Errorf("a data race in %s", l)
			}
		}
		if t.Failed() {
			for _, l := range logs {
				b, _ := os.ReadFile(l)
				t.Logf("%s:\n%s", l, b)
			}
		}
	}()

	c := dialAdoptDund(t, sock)
	c.send("c1", "", &proto.Create{Workspace: ws, Args: []string{"--no-tools"}})
	ack := c.until("create", func(ev proto.Event) bool { return ev.Type == "ack" && ev.Re == "c1" })
	if p := ack.Payload.(*proto.Ack); p.Error != "" {
		t.Fatalf("create: %s", p.Error)
	}
	id := ack.Session
	c.send("c2", id, &proto.Subscribe{})
	c.until("subscribe", func(ev proto.Event) bool { return ev.Type == "ack" && ev.Re == "c2" })
	if infos := engineInfos(engines); len(infos) != 1 || infos[0].Version != "v9.9.1" || infos[0].Session != id {
		t.Fatalf("engines: %+v", infos)
	}
	enginePid := engineInfos(engines)[0].Pid
	if viaSystemd {
		// In a scope of its own, not in the service's cgroup.
		cg, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", enginePid))
		if !strings.Contains(string(cg), ".scope") || strings.Contains(string(cg), "dun-adopt-test-") {
			t.Fatalf("engine cgroup: %s", cg)
		}
	}
	c.send("c3", id, &proto.User{Content: "COUNT SLOWLY to thirty"})

	// Mid-turn: the upgrade, at once (an adoptable engine is waited for by
	// nothing).
	var got []string
	upgraded := false
	for !upgraded {
		ev := c.next("a token")
		if tk, ok := ev.Payload.(*proto.Token); ok {
			got = append(got, tk.Text)
			if len(got) == 8 {
				c.send("c4", "", &proto.Upgrade{Exe: v2, Version: "v9.9.2"})
				upgraded = true
			}
		}
	}
	// The old dund's last events, to its socket's end.
	for {
		ev, ok := c.read()
		if !ok {
			break
		}
		if ack, isAck := ev.Payload.(*proto.Ack); isAck && ev.Re == "c4" && ack.Error != "" {
			t.Fatalf("upgrade: %s", ack.Error)
		}
		if tk, isTok := ev.Payload.(*proto.Token); isTok {
			got = append(got, tk.Text)
		}
	}
	if len(got) >= words {
		t.Fatalf("the turn ended before the restart (%d tokens): slow the fake LLM", len(got))
	}
	old := len(got)

	// The new dund (the same pid after an exec, a new one after a
	// handover), the engine still running.
	c2 := dialAdoptDund(t, sock)
	c2.send("h", "", &proto.Hello{Version: "test"})
	hello := c2.until("hello", func(ev proto.Event) bool { return ev.Re == "h" })
	if hr, ok := hello.Payload.(*proto.HelloReply); !ok || hr.Version != "v9.9.2" {
		t.Fatalf("hello after the restart: %+v", hello.Payload)
	}
	if syscall.Kill(enginePid, 0) != nil {
		t.Fatal("the engine died with the restart")
	}
	c2.send("s", id, &proto.Subscribe{})
	var message string
	done := false
	for !done {
		ev := c2.next("the turn's end")
		switch p := ev.Payload.(type) {
		case *proto.Snapshot:
			t.Fatalf("a snapshot for a fresh log: %d events", len(p.State))
		case *proto.Token:
			got = append(got, p.Text)
		case *proto.Message:
			message = p.Content
		case *proto.Done:
			done = true
		case *proto.History:
			if p.Reason == proto.HistoryReattach {
				t.Fatal("the ring overflowed")
			}
		}
	}
	var want []string
	for i := 1; i <= words; i++ {
		want = append(want, fmt.Sprintf("w%02d ", i))
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("tokens across the restart (%d before it):\n got %q\nwant %q", old, got, want)
	}
	if strings.TrimSpace(message) != strings.TrimSpace(strings.Join(want, "")) {
		t.Fatalf("message = %q", message)
	}
	if old == 0 {
		t.Fatal("no token before the restart")
	}

	// Idle now: the v1 engine moves to v2.
	deadline := time.Now().Add(60 * time.Second)
	for {
		infos := engineInfos(engines)
		if len(infos) == 1 && infos[0].Version == "v9.9.2" && infos[0].Session == id && infos[0].Pid != enginePid {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the adopted engine did not move to v9.9.2: %+v", infos)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if syscall.Kill(enginePid, 0) == nil {
		t.Fatal("the old engine still runs")
	}
	b, _ := os.ReadFile(logPath)
	want = []string{"restarting into " + v2, "adopted engine"}
	if viaSystemd {
		want[0] = "exiting 75 for the service manager"
	}
	for _, w := range want {
		if !strings.Contains(string(b), w) {
			t.Errorf("dund's log does not say %q", w)
		}
	}
}

func engineInfos(dir string) []proto.EngineInfo {
	paths, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	var out []proto.EngineInfo
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var info proto.EngineInfo
		if json.Unmarshal(b, &info) == nil {
			out = append(out, info)
		}
	}
	return out
}

type adoptConn struct {
	t  *testing.T
	c  net.Conn
	sc *bufio.Scanner
}

// dialAdoptDund connects to a dund's socket, waiting for it to listen.
func dialAdoptDund(t *testing.T, sock string) *adoptConn {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		c, err := net.Dial("unix", sock)
		if err == nil {
			t.Cleanup(func() { c.Close() })
			sc := bufio.NewScanner(c)
			sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
			return &adoptConn{t: t, c: c, sc: sc}
		}
		if time.Now().After(deadline) {
			t.Fatalf("dund did not listen on %s: %v", sock, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (d *adoptConn) send(cid, session string, payload any) {
	d.t.Helper()
	cmd := proto.NewCommand(cid, payload)
	cmd.Session = session
	b, _ := json.Marshal(cmd)
	if _, err := d.c.Write(append(b, '\n')); err != nil {
		d.t.Fatal(err)
	}
}

// read is the next event, false at the socket's end.
func (d *adoptConn) read() (proto.Event, bool) {
	_ = d.c.SetReadDeadline(time.Now().Add(60 * time.Second))
	for d.sc.Scan() {
		var ev proto.Event
		if json.Unmarshal(d.sc.Bytes(), &ev) == nil {
			return ev, true
		}
	}
	return proto.Event{}, false
}

func (d *adoptConn) next(what string) proto.Event {
	d.t.Helper()
	ev, ok := d.read()
	if !ok {
		d.t.Fatalf("waiting for %s: dund's socket ended (%v)", what, d.sc.Err())
	}
	return ev
}

func (d *adoptConn) until(what string, match func(proto.Event) bool) proto.Event {
	d.t.Helper()
	for {
		if ev := d.next(what); match(ev) {
			return ev
		}
	}
}
