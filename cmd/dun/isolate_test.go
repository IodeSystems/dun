package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/iodesystems/dun"
	"github.com/iodesystems/dun/proto"
)

// isoHarness is a session on dir: no tools, no servers.
func isoHarness(t *testing.T, dir string) *dun.Harness {
	t.Helper()
	h, err := dun.Start(context.Background(), dun.Config{Workspace: dir, NoTools: true, Servers: []dun.Server{},
		SessionFile: filepath.Join(t.TempDir(), "s.jsonl")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return h
}

// isoRepo is a git repository with one commit.
func isoRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git := []string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}
	runGit(t, dir, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "a.txt")
	runGit(t, dir, append(git, "commit", "-q", "-m", "init")...)
	return dir
}

// dockerAvailable sets the docker probe for one test.
func dockerAvailable(t *testing.T, err error) {
	t.Helper()
	old := dockerProbe
	dockerProbe = func() error { return err }
	t.Cleanup(func() { dockerProbe = old })
}

func ctl(h *dun.Harness, id, action string) string {
	return runControlCmd(context.Background(), h, id, action)
}

// In place → worktree → docker → off, each step what it says.
func TestIsolate_Transitions(t *testing.T) {
	dockerAvailable(t, nil)
	repo := isoRepo(t)
	h := isoHarness(t, repo)

	out := ctl(h, "isolate", "")
	if !strings.Contains(out, "isolate: none — in place: "+repo) || !strings.Contains(out, "/isolate docker") {
		t.Errorf("status in place: %q", out)
	}
	if isolationMode(h) != dun.IsoNone {
		t.Fatalf("mode %q", isolationMode(h))
	}

	out = ctl(h, "isolate", "worktree")
	wt := h.Worktree()
	if wt == nil || wt.Branch == "" || h.Workspace() == repo || h.Workspace() != wt.Path || h.IsDocker() {
		t.Fatalf("worktree: ws %q wt %+v docker %v (%q)", h.Workspace(), wt, h.IsDocker(), out)
	}
	if !strings.Contains(out, "created the worktree") || !strings.Contains(out, wt.Path) || !strings.Contains(out, wt.Branch) {
		t.Errorf("worktree reply: %q", out)
	}
	if _, err := os.Stat(filepath.Join(wt.Path, "a.txt")); err != nil {
		t.Errorf("the worktree holds the checkout: %v", err)
	}
	if out := ctl(h, "isolate", "worktree"); !strings.Contains(out, "already worktree") || h.Worktree().Path != wt.Path {
		t.Errorf("worktree again: %q", out)
	}

	// docker from a worktree keeps that worktree.
	out = ctl(h, "isolate", "docker")
	if !h.IsDocker() || h.Worktree().Path != wt.Path || isolationMode(h) != dun.IsoDocker {
		t.Fatalf("docker: %q, ws %q", out, h.Workspace())
	}
	if !strings.Contains(out, "kept the worktree") || !strings.Contains(out, "image golang:1.23") {
		t.Errorf("docker reply: %q", out)
	}
	if d := h.ExecBackend().(dun.DockerExec); d.Dir != wt.Path {
		t.Errorf("the container is on %q, want the worktree %q", d.Dir, wt.Path)
	}

	out = ctl(h, "isolate", "off")
	if h.Workspace() != repo || h.IsDocker() || isolationMode(h) != dun.IsoNone {
		t.Fatalf("off: ws %q docker %v (%q)", h.Workspace(), h.IsDocker(), out)
	}
	if !strings.Contains(out, "in place: "+repo) || !strings.Contains(out, "the worktree is kept: "+wt.Path) ||
		!strings.Contains(out, wt.Branch) || !strings.Contains(out, "git worktree remove "+wt.Path) {
		t.Errorf("off reply: %q", out)
	}
	if _, err := os.Stat(wt.Path); err != nil {
		t.Errorf("off deleted the worktree: %v", err)
	}
	if out := ctl(h, "isolate", "none"); !strings.Contains(out, "already in place") {
		t.Errorf("none in place: %q", out)
	}
}

