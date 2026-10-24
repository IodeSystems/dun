package main

import (
	"bufio"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/iodesystems/dun/internal/dund"
	"github.com/iodesystems/dun/proto"
)

func TestBuildDecision(t *testing.T) {
	for _, c := range []struct {
		mine, dund, disk string
		want             buildAction
	}{
		{"a", "a", "a", buildsAgree},
		{"a", "b", "b", reexecSelf},   // this file is dund's build now
		{"a", "b", "a", upgradeDund},  // this file is still this build
		{"a", "b", "", upgradeDund},   // the file would not say
		{"a", "b", "c", buildsDiffer}, // three builds
		{"dev", "b", "b", buildsAgree},
		{"a", "dev", "a", buildsAgree},
	} {
		if got := buildDecision(c.mine, c.dund, c.disk); got != c.want {
			t.Errorf("buildDecision(%q, %q, %q) = %v, want %v", c.mine, c.dund, c.disk, got, c.want)
		}
	}
}

var pendingEv = map[string]any{"type": "upgrade", "state": "pending", "version": "d80fc66", "deadline": float64(1700000000),
	"waiting": []any{
		map[string]any{"session": "here", "what": "turn"},
		map[string]any{"session": "here", "what": "job", "id": float64(3), "detail": "go test ./..."},
		map[string]any{"session": "20261022-1043", "what": "turn"},
	}}

// The pending upgrade is one row of its own; /upgrade, named on it before
// any detail, says everything: each wait and how to end the ones here.
func TestUpgradeStatusRow(t *testing.T) {
	u := parseUpgrade(pendingEv)
	row := upgradeRow(u, "here", 200)
	for _, want := range []string{"⟳ dund upgrade to d80fc66 · /upgrade · waits for 3 things: the turn in this session"} {
		if !strings.Contains(row, want) {
			t.Errorf("row lacks %q: %q", want, row)
		}
	}
	// At phone width the cut keeps the way to the detail (USER 2026-10-24).
	phone := upgradeRow(u, "here", 49)
	if n := len([]rune(phone)); n > 49 || !strings.Contains(phone, "/upgrade") {
		t.Errorf("phone row (%d runes): %q", n, phone)
	}
	d := upgradeDetail(u, "here")
	for _, want := range []string{"dund is upgrading to d80fc66.", "at the latest", "Waiting for:",
		"the turn in this session (ctrl+c interrupts)", "job #3 go test ./... (/jobs kill 3)",
		"a turn in session 20261022-1043 (another client)", "/upgrade now restarts it now"} {
		if !strings.Contains(d, want) {
			t.Errorf("detail lacks %q:\n%s", want, d)
		}
	}
	one := upgradeRow(parseUpgrade(map[string]any{"type": "upgrade", "state": "pending", "version": "d80fc66",
		"waiting": []any{map[string]any{"session": "here", "what": "turn"}}}), "here", 300)
	if !strings.HasSuffix(one, "/upgrade · waits for the turn in this session (ctrl+c interrupts)") {
		t.Errorf("one wait: %q", one)
	}
	r := upgradeRow(parseUpgrade(map[string]any{"type": "upgrade", "state": "restarting", "version": "d80fc66"}), "here", 200)
	if !strings.Contains(r, "dund is restarting into d80fc66") {
		t.Errorf("restarting: %q", r)
	}
}

// /upgrade at 49 columns shows every wait, wrapped, not cut.
func TestUpgradeCommandAtPhoneWidth(t *testing.T) {
	v := newVtui(49, 21)
	v.m.sessionID = "here"
	v.event(pendingEv)
	v.send("/upgrade")
	flat := strings.Join(strings.Fields(v.view()), " ")
	for _, want := range []string{"Waiting for:", "(/jobs kill 3)", "(another client)", "/upgrade now restarts it now"} {
		if !strings.Contains(flat, want) {
			t.Errorf("/upgrade at 49 columns lacks %q:\n%s", want, v.view())
		}
	}
}

// The status row shows while pending, the interrupt still works then, and a
// reconnect clears the row.
func TestUpgradeRowInTheTUIAndInterrupt(t *testing.T) {
	v, rec := busyVtui(t)
	v.m.sessionID = "here"
	v.event(pendingEv)
	if !strings.Contains(v.view(), "⟳ dund upgrade to d80fc66 · /upgrade · waits for 3 things") {
		t.Fatalf("no upgrade row:\n%s", v.view())
	}
	v.m = key(v.m, kCtrlG)
	if !sentInterrupt(rec) {
		t.Fatal("the interrupt key did nothing while an upgrade was pending")
	}
	m, _ := v.m.update(engineUpMsg{proc: v.m.proc, reattach: true})
	if tm := m.(tuiModel); tm.upgrade != nil {
		t.Fatal("a reconnect kept the old dund's upgrade row")
	}
}

// A failed upgrade (a service could not put the new build in place) clears
// the row and says why, once, in the conversation.
func TestUpgradeFailedClearsTheRowAndSaysWhy(t *testing.T) {
	v, _ := busyVtui(t)
	v.event(pendingEv)
	v.event(map[string]any{"type": "upgrade", "state": "failed", "version": "d80fc66",
		"error": "upgrade: the new build fails `serve --check` with the service's flags: exit status 1: bad flag"})
	if v.m.upgrade != nil {
		t.Fatal("the row stayed after the upgrade failed")
	}
	if got := v.view(); strings.Contains(got, "⟳ dund upgrade") ||
		!strings.Contains(got, "dund could not upgrade to d80fc66") {
		t.Fatalf("view:\n%s", got)
	}
}

