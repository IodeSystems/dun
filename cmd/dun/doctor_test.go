package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVersionLess(t *testing.T) {
	for _, c := range []struct {
		a, b string
		less bool
	}{{"1.26.2", "1.26.10", true}, {"1.26.10", "1.26.2", false}, {"1.25", "1.26.2", true}, {"1.26.2", "1.26.2", false}, {"1.27", "1.26.2", false}} {
		if got := versionLess(c.a, c.b); got != c.less {
			t.Errorf("versionLess(%s, %s) = %v", c.a, c.b, got)
		}
	}
}

func TestLookIn(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "mcpshell"), []byte("#!/bin/sh\n"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "notexec"), []byte(""), 0o644)
	if lookIn("/nope:"+dir, "mcpshell") == "" || lookIn(dir, "notexec") != "" || lookIn(dir, "git-nope") != "" {
		t.Error("lookIn finds executables only, in the PATH given")
	}
}

// Logged out is a failure doctor names, with the fix.
func TestDoctorLoggedOut(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	d := &doctor{}
	d.providers()
	if len(d.found) != 1 || d.found[0].level != "fail" || !strings.Contains(d.found[0].fix, "/login") {
		t.Fatalf("%+v", d.found)
	}
}

// --fix runs only the fixes that are one command; the rest need the person.
func TestDoctor_Fixable(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	act := fixable(finding{"warn", "config", "open", "chmod 600 " + p})
	if act == nil {
		t.Fatal("chmod is not fixable")
	}
	if err := act(); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", st.Mode().Perm())
	}
	if fixable(finding{"warn", "dund", "old", "dun dund service restart; sessions are kept"}) == nil {
		t.Error("the service restart is not fixable")
	}
	for _, f := range []finding{
		{"fail", "model", "no model", "/model in the TUI, web or app"},
		{"ok", "config", "fine", "chmod 600 " + p},
	} {
		if fixable(f) != nil {
			t.Errorf("fixable: %+v", f)
		}
	}
}

// dun doctor --help says what --fix runs.
func TestDoctor_Help(t *testing.T) {
	c := doctorCmd()
	var out strings.Builder
	c.SetOut(&out)
	c.SetArgs([]string{"--help"})
	if err := c.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"dun doctor [flags]", "--fix", "dun dund service restart", "chmod 600"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("help lacks %q:\n%s", want, out.String())
		}
	}
}

// `poly-lsp-mcp status --json`: a line per root, its running children, their
// RSS and the index's files; not JSON is no lines.
func TestPolyStatusLines(t *testing.T) {
	out := []byte(`[{"pid":41,"root":"/w/a","children":[{"language":"go","state":"running","pid":50,"rss":1073741824},
		{"language":"python","state":"stopped","idleSec":0}],"index":{"files":1234,"bytes":9},
		"fleet":{"maxChildren":4,"running":2,"rss":2147483648,"rssBudget":4294967296}},
		{"pid":42,"root":"/w/b","children":[]}]`)
	lines, ok := polyStatusLines(out)
	want := []string{"status: /w/a (pid 41): 1 child LSP running, 1.0 GB RSS, index 1,234 files; daemon 2 children (of 4), 2.0 GB RSS of 4.0 GB",
		"status: /w/b (pid 42): 0 child LSPs running, 0 B RSS, index not reported"}
	if !ok || strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("%v %q", ok, lines)
	}
	if lines, ok := polyStatusLines([]byte("[]")); !ok || len(lines) != 1 {
		t.Fatalf("none running: %v %q", ok, lines)
	}
	if _, ok := polyStatusLines([]byte("Error: unknown command")); ok {
		t.Fatal("not JSON read as status")
	}
}
