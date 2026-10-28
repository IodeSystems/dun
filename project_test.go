package dun

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// repoClean fails the test when dun left anything in the repo: the whole point
// of the project state dir is that a repo dun works on stays as it was.
func repoClean(t *testing.T, repo string) {
	t.Helper()
	if out, _ := git("", "-C", repo, "status", "--porcelain", "--ignored"); strings.TrimSpace(out) != "" {
		t.Errorf("dun wrote into the repo:\n%s", out)
	}
}

// First sight mints an id (the directory's name plus a random suffix) and
// saves it; every later sight gets the same one. Reading never registers.
func TestProject_MintThenReuse(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	dir := filepath.Join(t.TempDir(), "My Repo")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if ResolveProject(dir) != nil {
		t.Fatal("a directory dun has never seen has no project")
	}
	if _, err := os.Stat(registryFile()); !os.IsNotExist(err) {
		t.Fatal("ResolveProject must not create the registry")
	}
	p, err := EnsureProject(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(p.ID, "my-repo-") || len(p.ID) != len("my-repo-")+6 {
		t.Errorf("id = %q, want my-repo-<6 hex>", p.ID)
	}
	if st, err := os.Stat(p.StateDir); err != nil || !st.IsDir() {
		t.Fatalf("state dir not created: %v", err)
	}
	again, err := EnsureProject(dir)
	if err != nil || again.ID != p.ID {
		t.Fatalf("second sight got %q (%v), want %q", again.ID, err, p.ID)
	}
	if r := ResolveProject(dir); r == nil || r.ID != p.ID {
		t.Fatalf("ResolveProject = %+v, want id %q", r, p.ID)
	}
	var reg registry
	b, _ := os.ReadFile(registryFile())
	if err := json.Unmarshal(b, &reg); err != nil || reg.Projects[realPath(dir)] != p.ID {
		t.Fatalf("registry %s does not map the dir: %v", b, err)
	}
}

// The registry is written temp+rename under a lock: many first sights at once
// must all survive, and no temp file is left behind.
func TestProject_RegistryConcurrentMint(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	base := t.TempDir()
	const n = 16
	ids := make([]string, n)
	var wg sync.WaitGroup
	for i := range n {
		dir := filepath.Join(base, fmt.Sprintf("p%d", i))
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, err := EnsureProject(dir)
			if err != nil {
				t.Error(err)
				return
			}
			ids[i] = p.ID
		}()
	}
	wg.Wait()
	reg, err := readRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if len(reg.Projects) != n {
		t.Fatalf("registry holds %d projects, want %d — a concurrent write was lost", len(reg.Projects), n)
	}
	for i := range n {
		if got := reg.Projects[realPath(filepath.Join(base, fmt.Sprintf("p%d", i)))]; got != ids[i] {
			t.Errorf("p%d: registry %q, minted %q", i, got, ids[i])
		}
	}
	left, _ := filepath.Glob(filepath.Join(DunHome(), ".projects.json.tmp-*"))
	if len(left) != 0 {
		t.Errorf("temp files left behind: %v", left)
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

	src, err := EnsureProject(repo)
	if err != nil {
		t.Fatal(err)
	}
	w, err := EnsureProject(wt)
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
	writeJSON(t, src.StateDir, LocalServersFile, `{"servers":[{"id":"docs","autostart":true},{"id":"db","command":"dbtool"}]}`)
	writeJSON(t, w.OverlayDir, LocalServersFile, `{"servers":[{"id":"docs","autostart":false}]}`)

	inRepo, err := LoadServers(repo, "/ws", "/rag")
	if err != nil {
		t.Fatal(err)
	}
	inWt, err := LoadServers(wt, "/ws", "/rag")
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
	if err := os.MkdirAll(filepath.Join(repo, DunDir), 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, repo, ProjectServersFile, `{"servers":[{"id":"db","command":"root-db","args":["a"]}]}`)
	writeJSON(t, filepath.Join(repo, DunDir), ProjectServersFile, `{"servers":[{"id":"db","command":"dotdun-db"}]}`)
	gitrun(t, repo, "add", ".")
	gitrun(t, repo, "commit", "-qm", "dun config")
	committed, _ := os.ReadFile(filepath.Join(repo, DunDir, ProjectServersFile))

	got, err := LoadServers(repo, "/ws", "/rag")
	if err != nil {
		t.Fatal(err)
	}
	if db := byID(got)["db"]; db.Command != "dotdun-db" || len(db.Args) != 1 {
		t.Fatalf(".dun/dun.json must win over dun.json and inherit its args, got %+v", db)
	}

	p, err := EnsureProject(repo)
	if err != nil {
		t.Fatal(err)
	}
	writeJSON(t, p.StateDir, ProjectServersFile, `{"servers":[{"id":"db","command":"state-db"}]}`)
	if _, err := setAutostart(repo, "db", true); err != nil {
		t.Fatal(err)
	}
	got, err = LoadServers(repo, "/ws", "/rag")
	if err != nil {
		t.Fatal(err)
	}
	if db := byID(got)["db"]; db.Command != "state-db" || !db.Autostart {
		t.Fatalf("the project state dir must win over the repo's files, got %+v", db)
	}
	now, _ := os.ReadFile(filepath.Join(repo, DunDir, ProjectServersFile))
	if string(now) != string(committed) {
		t.Errorf("the committed .dun/dun.json changed:\n%s", now)
	}
	repoClean(t, repo)
}

// An older dun's .dun/dun.local.json and agents_read are COPIED in on first
// sight — the original stays — and an import never overwrites state that is
// already there.
func TestProject_ImportsLegacyState(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	repo := pruneRepo(t)
	legacy := filepath.Join(repo, DunDir)
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, legacy, LocalServersFile, `{"servers":[{"id":"docs","autostart":true}]}`)
	writeJSON(t, legacy, agentsReadName, "/x/AGENTS.md 1\n")

	p, err := EnsureProject(repo)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range legacyState {
		b, err := os.ReadFile(filepath.Join(p.StateDir, name))
		if err != nil {
			t.Fatalf("%s was not imported: %v", name, err)
		}
		orig, err := os.ReadFile(filepath.Join(legacy, name))
		if err != nil {
			t.Fatalf("the repo's %s must not be removed: %v", name, err)
		}
		if string(b) != string(orig) {
			t.Errorf("%s imported as %q, want %q", name, b, orig)
		}
	}
	got, _ := LoadServers(repo, "/ws", "/rag")
	if !byID(got)["raglit"].Autostart {
		t.Error("the imported setting must take effect")
	}
	// Turned off after the move: that is the state now, and a second sight
	// must not import the repo's stale copy over it.
	if _, err := setAutostart(repo, "raglit", false); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureProject(repo); err != nil {
		t.Fatal(err)
	}
	got, _ = LoadServers(repo, "/ws", "/rag")
	if byID(got)["raglit"].Autostart {
		t.Error("the project's own state must win over the repo's old copy")
	}
}

