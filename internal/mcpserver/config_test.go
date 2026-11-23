package mcpserver

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/iodesystems/dun/internal/oauth"
	"github.com/iodesystems/dun/internal/project"
)

func writeJSON(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func byID(servers []Server) map[string]Server {
	m := map[string]Server{}
	for _, s := range servers {
		m[s.ID] = s
	}
	return m
}

// No config files: the built-in trio is a working default, and most projects
// should never need to write a config at all.
func TestLoadServers_DefaultsWithNoFiles(t *testing.T) {
	got, err := LoadServers(t.TempDir(), "/ws", "/rag")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("want the 3 built-ins, got %d: %v", len(got), IDs(got))
	}
	m := byID(got)
	if m["poly-lsp"].Command != "poly-lsp-mcp" {
		t.Errorf("poly-lsp server: %+v", m["poly-lsp"])
	}
	if !strings.Contains(strings.Join(m["poly-lsp"].Args, " "), "/ws") {
		t.Errorf("workspace not threaded into args: %v", m["poly-lsp"].Args)
	}
	// Default docs server should disable the tools the agent never uses (forget is
	// dun's, through the index role, like ingest).
	if m["raglit"].Disable == nil || len(m["raglit"].Disable) != 7 {
		t.Errorf("raglit server: expected 7 disabled tools, got %v", m["raglit"].Disable)
	}
}

// Overriding one binary's path must not require restating the other two — that
// is how a config silently forks from the defaults it copied.
func TestLoadServers_OverrideByIDKeepsSiblings(t *testing.T) {
	dir := t.TempDir()
	writeJSON(t, dir, project.LocalServersFile, `{"servers":[{"id":"code","command":"/opt/bin/poly-lsp-mcp"}]}`)
	got, err := LoadServers(dir, "/ws", "/rag")
	if err != nil {
		t.Fatal(err)
	}
	m := byID(got)
	if m["poly-lsp"].Command != "/opt/bin/poly-lsp-mcp" {
		t.Errorf("override did not take: %+v", m["poly-lsp"])
	}
	// Args were not restated, so they must be inherited, not cleared.
	if len(m["poly-lsp"].Args) == 0 {
		t.Error("omitted args should INHERIT, not clear")
	}
	if len(got) != 3 {
		t.Errorf("siblings must survive an override: %v", IDs(got))
	}
}

// A project adding a fourth server is the motivating case.
func TestLoadServers_AddsProjectServer(t *testing.T) {
	dir := t.TempDir()
	writeJSON(t, dir, project.ProjectServersFile,
		`{"servers":[{"id":"db","command":"db-mcp","args":["stdio","--root","{{workspace}}"],"env":["DSN=x"]}]}`)
	got, err := LoadServers(dir, "/ws", "/rag")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("want 4, got %v", IDs(got))
	}
	db := byID(got)["db"]
	if db.Args[2] != "/ws" {
		t.Errorf("{{workspace}} not expanded: %v", db.Args)
	}
	if len(db.Env) != 1 || db.Env[0] != "DSN=x" {
		t.Errorf("env not carried: %v", db.Env)
	}
}

// local layers OVER project — machine facts win over project defaults.
func TestLoadServers_LocalBeatsProject(t *testing.T) {
	dir := t.TempDir()
	writeJSON(t, dir, project.ProjectServersFile, `{"servers":[{"id":"db","command":"db-mcp","args":["stdio"]}]}`)
	writeJSON(t, dir, project.LocalServersFile, `{"servers":[{"id":"db","command":"/home/me/bin/db-mcp"}]}`)
	got, err := LoadServers(dir, "/ws", "/rag")
	if err != nil {
		t.Fatal(err)
	}
	db := byID(got)["db"]
	if db.Command != "/home/me/bin/db-mcp" {
		t.Errorf("local must win: %+v", db)
	}
	if len(db.Args) != 1 || db.Args[0] != "stdio" {
		t.Errorf("project args should survive a local command-only override: %v", db.Args)
	}
}

// Dropping a built-in should be one line, not a transcription of the rest.
func TestLoadServers_Disable(t *testing.T) {
	dir := t.TempDir()
	writeJSON(t, dir, project.ProjectServersFile, `{"servers":[{"id":"docs","disabled":true}]}`)
	got, err := LoadServers(dir, "/ws", "/rag")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("docs should be gone: %v", IDs(got))
	}
	for _, s := range got {
		if s.ID == "raglit" {
			t.Error("disabled server still present")
		}
	}
}

