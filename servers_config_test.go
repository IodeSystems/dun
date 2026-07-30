package dun

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	got, err := LoadServers(t.TempDir(), "/ws", "/raglit")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("want the 3 built-ins, got %d: %v", len(got), ServerIDs(got))
	}
	m := byID(got)
	if m["code"].Command != "poly-lsp-mcp" {
		t.Errorf("code server: %+v", m["code"])
	}
	if !strings.Contains(strings.Join(m["code"].Args, " "), "/ws") {
		t.Errorf("workspace not threaded into args: %v", m["code"].Args)
	}
}

// Overriding one binary's path must not require restating the other two — that
// is how a config silently forks from the defaults it copied.
func TestLoadServers_OverrideByIDKeepsSiblings(t *testing.T) {
	dir := t.TempDir()
	writeJSON(t, dir, LocalServersFile, `{"servers":[{"id":"code","command":"/opt/bin/poly-lsp-mcp"}]}`)
	got, err := LoadServers(dir, "/ws", "/raglit")
	if err != nil {
		t.Fatal(err)
	}
	m := byID(got)
	if m["code"].Command != "/opt/bin/poly-lsp-mcp" {
		t.Errorf("override did not take: %+v", m["code"])
	}
	// Args were not restated, so they must be inherited, not cleared.
	if len(m["code"].Args) == 0 {
		t.Error("omitted args should INHERIT, not clear")
	}
	if len(got) != 3 {
		t.Errorf("siblings must survive an override: %v", ServerIDs(got))
	}
}

// A project adding a fourth server is the motivating case.
func TestLoadServers_AddsProjectServer(t *testing.T) {
	dir := t.TempDir()
	writeJSON(t, dir, ProjectServersFile,
		`{"servers":[{"id":"db","command":"db-mcp","args":["stdio","--root","{{workspace}}"],"env":["DSN=x"]}]}`)
	got, err := LoadServers(dir, "/ws", "/raglit")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("want 4, got %v", ServerIDs(got))
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
	writeJSON(t, dir, ProjectServersFile, `{"servers":[{"id":"db","command":"db-mcp","args":["stdio"]}]}`)
	writeJSON(t, dir, LocalServersFile, `{"servers":[{"id":"db","command":"/home/me/bin/db-mcp"}]}`)
	got, err := LoadServers(dir, "/ws", "/raglit")
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
	writeJSON(t, dir, ProjectServersFile, `{"servers":[{"id":"docs","disabled":true}]}`)
	got, err := LoadServers(dir, "/ws", "/raglit")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("docs should be gone: %v", ServerIDs(got))
	}
	for _, s := range got {
		if s.ID == "docs" {
			t.Error("disabled server still present")
		}
	}
}

// A local file must not silently resurrect something the project turned off:
// re-enabling means removing the entry that disabled it.
func TestLoadServers_LocalCannotUndisable(t *testing.T) {
	dir := t.TempDir()
	writeJSON(t, dir, ProjectServersFile, `{"servers":[{"id":"docs","disabled":true}]}`)
	writeJSON(t, dir, LocalServersFile, `{"servers":[{"id":"docs","disabled":false}]}`)
	got, err := LoadServers(dir, "/ws", "/raglit")
	if err != nil {
		t.Fatal(err)
	}
	if len(byID(got)) != 2 {
		t.Errorf("disabled must stay disabled: %v", ServerIDs(got))
	}
}

// A malformed file must name ITSELF — in a two-file layered config, an
// unattributed JSON error sends the reader to the wrong file.
func TestLoadServers_ParseErrorNamesTheFile(t *testing.T) {
	dir := t.TempDir()
	writeJSON(t, dir, LocalServersFile, `{"servers":[{"id":`)
	_, err := LoadServers(dir, "/ws", "/raglit")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), LocalServersFile) {
		t.Errorf("error should name the offending file: %v", err)
	}
}

// A new server with no command cannot be spawned; say so at load time rather
// than failing obscurely at exec.
func TestLoadServers_RejectsCommandlessServer(t *testing.T) {
	dir := t.TempDir()
	writeJSON(t, dir, ProjectServersFile, `{"servers":[{"id":"ghost"}]}`)
	if _, err := LoadServers(dir, "/ws", "/raglit"); err == nil {
		t.Error("a server with no command should be rejected")
	}
}

// Only shell autostarts. code and docs cost real startup time and a machine
// may not even have their binaries — a session must not depend on them.
func TestLoadServers_OnlyShellAutostartsByDefault(t *testing.T) {
	got, err := LoadServers(t.TempDir(), "/ws", "/raglit")
	if err != nil {
		t.Fatal(err)
	}
	m := byID(got)
	if !m["shell"].Autostart {
		t.Error("shell should autostart")
	}
	if m["code"].Autostart || m["docs"].Autostart {
		t.Errorf("code/docs must be opt-in: code=%v docs=%v", m["code"].Autostart, m["docs"].Autostart)
	}
}

// .dun/dun.local.json is where dun writes its own state, and it must win over
// the same file at the workspace root (the pre-.dun layout, still honored).
func TestLoadServers_DunDirLocalWinsOverRoot(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, DunDir), 0o700); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, dir, LocalServersFile, `{"servers":[{"id":"docs","autostart":true}]}`)
	writeJSON(t, filepath.Join(dir, DunDir), LocalServersFile, `{"servers":[{"id":"docs","autostart":false}]}`)
	got, err := LoadServers(dir, "/ws", "/raglit")
	if err != nil {
		t.Fatal(err)
	}
	if byID(got)["docs"].Autostart {
		t.Error(".dun/dun.local.json should have won")
	}
}

// Turning autostart back OFF must be expressible, not require deleting the
// entry that turned it on — which is why Autostart is a pointer.
func TestSetAutostart_RoundTripBothWays(t *testing.T) {
	dir := t.TempDir()
	for _, want := range []bool{true, false, true} {
		path, err := SetAutostart(dir, "docs", want)
		if err != nil {
			t.Fatal(err)
		}
		if filepath.Dir(path) != filepath.Join(dir, DunDir) {
			t.Errorf("wrote outside %s: %s", DunDir, path)
		}
		got, err := LoadServers(dir, "/ws", "/raglit")
		if err != nil {
			t.Fatal(err)
		}
		if byID(got)["docs"].Autostart != want {
			t.Fatalf("autostart=%v did not survive a reload", want)
		}
	}
	// One entry, not one per call.
	f, err := readServersFile(filepath.Join(dir, DunDir, LocalServersFile))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Servers) != 1 {
		t.Errorf("want a single upserted entry, got %d", len(f.Servers))
	}
}

// The local file holds machine state and may hold secrets: writing it must
// also make it uncommittable, or the first /rag auto leaves a trap.
func TestSetAutostart_WritesGitignore(t *testing.T) {
	dir := t.TempDir()
	if _, err := SetAutostart(dir, "docs", true); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, DunDir, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), LocalServersFile) {
		t.Errorf(".gitignore does not cover %s: %q", LocalServersFile, b)
	}
	st, err := os.Stat(filepath.Join(dir, DunDir, LocalServersFile))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("want 0600, got %v", st.Mode().Perm())
	}
}
