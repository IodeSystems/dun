package harness

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iodesystems/dun/internal/mcpserver"
	"github.com/iodesystems/dun/internal/project"
)

// repoClean fails the test when dun left anything in the repo: the whole point
// of the project state dir is that a repo dun works on stays as it was.
func repoClean(t *testing.T, repo string) {
	t.Helper()
	if out, _ := git("", "-C", repo, "status", "--porcelain", "--ignored"); strings.TrimSpace(out) != "" {
		t.Errorf("dun wrote into the repo:\n%s", out)
	}
}

// A git worktree — here one made by hand, outside the repo, as Claude Code
// does — is the SOURCE repo's project: it reads the project's state, and its
// own overlay wins over it.
func TestProject_GitWorktreeMapsToSource_OverlayWins(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	repo := pruneRepo(t)
	wt := filepath.Join(t.TempDir(), "feature")
	gitrun(t, repo, "worktree", "add", "-q", "-b", "feature", wt)

	src, err := project.Ensure(repo)
	if err != nil {
		t.Fatal(err)
	}
	w, err := project.Ensure(wt)
	if err != nil {
		t.Fatal(err)
	}
	if w.ID != src.ID || w.StateDir != src.StateDir {
		t.Fatalf("worktree project %q, want the source's %q", w.ID, src.ID)
	}
	if w.OverlayDir == "" || src.OverlayDir != "" {
		t.Fatalf("only the linked worktree has an overlay: src %q, wt %q", src.OverlayDir, w.OverlayDir)
	}
	// Project: docs on, and a db server. Overlay: docs off.
	writeJSON(t, src.StateDir, project.LocalServersFile, `{"servers":[{"id":"docs","autostart":true},{"id":"db","command":"dbtool"}]}`)
	writeJSON(t, w.OverlayDir, project.LocalServersFile, `{"servers":[{"id":"docs","autostart":false}]}`)

	inRepo, err := mcpserver.LoadServers(repo, "/ws", "/rag")
	if err != nil {
		t.Fatal(err)
	}
	inWt, err := mcpserver.LoadServers(wt, "/ws", "/rag")
	if err != nil {
		t.Fatal(err)
	}
	if !byID(inRepo)["raglit"].Autostart {
		t.Error("the source reads the project's state")
	}
	if byID(inWt)["raglit"].Autostart {
		t.Error("the worktree's overlay must win over the project's state")
	}
	if byID(inWt)["db"].Command != "dbtool" {
		t.Error("the worktree must still see the project's state under the overlay")
	}
	// A /rag auto in the worktree is that worktree's.
	path, err := setAutostart(wt, "raglit", true)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(path) != w.OverlayDir {
		t.Errorf("a linked worktree's state goes to its overlay, got %s", path)
	}
	repoClean(t, repo)
	repoClean(t, wt)
}

// Precedence: built-in < repo dun.json < repo .dun/dun.json < project state.
// A repo's committed .dun/dun.json is READ, and dun never writes to it.
func TestProject_ConfigPrecedence_CommittedRepoFilesReadNotWritten(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	repo := pruneRepo(t)
	if err := os.MkdirAll(filepath.Join(repo, project.DunDir), 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, repo, project.ProjectServersFile, `{"servers":[{"id":"db","command":"root-db","args":["a"]}]}`)
	writeJSON(t, filepath.Join(repo, project.DunDir), project.ProjectServersFile, `{"servers":[{"id":"db","command":"dotdun-db"}]}`)
	gitrun(t, repo, "add", ".")
	gitrun(t, repo, "commit", "-qm", "dun config")
	committed, _ := os.ReadFile(filepath.Join(repo, project.DunDir, project.ProjectServersFile))

	got, err := mcpserver.LoadServers(repo, "/ws", "/rag")
	if err != nil {
		t.Fatal(err)
	}
	if db := byID(got)["db"]; db.Command != "dotdun-db" || len(db.Args) != 1 {
		t.Fatalf(".dun/dun.json must win over dun.json and inherit its args, got %+v", db)
	}

	p, err := project.Ensure(repo)
	if err != nil {
		t.Fatal(err)
	}
	writeJSON(t, p.StateDir, project.ProjectServersFile, `{"servers":[{"id":"db","command":"state-db"}]}`)
	if _, err := setAutostart(repo, "db", true); err != nil {
		t.Fatal(err)
	}
	got, err = mcpserver.LoadServers(repo, "/ws", "/rag")
	if err != nil {
		t.Fatal(err)
	}
	if db := byID(got)["db"]; db.Command != "state-db" || !db.Autostart {
		t.Fatalf("the project state dir must win over the repo's files, got %+v", db)
	}
	now, _ := os.ReadFile(filepath.Join(repo, project.DunDir, project.ProjectServersFile))
	if string(now) != string(committed) {
		t.Errorf("the committed .dun/dun.json changed:\n%s", now)
	}
	repoClean(t, repo)
}

