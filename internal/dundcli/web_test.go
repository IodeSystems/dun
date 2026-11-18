//go:build unix

package dundcli

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/dun/internal/dundapi"
	"github.com/iodesystems/dun/proto"
)

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := ln.Addr().String()
	ln.Close()
	return a
}

// runDund runs serve for home in this process until the returned stop.
func runDund(t *testing.T, l *loader, flagAddr string) (stop func()) {
	t.Helper()
	t.Setenv("INVOCATION_ID", "")
	t.Setenv("XPC_SERVICE_NAME", "")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, l, "/bin/true", flagAddr, dundapi.Options{}) }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		// An answer, not a connect: the socket is bound before the web
		// listener starts, and answered only after.
		if _, err := webSetLive(l.socket(), proto.WebSet{}); err == nil {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("dund did not start: %v", <-done)
		}
		time.Sleep(20 * time.Millisecond)
	}
	stopped := false
	stop = func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("serve: %v", err)
			}
		case <-time.After(20 * time.Second):
			t.Error("dund did not stop")
		}
	}
	t.Cleanup(stop)
	return stop
}

func serving(addr string) bool {
	c := &http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get("http://" + addr + "/healthz")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200
}

func set(t *testing.T, l *loader, ws proto.WebSet) (string, error) {
	t.Helper()
	var b bytes.Buffer
	err := webSet(l, &b, ws)
	return b.String(), err
}

func saved(t *testing.T, home string) webState {
	t.Helper()
	st, ok, err := loadWebState(home)
	if err != nil || !ok {
		t.Fatalf("web.json: %v %v", ok, err)
	}
	return st
}

// The listener starts, moves, refuses a busy port (staying where it was) and
// stops while dund runs; each change is saved, and a restart comes back to
// the saved state.
func TestWebListenerAtRuntime(t *testing.T) {
	home := t.TempDir()
	l := &loader{home: home}
	stop := runDund(t, l, "")
	a1, a2 := freeAddr(t), freeAddr(t)
	if _, ok, _ := loadWebState(home); ok {
		t.Fatal("web.json written with no flag and no change")
	}
	out, err := set(t, l, proto.WebSet{Enable: ptr(true), Addr: a1})
	if err != nil || !strings.Contains(out, "open http://"+a1+"/") || !serving(a1) {
		t.Fatalf("enable: %v %q", err, out)
	}
	if out, err := set(t, l, proto.WebSet{TUI: ptr(true)}); err != nil || !strings.Contains(out, "tui on") {
		t.Fatalf("enable tui: %v %q", err, out)
	}
	if st := saved(t, home); st != (webState{Enabled: true, Addr: a1, TUI: true}) {
		t.Fatalf("saved %+v", st)
	}
	if info, _ := os.Stat(webStatePath(home)); info.Mode().Perm() != 0o600 {
		t.Errorf("web.json mode %v", info.Mode().Perm())
	}
	if _, err := set(t, l, proto.WebSet{Enable: ptr(true), Addr: a2}); err != nil || !serving(a2) || serving(a1) {
		t.Fatalf("move: %v a1 %v a2 %v", err, serving(a1), serving(a2))
	}
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	if _, err := set(t, l, proto.WebSet{Addr: busy.Addr().String()}); err == nil || !serving(a2) {
		t.Fatalf("busy port: %v, still on a2: %v", err, serving(a2))
	}
	if saved(t, home).Addr != a2 {
		t.Error("a refused change was saved")
	}
	if _, err := set(t, l, proto.WebSet{Addr: "nope"}); err == nil {
		t.Error("a bad address was taken")
	}
	stop()
	if serving(a2) {
		t.Fatal("still serving after dund stopped")
	}

	// A restart restores it.
	stop = runDund(t, l, "")
	if !serving(a2) {
		t.Fatal("not restored after a restart")
	}
	if _, err := set(t, l, proto.WebSet{Enable: ptr(false)}); err != nil || serving(a2) {
		t.Fatalf("disable: %v %v", err, serving(a2))
	}
	stop()

	// With dund stopped the change is saved for its next start.
	out, err = set(t, l, proto.WebSet{Enable: ptr(true)})
	if err != nil || !strings.Contains(out, "applies when dund next starts") || !saved(t, home).Enabled {
		t.Fatalf("offline: %v %q", err, out)
	}
	runDund(t, l, "")
	if !serving(a2) {
		t.Fatal("the offline change did not apply at the next start")
	}
}