// A local file must not silently resurrect something the project turned off:
// re-enabling means removing the entry that disabled it.
func TestLoadServers_LocalCannotUndisable(t *testing.T) {
	dir := t.TempDir()
	writeJSON(t, dir, project.ProjectServersFile, `{"servers":[{"id":"docs","disabled":true}]}`)
	writeJSON(t, dir, project.LocalServersFile, `{"servers":[{"id":"docs","disabled":false}]}`)
	got, err := LoadServers(dir, "/ws", "/rag")
	if err != nil {
		t.Fatal(err)
	}
	if len(byID(got)) != 2 {
		t.Errorf("disabled must stay disabled: %v", IDs(got))
	}
}

// A malformed file must name ITSELF — in a two-file layered config, an
// unattributed JSON error sends the reader to the wrong file.
func TestLoadServers_ParseErrorNamesTheFile(t *testing.T) {
	dir := t.TempDir()
	writeJSON(t, dir, project.LocalServersFile, `{"servers":[{"id":`)
	_, err := LoadServers(dir, "/ws", "/rag")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), project.LocalServersFile) {
		t.Errorf("error should name the offending file: %v", err)
	}
}

// A new server with no command cannot be spawned; say so at load time rather
// than failing obscurely at exec.
func TestLoadServers_RejectsCommandlessServer(t *testing.T) {
	dir := t.TempDir()
	writeJSON(t, dir, project.ProjectServersFile, `{"servers":[{"id":"ghost"}]}`)
	if _, err := LoadServers(dir, "/ws", "/rag"); err == nil {
		t.Error("a server with no command should be rejected")
	}
}

// Only shell autostarts. code and docs cost real startup time and a machine
// may not even have their binaries — a session must not depend on them.
func TestLoadServers_OnlyShellAutostartsByDefault(t *testing.T) {
	got, err := LoadServers(t.TempDir(), "/ws", "/rag")
	if err != nil {
		t.Fatal(err)
	}
	m := byID(got)
	if !m["shell"].Autostart {
		t.Error("shell should autostart")
	}
	if m["poly-lsp"].Autostart || m["raglit"].Autostart {
		t.Errorf("poly-lsp/raglit must be opt-in: poly-lsp=%v raglit=%v", m["poly-lsp"].Autostart, m["raglit"].Autostart)
	}
}

// .dun/dun.local.json is where dun writes its own state, and it must win over
// the same file at the workspace root (the pre-.dun layout, still honored).
func TestLoadServers_DunDirLocalWinsOverRoot(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, project.DunDir), 0o700); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, dir, project.LocalServersFile, `{"servers":[{"id":"docs","autostart":true}]}`)
	writeJSON(t, filepath.Join(dir, project.DunDir), project.LocalServersFile, `{"servers":[{"id":"docs","autostart":false}]}`)
	got, err := LoadServers(dir, "/ws", "/rag")
	if err != nil {
		t.Fatal(err)
	}
	if byID(got)["raglit"].Autostart {
		t.Error(".dun/dun.local.json should have won")
	}
}

// setAutostart writes dir's project switch for id, as /mcp on --project does.
func setAutostart(dir, id string, on bool) (string, error) {
	return UpdateLocalServer(dir, id, func(s *Spec) { s.Autostart = &on })
}

// gitRepo is a repo with one commit.
func gitRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"config", "user.email", "t@t"}, {"config", "user.name", "t"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("hello\n"), 0o644)
	for _, args := range [][]string{{"add", "."}, {"commit", "-qm", "init"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return repo
}

// Turning autostart back OFF must be expressible, not require deleting the
// entry that turned it on — which is why Autostart is a pointer.
func TestSetAutostart_RoundTripBothWays(t *testing.T) {
	dir := t.TempDir()
	for _, want := range []bool{true, false, true} {
		path, err := setAutostart(dir, "raglit", want)
		if err != nil {
			t.Fatal(err)
		}
		if p := project.Resolve(dir); p == nil || filepath.Dir(path) != p.StateDir {
			t.Errorf("wrote outside the project state dir: %s", path)
		}
		got, err := LoadServers(dir, "/ws", "/rag")
		if err != nil {
			t.Fatal(err)
		}
		if byID(got)["raglit"].Autostart != want {
			t.Fatalf("autostart=%v did not survive a reload", want)
		}
	}
	// One entry, not one per call.
	f, err := ReadConfig(filepath.Join(project.Resolve(dir).StateDir, project.LocalServersFile))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Servers) != 1 {
		t.Errorf("want a single upserted entry, got %d", len(f.Servers))
	}
}