// The proof that moving session worktrees out of the repo costs nothing: a
// go.mod "replace => ../x" still builds inside one, through the mount symlink
// beside it, and the repo — go.mod included — is untouched.
func TestNewWorktree_OutsideRepo_RelativeReplaceBuilds(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain")
	}
	t.Setenv("DUN_HOME", t.TempDir())
	base := t.TempDir()
	x, app := filepath.Join(base, "x"), filepath.Join(base, "app")
	for name, body := range map[string]string{
		filepath.Join(x, "go.mod"):    "module example.com/x\n\ngo 1.21\n",
		filepath.Join(x, "x.go"):      "package x\n\nfunc Answer() int { return 42 }\n",
		filepath.Join(app, "go.mod"):  "module example.com/app\n\ngo 1.21\n\nrequire example.com/x v0.0.0\n\nreplace example.com/x => ../x\n",
		filepath.Join(app, "main.go"): "package main\n\nimport \"example.com/x\"\n\nfunc main() { println(x.Answer()) }\n",
	} {
		if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitrun(t, app, "init", "-q")
	gitrun(t, app, "config", "user.email", "t@t")
	gitrun(t, app, "config", "user.name", "t")
	gitrun(t, app, "add", ".")
	gitrun(t, app, "commit", "-qm", "init")

	wt, isRepo, err := NewWorktree(app, mcpserver.LoadMounts(app, app))
	if err != nil || !isRepo {
		t.Fatalf("NewWorktree: isRepo=%v err=%v", isRepo, err)
	}
	defer wt.Cleanup()
	p := project.Resolve(app)
	if p == nil || filepath.Dir(wt.Path) != p.WorktreesDir() {
		t.Fatalf("worktree %s is not in the project's worktrees dir", wt.Path)
	}

	build := exec.Command("go", "build", "-o", os.DevNull, ".")
	build.Dir = wt.Path
	build.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod", "GOTOOLCHAIN=local", "GOPROXY=off")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build in the session worktree: %v\n%s", err, out)
	}
	gomod, _ := os.ReadFile(filepath.Join(wt.Path, "go.mod"))
	if !strings.Contains(string(gomod), "=> ../x") {
		t.Errorf("the worktree's go.mod must not be rewritten:\n%s", gomod)
	}
	repoClean(t, app)
}

// Worktrees made before the move, in <repo>/.dun/worktrees/, are still listed
// and pruned beside the new ones; a tree a human made elsewhere is not.
func TestScanWorktrees_OldAndNewLocations(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	repo := pruneRepo(t)
	old := addWorktree(t, repo, "old-layout")
	wt, _, err := NewWorktree(repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	human := filepath.Join(t.TempDir(), "mine")
	gitrun(t, repo, "worktree", "add", "-q", "-b", "mine", human)

	seen := map[string]bool{}
	for _, w := range ScanWorktrees(repo) {
		seen[project.RealPath(w.Path)] = true
		backdate(t, w.Path)
	}
	if !seen[project.RealPath(old)] || !seen[project.RealPath(wt.Path)] {
		t.Fatalf("both layouts must be listed, got %v", seen)
	}
	if seen[project.RealPath(human)] {
		t.Fatal("a worktree dun did not make must never be listed")
	}
	res := PruneWorktrees(repo, "")
	if len(res.Pruned) != 2 {
		t.Fatalf("both empty session trees should be pruned, got %+v", res.Pruned)
	}
	if _, err := os.Stat(human); err != nil {
		t.Fatal("the human's worktree must survive a prune")
	}
}