// The flag seeds web.json once; then web.json wins over it, and disabling
// stops a listener the flag started.
func TestWebFlagOnlySeeds(t *testing.T) {
	home := t.TempDir()
	l := &loader{home: home}
	a1, a2 := freeAddr(t), freeAddr(t)
	stop := runDund(t, l, a1)
	if !serving(a1) || saved(t, home) != (webState{Enabled: true, Addr: a1}) {
		t.Fatalf("seed: serving %v", serving(a1))
	}
	if _, err := set(t, l, proto.WebSet{Enable: ptr(false)}); err != nil || serving(a1) {
		t.Fatalf("disable a flag-started listener: %v", err)
	}
	stop()
	runDund(t, l, a2)
	if serving(a2) || serving(a1) {
		t.Fatal("the flag won over web.json")
	}
}

// check binds the address web.json names, not the flag's; beside a live
// dund (a handover) it only resolves it.
func TestCheckUsesTheSavedAddress(t *testing.T) {
	home := t.TempDir()
	l := &loader{home: home}
	bin := filepath.Join(t.TempDir(), "dun")
	fakeDun(t, bin, "v2", 0, "")
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	free := freeAddr(t)
	if err := saveWebState(home, webState{Enabled: true, Addr: busy.Addr().String()}); err != nil {
		t.Fatal(err)
	}
	if err := check(l, bin, free, "", 0); err == nil || !strings.Contains(err.Error(), busy.Addr().String()) {
		t.Fatalf("check bound the flag's address, not the saved one: %v", err)
	}
	live := os.Getppid()
	_ = os.WriteFile(l.pidfile(), []byte(strconv.Itoa(live)+"\n"), 0o600)
	if err := check(l, bin, "", "", live); err != nil {
		t.Fatalf("handover check with a live listener: %v", err)
	}
	_ = os.Remove(l.pidfile())
	if err := saveWebState(home, webState{Enabled: false, Addr: busy.Addr().String()}); err != nil {
		t.Fatal(err)
	}
	if err := check(l, bin, busy.Addr().String(), "", 0); err != nil {
		t.Fatalf("web off in web.json, the flag's busy address was still checked: %v", err)
	}
}

func TestStripWebFlag(t *testing.T) {
	for _, c := range []struct {
		in   []string
		out  string
		addr string
	}{
		{[]string{"--web-experimental", "1.2.3.4:5", "--oidc-issuer", "x"}, "--oidc-issuer x", "1.2.3.4:5"},
		{[]string{"--http=127.0.0.1:9"}, "", "127.0.0.1:9"},
		{[]string{"--releases", "/r"}, "--releases /r", ""},
	} {
		got, addr := stripWebFlag(c.in)
		if strings.Join(got, " ") != c.out || addr != c.addr {
			t.Errorf("%v: %v %q", c.in, got, addr)
		}
	}
}

func TestTUIEnv(t *testing.T) {
	env := tuiEnv([]string{"PATH=/bin", "TERM=screen", "INVOCATION_ID=x", "JOURNAL_STREAM=1", "DUN_HOME=/other", "HOME=/h"}, "/h/.dun")
	want := "PATH=/bin HOME=/h DUN_HOME=/h/.dun TERM=xterm-256color COLORTERM=truecolor DUN_CHILD=1"
	if strings.Join(env, " ") != want {
		t.Fatalf("%v", env)
	}
	if w := tuiWarning(webState{Enabled: true, TUI: true, Addr: "0.0.0.0:1"}); w == "" {
		t.Error("no warning for the TUI on all interfaces")
	}
	if w := tuiWarning(webState{Enabled: true, TUI: true, Addr: "127.0.0.1:1"}); w != "" {
		t.Error("a warning on loopback")
	}
}