// The local file holds machine state and may hold secrets. It goes to the
// project's state dir, 0600, and NOTHING lands in the repo — it used to write
// .dun/dun.local.json and a .gitignore there, which asked every repo dun
// touched to carry dun's files.
func TestSetAutostart_WritesProjectDirNotRepo(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	repo := gitRepo(t)
	path, err := setAutostart(repo, "raglit", true)
	if err != nil {
		t.Fatal(err)
	}
	p := project.Resolve(repo)
	if p == nil || path != filepath.Join(p.StateDir, project.LocalServersFile) {
		t.Fatalf("wrote %s, want the project's %s", path, project.LocalServersFile)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("want 0600, got %v", st.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(repo, project.DunDir)); !os.IsNotExist(err) {
		t.Errorf("a project switch created %s in the repo", project.DunDir)
	}
	if out, _ := exec.Command("git", "-C", repo, "status", "--porcelain").Output(); strings.TrimSpace(string(out)) != "" {
		t.Errorf("the repo must stay clean, got:\n%s", out)
	}
}

// Auto-discovery: a go.mod with a local replace directive should produce a mount.
func TestDiscoverGoModReplaces_LocalReplace(t *testing.T) {
	dir := t.TempDir()
	gomod := "module example.com/project\n\ngo 1.22\n\nrequire github.com/iodesystems/agentkit v0.0.0\n\nreplace github.com/iodesystems/agentkit => ../agentkit\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(gomod), 0644); err != nil {
		t.Fatal(err)
	}
	mounts := discoverGoModReplaces(dir, dir)
	if len(mounts) != 1 {
		t.Fatalf("want 1 mount, got %d", len(mounts))
	}
	if mounts[0].Name != "agentkit" {
		t.Errorf("name = %q, want %q", mounts[0].Name, "agentkit")
	}
	if mounts[0].Source != "../agentkit" {
		t.Errorf("source = %q, want %q", mounts[0].Source, "../agentkit")
	}
}

// Auto-discovery: non-local replaces are ignored.
func TestDiscoverGoModReplaces_SkipsNonLocal(t *testing.T) {
	dir := t.TempDir()
	gomod := "module example.com/project\n\ngo 1.22\n\nreplace example.com/old => example.com/new v1.0.0\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(gomod), 0644); err != nil {
		t.Fatal(err)
	}
	mounts := discoverGoModReplaces(dir, dir)
	if len(mounts) != 0 {
		t.Errorf("want 0 mounts for non-local replace, got %d", len(mounts))
	}
}

// Auto-discovery: absolute local replaces are included.
func TestDiscoverGoModReplaces_AbsolutePath(t *testing.T) {
	dir := t.TempDir()
	gomod := "module example.com/project\n\ngo 1.22\n\nreplace example.com/lib => /opt/lib\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(gomod), 0644); err != nil {
		t.Fatal(err)
	}
	mounts := discoverGoModReplaces(dir, dir)
	if len(mounts) != 1 {
		t.Fatalf("want 1 mount, got %d", len(mounts))
	}
	if mounts[0].Name != "lib" {
		t.Errorf("name = %q, want %q", mounts[0].Name, "lib")
	}
	if mounts[0].Source != "/opt/lib" {
		t.Errorf("source = %q, want %q", mounts[0].Source, "/opt/lib")
	}
}

// LoadMounts: no config, no go.mod -> empty.
func TestLoadMounts_NoConfigNoGoMod(t *testing.T) {
	dir := t.TempDir()
	mounts := LoadMounts(dir, dir)
	if len(mounts) != 0 {
		t.Errorf("want 0 mounts, got %d", len(mounts))
	}
}

// LoadMounts: config mounts merge by name across layers.
func TestLoadMounts_MergeByName(t *testing.T) {
	dir := t.TempDir()
	writeJSON(t, dir, project.ProjectServersFile, `{"mounts":[{"source":"../shared","name":"shared"}]}`)
	writeJSON(t, dir, project.LocalServersFile, `{"mounts":[{"source":"/opt/shared","name":"shared"}]}`)
	mounts := LoadMounts(dir, dir)
	if len(mounts) != 1 {
		t.Fatalf("want 1 mount, got %d", len(mounts))
	}
	if mounts[0].Name != "shared" {
		t.Errorf("name = %q, want %q", mounts[0].Name, "shared")
	}
	if mounts[0].Source != "/opt/shared" {
		t.Errorf("source = %q, want %q", mounts[0].Source, "/opt/shared")
	}
}