// docker from in place makes a worktree too: a container on the checkout
// would not isolate the files.
func TestIsolate_DockerBringsAWorktree(t *testing.T) {
	dockerAvailable(t, nil)
	repo := isoRepo(t)
	h := isoHarness(t, repo)
	out := ctl(h, "isolate", "docker")
	wt := h.Worktree()
	if !h.IsDocker() || wt == nil || wt.Branch == "" || h.Workspace() == repo {
		t.Fatalf("docker from in place: %q, ws %q", out, h.Workspace())
	}
	if !strings.Contains(out, "created the worktree") || !strings.Contains(out, "isolate: docker — worktree: "+wt.Path) {
		t.Errorf("reply: %q", out)
	}
	// worktree from docker: same tree, commands back on this machine.
	ctl(h, "isolate", "worktree")
	if h.IsDocker() || h.Worktree().Path != wt.Path {
		t.Errorf("worktree from docker: docker %v ws %q", h.IsDocker(), h.Workspace())
	}
}

func TestIsolate_RefusesOutsideARepo(t *testing.T) {
	dockerAvailable(t, nil)
	dir := t.TempDir()
	h := isoHarness(t, dir)
	for _, mode := range []string{"worktree", "docker"} {
		out := ctl(h, "isolate", mode)
		if !strings.Contains(out, "isolate: "+mode+" refused") || !strings.Contains(out, "not in a git repository") {
			t.Errorf("%s: %q", mode, out)
		}
		if h.Workspace() != dir || h.IsDocker() {
			t.Errorf("%s moved the session: ws %q docker %v", mode, h.Workspace(), h.IsDocker())
		}
	}
}

func TestIsolate_RefusesDockerWithoutDocker(t *testing.T) {
	dockerAvailable(t, errors.New("docker is not installed (not on PATH)"))
	repo := isoRepo(t)
	h := isoHarness(t, repo)
	out := ctl(h, "isolate", "docker")
	if !strings.Contains(out, "docker refused — docker is not installed") || !strings.Contains(out, "/isolate worktree") {
		t.Errorf("reply: %q", out)
	}
	if h.Workspace() != repo || h.IsDocker() {
		t.Errorf("moved: ws %q docker %v", h.Workspace(), h.IsDocker())
	}
	// No worktree was made on the way to the refusal.
	if list, _ := exec.Command("git", "-C", repo, "worktree", "list").Output(); strings.Count(string(list), "\n") != 1 {
		t.Errorf("a worktree was made:\n%s", list)
	}
	// worktree needs no Docker.
	if out := ctl(h, "isolate", "worktree"); !strings.Contains(out, "created the worktree") {
		t.Errorf("worktree without docker: %q", out)
	}
}

func TestIsolate_Unknown(t *testing.T) {
	if out := runIsolateCmd(nil, "sideways"); !strings.Contains(out, `unknown "sideways"`) || !strings.Contains(out, "/isolate off") {
		t.Errorf("%q", out)
	}
}

// /docker and /worktree new still work, and each says /isolate once.
func TestIsolate_Aliases(t *testing.T) {
	dockerAvailable(t, nil)
	repo := isoRepo(t)
	h := isoHarness(t, repo)

	out := ctl(h, "docker", "status")
	if first := strings.SplitN(out, "\n", 2)[0]; first != "/docker is /isolate now" || !strings.Contains(out, "isolate: none") {
		t.Errorf("/docker: %q", out)
	}
	out = ctl(h, "docker", "on")
	if !strings.HasPrefix(out, "/docker on is /isolate docker now") || isolationMode(h) != dun.IsoDocker || h.Workspace() == repo {
		t.Errorf("/docker on: %q", out)
	}
	out = ctl(h, "docker", "off")
	if !strings.HasPrefix(out, "/docker off is /isolate off now\n") || isolationMode(h) != dun.IsoNone || h.Workspace() != repo {
		t.Errorf("/docker off: %q", out)
	}
	out = ctl(h, "worktree", "new")
	if !strings.HasPrefix(out, "/worktree new is /isolate worktree now\n") || isolationMode(h) != dun.IsoWorktree {
		t.Errorf("/worktree new: %q", out)
	}
	// /worktree's own actions are not aliases.
	if out := ctl(h, "worktree", ""); strings.Contains(out, "/isolate") || !strings.Contains(out, "worktree: ") {
		t.Errorf("/worktree status: %q", out)
	}
	if out := ctl(h, "docker", "sideways"); !strings.Contains(out, "/docker is /isolate now") {
		t.Errorf("/docker sideways: %q", out)
	}
}