// The go.mod replace shapes: the block form is read, "../x/sub" needs x beside
// the worktree (not sub), a target inside the repo needs nothing.
func TestDiscoverGoModReplaces_Shapes(t *testing.T) {
	dir := t.TempDir()
	gomod := "module example.com/app\n\ngo 1.22\n\n" +
		"replace example.com/a => ../a // a comment\n" +
		"replace (\n" +
		"\texample.com/b v1.0.0 => ../mono/b\n" +
		"\texample.com/c => ./internal/c\n" +
		"\texample.com/d => ../../d\n" +
		"\texample.com/e => example.com/e2 v1.0.0\n" +
		"\texample.com/f => ../mono/f\n" +
		")\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(gomod), 0o644); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, m := range discoverGoModReplaces(dir, dir) {
		if _, dup := got[m.Name]; dup {
			t.Errorf("mount %q listed twice", m.Name)
		}
		got[m.Name] = m.Source
	}
	want := map[string]string{"a": "../a", "mono": "../mono", "d": "../../d"}
	if len(got) != len(want) {
		t.Fatalf("mounts = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("mount %q = %q, want %q", k, got[k], v)
		}
	}
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

	wt, isRepo, err := NewWorktree(app, LoadMounts(app, app))
	if err != nil || !isRepo {
		t.Fatalf("NewWorktree: isRepo=%v err=%v", isRepo, err)
	}
	defer wt.Cleanup()
	p := ResolveProject(app)
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
		seen[realPath(w.Path)] = true
		backdate(t, w.Path)
	}
	if !seen[realPath(old)] || !seen[realPath(wt.Path)] {
		t.Fatalf("both layouts must be listed, got %v", seen)
	}
	if seen[realPath(human)] {
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
