package dun

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func waitIdle(t *testing.T, sa *subAgent) {
	t.Helper()
	select {
	case <-sa.waitCh():
	case <-time.After(10 * time.Second):
		t.Fatal("the child did not settle")
	}
}

// isolation worktree: the child works in its own worktree, is told so, and
// its report names the branch and its commits; a dismissed child's worktree
// is kept when it holds work and removed when it holds none. A workspace
// that is not a repository is refused.
func TestSubAgent_WorktreeIsolation(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	repo := t.TempDir()
	gitT(t, repo, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitT(t, repo, "add", ".")
	gitT(t, repo, "commit", "-qm", "base")
	run := &scriptRunner{}
	h, err := Start(context.Background(), Config{Workspace: repo, Client: run, Exec: &HostShell{Dir: repo}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	sa, err := h.spawnAgent(context.Background(), "edit a.txt", "", IsoWorktree)
	if err != nil {
		t.Fatal(err)
	}
	waitIdle(t, sa)
	if sa.iso.Path == "" || sa.iso.Path == repo || sa.h.cfg.Workspace != sa.iso.Path {
		t.Fatalf("iso %+v, child workspace %q", sa.iso, sa.h.cfg.Workspace)
	}
	if hs, ok := sa.h.cfg.Exec.(*HostShell); !ok || hs.Dir != sa.iso.Path {
		t.Errorf("child exec %#v", sa.h.cfg.Exec)
	}
	if got := run.lastSent(); !strings.Contains(got[len(got)-1].Content, "[isolation] You work in your own git worktree") {
		t.Errorf("the child was not told: %q", got[len(got)-1].Content)
	}
	if err := os.WriteFile(filepath.Join(sa.iso.Path, "a.txt"), []byte("b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitT(t, sa.iso.Path, "commit", "-qam", "child's edit")
	rep := sa.report()
	if !strings.Contains(rep, "branch "+sa.iso.Branch+": 1 commit(s)") || !strings.Contains(rep, "git merge "+sa.iso.Branch) {
		t.Errorf("report:\n%s", rep)
	}
	if b, _ := os.ReadFile(filepath.Join(repo, "a.txt")); string(b) != "a\n" {
		t.Errorf("the parent's tree changed: %q", b)
	}
	if out := sa.quit(); !strings.Contains(out, "worktree is kept") {
		t.Errorf("quit: %s", out)
	}
	if _, err := os.Stat(sa.iso.Path); err != nil {
		t.Errorf("a worktree with work was removed: %v", err)
	}

	empty, err := h.spawnAgent(context.Background(), "look only", "", IsoWorktree)
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

	plain, err := Start(context.Background(), Config{Workspace: t.TempDir(), Client: run})
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	if _, err := plain.spawnAgent(context.Background(), "x", "", IsoWorktree); err == nil || !strings.Contains(err.Error(), "needs a git repository") {
		t.Errorf("a non-repository: %v", err)
	}
	if _, err := plain.spawnAgent(context.Background(), "x", "", "sideways"); err == nil {
		t.Error("an unknown isolation was taken")
	}
}

// isolation nodir: no file tools, exec in a scratch directory, removed
// when the child is dismissed.
func TestSubAgent_NoDirIsolation(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws := t.TempDir()
	h, err := Start(context.Background(), Config{Workspace: ws, Client: &scriptRunner{}, Exec: &HostShell{Dir: ws}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	sa, err := h.spawnAgent(context.Background(), "summarize", "", IsoNoDir)
	if err != nil {
		t.Fatal(err)
	}
	waitIdle(t, sa)
	c := sa.h.cfg
	if !c.NoDir || c.Files == nil || len(c.Files.Tools) != 0 || c.Workspace != sa.iso.Path || c.Workspace == ws {
		t.Fatalf("child config: nodir %v files %+v ws %q", c.NoDir, c.Files, c.Workspace)
	}
	for _, d := range sa.h.Session.Tools {
		if strings.HasPrefix(d.Function.Name, "file_") {
			t.Errorf("a nodir child has %s", d.Function.Name)
		}
	}
	sa.quit()
	if _, err := os.Stat(sa.iso.Path); !os.IsNotExist(err) {
		t.Errorf("the scratch directory was kept: %v", err)
	}
}

// A resumed child goes back to its own tree.
func TestIsolation_SavedBesideTheTranscript(t *testing.T) {
	dir := t.TempDir()
	sf := filepath.Join(dir, "s.sub1.jsonl")
	iso := isolation{Mode: IsoWorktree, Path: "/w", Branch: "dun/1", Base: "abc", Repo: "/r"}
	saveIsolation(sf, iso)
	if got := loadIsolation(sf); got != iso {
		t.Errorf("got %+v", got)
	}
	if got := loadIsolation(filepath.Join(dir, "s.sub2.jsonl")); got.isolated() {
		t.Errorf("a child with none: %+v", got)
	}
}
