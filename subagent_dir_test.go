package dun

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// spawnAgentIn is spawnAgent with a dir.
func (h *Harness) spawnAgentIn(ctx context.Context, prompt, model, mode, dir string) (*subAgent, error) {
	return h.spawnAgentFor(ctx, prompt, "", model, "", mode, dir, "")
}

// gitRepoT is a repository at dir with one commit, holding sub/b.txt.
func gitRepoT(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	gitT(t, dir, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "sub", "b.txt"), []byte("b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitT(t, dir, "add", ".")
	gitT(t, dir, "commit", "-qm", "base")
	return dir
}

// dir with isolation worktree (USER 2026-11-17): a parent whose workspace is
// not a repository branches the worktree from dir's repo, the merge hint
// names that repo, and an empty one is removed from it on dismiss.
func TestSubAgent_DirWorktreeFromTheDirsRepo(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	over := t.TempDir()
	repo := gitRepoT(t, filepath.Join(over, "proj"))
	run := &scriptRunner{}
	h, err := Start(context.Background(), Config{Workspace: over, Client: run, Exec: &HostShell{Dir: over}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	sa, err := h.spawnAgentIn(context.Background(), "edit", "", IsoWorktree, "proj")
	if err != nil {
		t.Fatal(err)
	}
	waitIdle(t, sa)
	if sa.iso.Repo != repo || sa.iso.Sub != "" || !sa.iso.Foreign || sa.h.cfg.Workspace != sa.iso.Path {
		t.Fatalf("iso %+v, child workspace %q", sa.iso, sa.h.cfg.Workspace)
	}
	if br := gitT(t, repo, "branch", "--list", sa.iso.Branch); br == "" {
		t.Errorf("branch %s is not in %s", sa.iso.Branch, repo)
	}
	if err := os.WriteFile(filepath.Join(sa.iso.Path, "c.txt"), []byte("c\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitT(t, sa.iso.Path, "add", ".")
	gitT(t, sa.iso.Path, "commit", "-qm", "child's file")
	if rep := sa.report(); !strings.Contains(rep, "git -C "+repo+" merge "+sa.iso.Branch) {
		t.Errorf("report:\n%s", rep)
	}

	empty, err := h.spawnAgentIn(context.Background(), "look", "", IsoWorktree, repo)
	if err != nil {
		t.Fatal(err)
	}
	waitIdle(t, empty)
	if out := empty.quit(); !strings.Contains(out, "held nothing and was removed") {
		t.Errorf("quit: %s", out)
	}
	if _, err := os.Stat(empty.iso.Path); !os.IsNotExist(err) {
		t.Errorf("an empty worktree was kept: %v", err)
	}
	if br := gitT(t, repo, "branch", "--list", empty.iso.Branch); br != "" {
		t.Errorf("its branch was kept: %q", br)
	}
}

// dir in a subdirectory of a repo: the child works in the same subdirectory
// of its worktree, and is told so.
func TestSubAgent_DirSubdirOfARepo(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	over := t.TempDir()
	repo := gitRepoT(t, filepath.Join(over, "proj"))
	run := &scriptRunner{}
	h, err := Start(context.Background(), Config{Workspace: over, Client: run, Exec: &HostShell{Dir: over}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	sa, err := h.spawnAgentIn(context.Background(), "edit", "", IsoWorktree, filepath.Join(repo, "sub"))
	if err != nil {
		t.Fatal(err)
	}
	waitIdle(t, sa)
	want := filepath.Join(sa.iso.Path, "sub")
	if sa.iso.Sub != "sub" || sa.h.cfg.Workspace != want {
		t.Fatalf("iso %+v, child workspace %q", sa.iso, sa.h.cfg.Workspace)
	}
	if hs, ok := sa.h.cfg.Exec.(*HostShell); !ok || hs.Dir != want {
		t.Errorf("child exec %#v", sa.h.cfg.Exec)
	}
	if got := run.lastSent(); !strings.Contains(got[len(got)-1].Content, "Your directory is "+want) {
		t.Errorf("the child was not told: %q", got[len(got)-1].Content)
	}
	if d := (DockerExec{Dir: sa.iso.Path, Sub: sa.iso.Sub}).runArgs("n", "true"); !slices.Contains(d, "/work/sub") {
		t.Errorf("docker workdir: %v", d)
	}
	sa.quit()
}

// dir without isolation: the child works there in place.
func TestSubAgent_DirInPlace(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws, dir := t.TempDir(), t.TempDir()
	run := &scriptRunner{}
	h, err := Start(context.Background(), Config{Workspace: ws, Client: run, Exec: &HostShell{Dir: ws}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	sa, err := h.spawnAgentIn(context.Background(), "look", "", "", dir)
	if err != nil {
		t.Fatal(err)
	}
	waitIdle(t, sa)
	if sa.h.cfg.Workspace != dir {
		t.Fatalf("child workspace %q, want %q", sa.h.cfg.Workspace, dir)
	}
	if hs, ok := sa.h.cfg.Exec.(*HostShell); !ok || hs.Dir != dir {
		t.Errorf("child exec %#v", sa.h.cfg.Exec)
	}
	if got := run.lastSent(); !strings.Contains(got[len(got)-1].Content, "You work directly in "+dir) {
		t.Errorf("the child was not told: %q", got[len(got)-1].Content)
	}
	if _, err := h.spawnAgentIn(context.Background(), "x", "", "", filepath.Join(dir, "missing")); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("a missing dir: %v", err)
	}
	if _, err := h.spawnAgentIn(context.Background(), "x", "", IsoNoDir, dir); err == nil {
		t.Error("nodir took a dir")
	}
}

// A worktree for a dir in no repository is refused; one holding repos
// names them.
func TestSubAgent_DirNotARepo(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	over := t.TempDir()
	h, err := Start(context.Background(), Config{Workspace: over, Client: &scriptRunner{}, Exec: &HostShell{Dir: over}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if _, err := h.spawnAgentIn(context.Background(), "x", "", IsoWorktree, over); err == nil || !strings.Contains(err.Error(), "is not in a git repository") {
		t.Errorf("no repo: %v", err)
	}
	a := gitRepoT(t, filepath.Join(over, "a"))
	b := gitRepoT(t, filepath.Join(over, "group", "b"))
	_, err = h.spawnAgentIn(context.Background(), "x", "", IsoWorktree, over)
	if err == nil || !strings.Contains(err.Error(), a) || !strings.Contains(err.Error(), b) {
		t.Errorf("two repos below: %v", err)
	}
}

// dir is a spawn's: with id it is refused, unless it is where the agent
// already works. The spawn result names the directory.
func TestAgentTool_DirIsASpawns(t *testing.T) {
	h := startParentOf(t, &scriptRunner{})
	dir := t.TempDir()
	planCall(t, h, "plan_add", `{"title":"scan","done_when":"x"}`)
	out := callTool(t, h, "agent", map[string]any{"node": planIDs(h)["scan"], "dir": dir})
	if !strings.Contains(out, "in "+dir+", in place") {
		t.Fatalf("spawn result: %q", out)
	}
	if out := callTool(t, h, "agent", map[string]any{"id": 1, "dir": t.TempDir()}); !strings.HasPrefix(out, "ERROR: dir is a new agent's") {
		t.Errorf("another dir: %q", out)
	}
	if out := callTool(t, h, "agent", map[string]any{"id": 1, "dir": dir}); strings.HasPrefix(out, "ERROR") {
		t.Errorf("its own dir: %q", out)
	}
}

// A resumed child goes back to the same worktree, in its subdirectory.
func TestSubAgent_DirWorktreeResumes(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	over := t.TempDir()
	repo := gitRepoT(t, filepath.Join(over, "proj"))
	sf := filepath.Join(t.TempDir(), "s.jsonl")
	runners := map[string]*scriptRunner{"a": {rounds: []roundFn{textRound("ok")}}}
	h := startWith(t, over, sf, runners, true)
	sa, err := h.spawnAgentIn(context.Background(), "edit", "a", IsoWorktree, filepath.Join(repo, "sub"))
	if err != nil {
		t.Fatal(err)
	}
	waitIdle(t, sa)
	was := sa.h.cfg.Workspace
	h.Close()

	h = startWith(t, over, sf, runners, true)
	defer h.Close()
	one := h.agentByID(1)
	if one == nil || one.harness() == nil {
		t.Fatal("#1 did not come back")
	}
	if got := one.harness().cfg.Workspace; got != was || one.iso.Sub != "sub" {
		t.Errorf("resumed in %q (iso %+v), was %q", got, one.iso, was)
	}
}
