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