// Every client types the same words: routeSlash hands them to the engine.
func TestIsolate_RouteSlash(t *testing.T) {
	var got []string
	r := slashRoutes{control: func(a, b string) { got = append(got, a+" "+b) }, reply: func(string) {}}
	for _, line := range []string{"isolate", "/isolate DOCKER", "isolate off", "docker on", "worktree new"} {
		routeSlash(line, r)
	}
	want := []string{"isolate ", "isolate docker", "isolate off", "docker on", "worktree new"}
	if !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

// /isolate is listed, with its choices described; /docker and /worktree new
// are not.
func TestIsolate_Palette(t *testing.T) {
	cmds := engineCmdsNow(t)
	var iso, wt *engineCmd
	for i := range cmds {
		switch cmds[i].name {
		case "isolate":
			iso = &cmds[i]
		case "worktree":
			wt = &cmds[i]
		case "docker":
			t.Error("/docker is listed")
		}
	}
	if iso == nil || wt == nil {
		t.Fatalf("isolate %v, worktree %v", iso, wt)
	}
	var vals []string
	for _, c := range iso.choices {
		vals = append(vals, c.value)
		if c.desc == "" || strings.Contains(c.desc, "\n") {
			t.Errorf("%s: %q", c.value, c.desc)
		}
	}
	if !slices.Equal(vals, []string{"none", "worktree", "docker", "off"}) {
		t.Errorf("choices %v", vals)
	}
	if hasChoice(*wt, "new") {
		t.Error("/worktree new is in the palette")
	}

	m, _ := skillModel(t, 0)
	m.engineCmds = cmds
	m = typeStr(m, "/isolate d")
	if _, l, ok := m.paletteArgs(); !ok || len(l.choices) == 0 || l.choices[0].value != "docker" {
		t.Fatalf("/isolate d: %+v", l)
	}
	m.input.Reset()
	m = typeStr(m, "/dock")
	for _, c := range m.paletteMatches() {
		if c.name == "docker" {
			t.Error("/dock lists /docker")
		}
	}
	m.input.Reset()
	m = typeStr(m, "/isol")
	if ms := m.paletteMatches(); len(ms) == 0 || ms[0].name != "isolate" {
		t.Errorf("/isol: %v", ms)
	}
	m.input.Reset()
	m.showHelp(false)
	if help := stripANSI(m.convo[len(m.convo)-1].collapsed); !strings.Contains(help, "/isolate [none|worktree|docker|off]") {
		t.Errorf("/help lacks /isolate:\n%s", help)
	}
}

// A move tells every client: the command list with /isolate's state, and
// where the session is (an empty branch back in place).
func TestIsolate_Events(t *testing.T) {
	dockerAvailable(t, nil)
	repo := isoRepo(t)
	h := isoHarness(t, repo)
	step := func(action string) []any {
		ws, iso := h.Workspace(), isolationMode(h)
		ctl(h, "isolate", action)
		return isolateEvents(h, ws, iso)
	}
	state := func(evs []any) string {
		for _, e := range evs {
			if c, ok := e.(*proto.Commands); ok {
				for _, s := range c.Items {
					if s.Name == "isolate" {
						return s.State
					}
				}
			}
		}
		return ""
	}
	where := func(evs []any) *proto.Workspace {
		for _, e := range evs {
			if w, ok := e.(*proto.Workspace); ok {
				return w
			}
		}
		return nil
	}
	evs := step("worktree")
	if w := where(evs); state(evs) != "worktree" || w == nil || w.Path != h.Workspace() || w.Branch != h.Worktree().Branch {
		t.Errorf("worktree: %q %+v", state(evs), w)
	}
	evs = step("docker") // same tree: no workspace event
	if state(evs) != "docker" || where(evs) != nil {
		t.Errorf("docker: %q %+v", state(evs), where(evs))
	}
	evs = step("off")
	if w := where(evs); state(evs) != "none" || w == nil || w.Path != repo || w.Branch != "" {
		t.Errorf("off: %q %+v", state(evs), w)
	}
	if evs := step("off"); len(evs) != 0 {
		t.Errorf("no change, no events: %v", evs)
	}
}

// The TUI says nothing for an empty branch (back in place) and drops the ⎇.
func TestIsolate_TUIWorkspaceEvent(t *testing.T) {
	m, _ := skillModel(t, 0)
	m = m.handleEvent(evMsg{"type": "workspace", "path": "/wt", "branch": "dun/1-x"})
	n := len(m.convo)
	if m.branch != "dun/1-x" || !strings.Contains(stripANSI(m.convo[n-1].collapsed), "worktree branch: dun/1-x") {
		t.Fatalf("branch %q", m.branch)
	}
	m = m.handleEvent(evMsg{"type": "workspace", "path": "/ws", "branch": ""})
	if m.branch != "" || len(m.convo) != n {
		t.Errorf("back in place: branch %q, %d lines (was %d)", m.branch, len(m.convo), n)
	}
}