// LoadMounts: relative sources are resolved against repoRoot.
func TestLoadMounts_RelativeSourceResolved(t *testing.T) {
	dir := t.TempDir()
	writeJSON(t, dir, project.ProjectServersFile, `{"mounts":[{"source":"../agentkit","name":"agentkit"}]}`)
	mounts := LoadMounts(dir, dir)
	if len(mounts) != 1 {
		t.Fatalf("want 1 mount, got %d", len(mounts))
	}
	want := filepath.Join(dir, "../agentkit")
	if mounts[0].Source != want {
		t.Errorf("source = %q, want %q", mounts[0].Source, want)
	}
}

// LoadMounts: auto-discovered go.mod mounts can be supplemented by config.
func TestLoadMounts_AutoAndConfig(t *testing.T) {
	dir := t.TempDir()
	gomod := "module example.com/project\n\ngo 1.22\n\nreplace github.com/iodesystems/agentkit => ../agentkit\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(gomod), 0644); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, dir, project.ProjectServersFile, `{"mounts":[{"source":"../tools","name":"tools"}]}`)
	mounts := LoadMounts(dir, dir)
	if len(mounts) != 2 {
		t.Fatalf("want 2 mounts, got %d", len(mounts))
	}
	if mounts[0].Name != "agentkit" {
		t.Errorf("first mount name = %q, want %q", mounts[0].Name, "agentkit")
	}
	if mounts[1].Name != "tools" {
		t.Errorf("second mount name = %q, want %q", mounts[1].Name, "tools")
	}
}