// A service that cannot upgrade itself is told the step that works: a
// restart only when its unit runs this very binary.
func TestServiceFix(t *testing.T) {
	for _, c := range []struct {
		r    proto.HelloReply
		want string
	}{
		{proto.HelloReply{Exe: "/me/dun"}, "dun dund service restart"},
		{proto.HelloReply{Exe: "/svc/dun", NoUpgrade: "this user cannot write in /svc"}, "install the service from this build: /me/dun dund service install --start"},
		{proto.HelloReply{Exe: "/svc/dun"}, "install -m 755 /me/dun /svc/dun.new && mv -f /svc/dun.new /svc/dun"},
	} {
		if got := serviceFix(&c.r, "/me/dun"); !strings.Contains(got, c.want) {
			t.Errorf("%+v: %q, want %q", c.r, got, c.want)
		}
	}
}

// /upgrade now asks dund to restart at once.
func TestUpgradeNowCommand(t *testing.T) {
	v, rec := busyVtui(t)
	v.event(pendingEv)
	v.send("/upgrade now")
	if !strings.Contains(rec.String(), `"type":"upgrade"`) || !strings.Contains(rec.String(), `"now":true`) {
		t.Fatalf("sent %q", rec.String())
	}
}

// A self_reexec quits the TUI for /reload's re-exec.
func TestSelfReexecQuits(t *testing.T) {
	v := newVtui(80, 24)
	m, cmd := v.m.update(evMsg{"type": "self_reexec", "version": "b"})
	if !m.(tuiModel).reloadReq || cmd == nil {
		t.Fatal("did not ask to re-exec")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("did not quit")
	}
}

// dund's automatic resume shows dim as what it is, live and replayed.
func TestAutoResumeRow(t *testing.T) {
	if got := userRow("[automatic] dund restarted into build b while you were working."); !strings.Contains(got, "↻ resumed after the restart — dund restarted") {
		t.Errorf("row %q", got)
	}
	if got := userRow("hello"); !strings.Contains(got, "› hello") {
		t.Errorf("row %q", got)
	}
}

// A dund from before coordinated upgrades (hello, no broadcast) gets the
// command that replaces it, once, and is never asked to upgrade.
func TestCheckDundBuildOldDund(t *testing.T) {
	old := version
	version = "new"
	defer func() { version = old }()
	upgradeAsked = map[string]bool{}
	sock := filepath.Join(t.TempDir(), "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var cmds []string
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			sc := bufio.NewScanner(c)
			for sc.Scan() {
				var cmd proto.Command
				_ = json.Unmarshal(sc.Bytes(), &cmd)
				cmds = append(cmds, cmd.Type)
				ev := proto.NewEvent(&proto.HelloReply{Version: "old", Upgradable: true})
				ev.Re = cmd.CID
				_ = json.NewEncoder(c).Encode(ev)
			}
		}
	}()
	check := func() []string {
		c, err := net.Dial("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		n, _ := checkDundBuild(&attachConn{c: c}, bufio.NewScanner(c))
		return n
	}
	if n := check(); len(n) != 1 || !strings.Contains(n[0], "too old to coordinate") || !strings.Contains(n[0], "kill $(cat ") {
		t.Fatalf("first: %v", n)
	}
	if n := check(); len(n) != 0 {
		t.Fatalf("said twice: %v", n)
	}
	for _, c := range cmds {
		if c == "upgrade" {
			t.Fatal("an old dund was asked to upgrade")
		}
	}
}

// Against a dund that coordinates: asked once per build pair; told it came
// back the same old build once; and a TUI whose file is now dund's build
// re-execs instead of asking dund to move back.
func TestCheckDundBuild(t *testing.T) {
	old, oldProbe := version, probeSelf
	version = "new"
	defer func() { version, probeSelf = old, oldProbe }()
	upgradeAsked = map[string]bool{}
	disk := "new"
	probeSelf = func(string) string { return disk }

	svc, err := dund.New(t.TempDir(), dund.ExecSpawner{Bin: "true"})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	sock := filepath.Join(t.TempDir(), "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	srv := dund.NewServer(svc, func(net.Conn) (string, error) { return "unix:1", nil })
	asked := 0
	srv.SetBuild("old", "/old/dun", func(string, string) error { asked++; return nil })
	go srv.Serve(ln)

	check := func() ([]string, string) {
		c, err := net.Dial("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		return checkDundBuild(&attachConn{c: c}, bufio.NewScanner(c))
	}
	if n, re := check(); asked != 1 || len(n) != 0 || re != "" {
		t.Fatalf("first: asked=%d %v %q", asked, n, re)
	}
	if n, _ := check(); asked != 1 || len(n) != 1 || !strings.Contains(n[0], "still runs build old") || strings.Contains(n[0], "stale copy") {
		t.Fatalf("second: asked=%d %v", asked, n)
	}
	if n, _ := check(); asked != 1 || len(n) != 0 {
		t.Fatalf("third: asked=%d %v (said twice)", asked, n)
	}
	disk = "old"
	if n, re := check(); asked != 1 || re != "old" || len(n) != 1 {
		t.Fatalf("file now dund's build: asked=%d %v %q", asked, n, re)
	}
}