// dun used to point raglit at a per-session temp dir with --embedded: it
// re-embedded the whole workspace every session, threw the result away on exit,
// and opted out of the daemon that exists for exactly dun's shape. It also made
// durable memory impossible — a home deleted on exit cannot hold anything.
func TestRaglitProject_NamespacesByCheckoutNotBySession(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "My Project.v2")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}

	// No .raglit → the directory name, normalized the way raglit itself would.
	if got := RaglitProject(repo); got != "my-project-v2" {
		t.Fatalf("project = %q, want my-project-v2", got)
	}
	// Stable across sessions: that is the whole point.
	if RaglitProject(repo) != RaglitProject(repo+"/") {
		t.Error("a trailing separator must not change the index a session lands on")
	}

	// A checkout that has been `raglit init`ed declares its own name, and that
	// wins — splitting one repo across two indexes is the failure to avoid.
	if err := os.MkdirAll(filepath.Join(repo, ".raglit"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".raglit", "config.json"),
		[]byte(`{"project":"Declared Name"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := RaglitProject(repo); got != "declared-name" {
		t.Fatalf("a declared project must win, got %q", got)
	}

	// The docs server and the ingest must agree, or the agent searches an index
	// nothing was written to.
	srv := DefaultServers(repo, "")
	var args []string
	for _, s := range srv {
		if s.ID == ServerDocs {
			args = s.Args
		}
	}
	if !slices.Contains(args, "--project") || !slices.Contains(args, "declared-name") {
		t.Fatalf("serve must name the project: %v", args)
	}
	if slices.Contains(args, "--embedded") {
		t.Error("serve must not bypass the shared daemon: N sessions, one index")
	}
}

// The corpus is the REPOSITORY and the index is this directory on this branch.
// Getting the first wrong is expensive: a project resolved from the worktree
// path gives every worktree its own corpus, so each re-embeds what the others
// already paid for — the exact rework the corpus exists to kill.
func TestRaglitCorpusIsTheRepo_IndexIsDirAndBranch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	root := t.TempDir()
	repo := filepath.Join(root, "myrepo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(dir string, args ...string) {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", dir}, args...)...)
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run(repo, "init", "-q", "-b", "main")
	run(repo, "commit", "-q", "--allow-empty", "-m", "init")

	wt := filepath.Join(root, "wt-feature")
	run(repo, "worktree", "add", "-q", wt, "-b", "feat/x")

	// Same corpus from both, because it is the same repository.
	if p1, p2 := RaglitProject(repo), RaglitProject(wt); p1 != p2 {
		t.Fatalf("a worktree must share its repo's corpus: %q vs %q", p1, p2)
	}
	if got := RaglitProject(wt); got != "myrepo" {
		t.Errorf("corpus should be the repo name, got %q", got)
	}
	// Different indexes, because they are different working trees on different
	// branches — a search must not return a sibling worktree's files.
	if i1, i2 := RaglitIndex(repo), RaglitIndex(wt); i1 == i2 {
		t.Fatalf("two worktrees must not share one index: both %q", i1)
	}
	if got := RaglitIndex(wt); got != "wt-feature-feat-x" {
		t.Errorf("index should be dir+branch, got %q", got)
	}
	if got := RaglitIndex(repo); got != "myrepo-main" {
		t.Errorf("index should be dir+branch, got %q", got)
	}
}

// Config written before the rename says "code" and "docs". Every file and
// sidecar that can carry a server id reads those as poly-lsp and raglit.
func TestLegacyServerIDs_ReadAsTheNewOnes(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	dir := t.TempDir()

	// dun.json, mcp.json: a definition and a switch under the old ids.
	writeJSON(t, dir, project.ProjectServersFile, `{"servers":[{"id":"docs","autostart":true},{"id":"code","command":"/opt/bin/lsp"}]}`)
	got, err := LoadServers(dir, "/ws", "/rag")
	if err != nil {
		t.Fatal(err)
	}
	if ids := IDs(got); slices.Contains(ids, "docs") || slices.Contains(ids, "code") {
		t.Errorf("an old id must not survive as a second server: %v", ids)
	}
	m := byID(got)
	if !m["raglit"].Autostart {
		t.Error("docs autostart on must read as raglit on")
	}
	if m["poly-lsp"].Command != "/opt/bin/lsp" || len(m["poly-lsp"].Args) == 0 {
		t.Errorf("code override must read as poly-lsp and inherit its args: %+v", m["poly-lsp"])
	}

	// Project state (what /rag auto wrote): old id off, then new id on, wins.
	writeJSON(t, dir, project.LocalServersFile, `{"servers":[{"id":"docs","autostart":false}]}`)
	got, _ = LoadServers(dir, "/ws", "/rag")
	if byID(got)["raglit"].Autostart {
		t.Error("project state docs=false must read as raglit=false")
	}
	if _, err := setAutostart(dir, "raglit", true); err != nil {
		t.Fatal(err)
	}
	got, _ = LoadServers(dir, "/ws", "/rag")
	if !byID(got)["raglit"].Autostart {
		t.Error("a new write under the new id must be read")
	}
	f, err := ReadConfig(filepath.Join(project.Resolve(dir).StateDir, project.LocalServersFile))
	if err != nil || f == nil {
		t.Fatal(err)
	}
	for _, s := range f.Servers {
		if s.ID != "raglit" {
			t.Errorf("new writes use the new ids, found %q", s.ID)
		}
	}

	// The session sidecar. A current key beats an old one in the same file.
	sess := filepath.Join(t.TempDir(), "s.jsonl")
	on, off := true, false
	if _, err := SaveSessionServers(sess, map[string]SessionServer{"docs": {On: &off}, "code": {On: &on}, "raglit": {On: &on}}); err != nil {
		t.Fatal(err)
	}
	sw := LoadSessionServers(sess)
	if len(sw) != 2 || sw["poly-lsp"].On == nil || !*sw["poly-lsp"].On || sw["raglit"].On == nil || !*sw["raglit"].On {
		t.Errorf("sidecar: %+v", sw)
	}
	if !IsBuiltin("docs") || !IsBuiltin("code") || !IsBuiltin("raglit") || !IsBuiltin("poly-lsp") {
		t.Error("old and new ids both name the built-ins")
	}
}

// An older dun's .dun/dun.local.json is COPIED in on first
// sight — the original stays — and an import never overwrites state that is
// already there.
func TestProject_ImportsLegacyState(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	repo := gitRepo(t)
	legacy := filepath.Join(repo, project.DunDir)
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, legacy, project.LocalServersFile, `{"servers":[{"id":"docs","autostart":true}]}`)

	p, err := project.Ensure(repo)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{project.LocalServersFile} { // project.legacyState
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
	if _, err := project.Ensure(repo); err != nil {
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

func TestServerWake_SpecMerges(t *testing.T) {
	if !mergeSpec(Spec{ID: "a", Wake: true}, Spec{ID: "a", Command: "x"}).Wake {
		t.Error("omitted wake should inherit")
	}
}

func TestMergeSpec_ShellReplaced(t *testing.T) {
	low := Spec{ID: "x", Shell: &ShellOptions{Unchecked: []string{"a"}}}
	if got := mergeSpec(low, Spec{ID: "x"}); got.Shell == nil || !slices.Equal(got.Shell.Unchecked, []string{"a"}) {
		t.Errorf("a layer without a shell block keeps the lower one: %+v", got.Shell)
	}
	if got := mergeSpec(low, Spec{ID: "x", Shell: &ShellOptions{Unchecked: []string{"*"}}}); !slices.Equal(got.Shell.Unchecked, []string{"*"}) {
		t.Errorf("a later shell block replaces: %+v", got.Shell)
	}
}

// Re-adding a server replaces its definition but keeps the mode and instance
// it had: they are switches, and the add form does not ask for them.
func TestPutUserServer_KeepsModeAndInstance(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	if _, err := PutUserServer(Spec{ID: "corp", Command: "corp-mcp", Via: ViaShell, Instance: InstanceSession}); err != nil {
		t.Fatal(err)
	}
	if _, err := PutUserServer(Spec{ID: "corp", Command: "corp-mcp2"}); err != nil {
		t.Fatal(err)
	}
	f, _ := ReadConfig(UserServersFile())
	if len(f.Servers) != 1 || f.Servers[0].Command != "corp-mcp2" || f.Servers[0].Via != ViaShell || f.Servers[0].Instance != InstanceSession {
		t.Errorf("re-add: %+v", f.Servers)
	}
	// A value the new spec names wins.
	if _, err := PutUserServer(Spec{ID: "corp", Command: "corp-mcp2", Via: ViaDirect, Instance: InstanceShared}); err != nil {
		t.Fatal(err)
	}
	f, _ = ReadConfig(UserServersFile())
	if f.Servers[0].Via != ViaDirect || f.Servers[0].Instance != InstanceShared {
		t.Errorf("explicit: %+v", f.Servers)
	}
}

// The empty list survives a write; nil stays absent.
func TestServerSpec_EmptyDisableIsWritten(t *testing.T) {
	b, _ := json.Marshal(Spec{ID: "x", Disable: []string{}})
	if !strings.Contains(string(b), `"disable":[]`) {
		t.Errorf("%s", b)
	}
	b, _ = json.Marshal(Spec{ID: "x"})
	if strings.Contains(string(b), "disable") {
		t.Errorf("%s", b)
	}
}

// The user list is the lowest file layer; a remote server's bearer becomes
// an Authorization header, read from the environment when it says $NAME.
func TestLoadServers_UserListAndBearer(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	t.Setenv("CORP_TOKEN", "s3cret")
	if _, err := PutUserServer(Spec{ID: "corp", URL: "https://corp/mcp", Bearer: "$CORP_TOKEN"}); err != nil {
		t.Fatal(err)
	}
	if _, err := PutUserServer(Spec{ID: "gone", URL: "https://x/mcp"}); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(UserServersFile()); st.Mode().Perm() != 0o600 {
		t.Errorf("the user list holds tokens: perms %v", st.Mode().Perm())
	}
	dir := t.TempDir()
	writeJSON(t, dir, project.ProjectServersFile, `{"servers":[{"id":"gone","disabled":true}]}`)
	got, err := LoadServers(dir, "/ws", "/rag")
	if err != nil {
		t.Fatal(err)
	}
	corp, ok := byID(got)["corp"]
	if !ok || corp.URL != "https://corp/mcp" || corp.Headers["Authorization"] != "Bearer s3cret" {
		t.Fatalf("corp = %+v", corp)
	}
	if _, ok := byID(got)["gone"]; ok {
		t.Error("a project file must be able to disable a user server")
	}
	if ok, _ := RemoveUserServer("corp"); !ok {
		t.Error("remove")
	}
}

// The oauth block round-trips through mcp.json, merges field by field across
// layers, and is refused where it cannot work.
func TestOAuthSpec_ConfigRoundTripAndMerge(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	in := Spec{ID: "snow", URL: "https://acct.snowflakecomputing.com/api/v2/databases/d/schemas/s/mcp-servers/m",
		OAuth: &oauth.Spec{ClientID: "c1", ClientSecret: "$SNOW_SECRET", Scopes: []string{"session:role:ANALYST"},
			Resource: "none", Send: oauth.SendAccessToken, RedirectPort: 4000, AuthParams: map[string]string{"prompt": "consent"}}}
	if _, err := PutUserServer(in); err != nil {
		t.Fatal(err)
	}
	f, err := ReadConfig(UserServersFile())
	if err != nil || len(f.Servers) != 1 {
		t.Fatal(f, err)
	}
	b1, _ := json.Marshal(in)
	b2, _ := json.Marshal(f.Servers[0])
	if string(b1) != string(b2) {
		t.Errorf("round trip:\n%s\n%s", b1, b2)
	}
	if !strings.Contains(string(b2), `"client_secret":"$SNOW_SECRET"`) {
		t.Errorf("the secret reference must be kept as written: %s", b2)
	}
	dir := t.TempDir()
	writeJSON(t, dir, project.ProjectServersFile, `{"servers":[{"id":"snow","oauth":{"issuer":"https://idp.example","scopes":["a","b"]}}]}`)
	got, err := LoadServers(dir, "/ws", "/rag")
	if err != nil {
		t.Fatal(err)
	}
	o := byID(got)["snow"].OAuth
	if o == nil || o.ClientID != "c1" || o.Issuer != "https://idp.example" || !slices.Equal(o.Scopes, []string{"a", "b"}) || o.RedirectPort != 4000 {
		t.Errorf("merged %+v", o)
	}
	for _, c := range []struct {
		s    Spec
		via  string
		want string
	}{
		{Spec{ID: "x", Command: "x", OAuth: &oauth.Spec{ClientID: "c"}}, "", "remote server"},
		{Spec{ID: "x", URL: "https://x", Bearer: "t", OAuth: &oauth.Spec{ClientID: "c"}}, "", "both bearer and oauth"},
		{Spec{ID: "x", URL: "https://x", OAuth: &oauth.Spec{ClientID: "c", Send: "jwt"}}, "", "send"},
	} {
		if err := CheckOAuth(c.s, c.via); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: %v, want %q", c.s, err, c.want)
		}
	}
}

// Setting an OAuth server scripted is allowed now.
func TestOAuth_ScriptedIsAllowed(t *testing.T) {
	if err := CheckOAuth(Spec{ID: "x", URL: "https://x/mcp", OAuth: &oauth.Spec{ClientID: "c"}}, ViaShell); err != nil {
		t.Errorf("checkOAuth: %v", err)
	}
}

// Via survives the user file and the layered load; "direct" reads as the
// default; anything else, and a built-in in the shell, is refused by name.
func TestServerSpec_ViaRoundTrip(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	if _, err := PutUserServer(Spec{ID: "corp", Command: "corp-mcp", Via: ViaShell}); err != nil {
		t.Fatal(err)
	}
	if _, err := PutUserServer(Spec{ID: "db", Command: "db-mcp", Via: ViaDirect}); err != nil {
		t.Fatal(err)
	}
	f, _ := ReadConfig(UserServersFile())
	if f.Servers[0].Via != ViaShell {
		t.Errorf("file: %+v", f.Servers)
	}
	dir := t.TempDir()
	got, err := LoadServers(dir, "/ws", "")
	if err != nil {
		t.Fatal(err)
	}
	if byID(got)["corp"].Via != ViaShell || byID(got)["db"].Via != "" {
		t.Errorf("corp %+v db %+v", byID(got)["corp"], byID(got)["db"])
	}
	// A project file can move it back with an explicit "direct".
	writeJSON(t, dir, project.ProjectServersFile, `{"servers":[{"id":"corp","via":"direct"}]}`)
	if got, _ := LoadServers(dir, "/ws", ""); byID(got)["corp"].Via != "" {
		t.Errorf("direct over shell: %+v", byID(got)["corp"])
	}
	writeJSON(t, dir, project.ProjectServersFile, `{"servers":[{"id":"corp","via":"sideways"}]}`)
	if _, err := LoadServers(dir, "/ws", ""); err == nil || !strings.Contains(err.Error(), "sideways") {
		t.Errorf("bad via: %v", err)
	}
	writeJSON(t, dir, project.ProjectServersFile, `{"servers":[{"id":"code","via":"shell"}]}`)
	if _, err := LoadServers(dir, "/ws", ""); err == nil || !strings.Contains(err.Error(), "built in") {
		t.Errorf("a built-in in the shell: %v", err)
	}
}
