package dun

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A command on neither PATH nor beside dun is a MissingError that says what
// is lost and where the install step is; one beside dun's executable is
// found with no PATH entry (a release that ships its tools in one directory).
func TestFindCommand_PathThenBesideDun(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := FindCommand("mcpshell")
	var miss *MissingError
	if !errors.As(err, &miss) || !strings.Contains(err.Error(), "no eval tool") || !strings.Contains(err.Error(), "dun doctor") {
		t.Fatalf("missing mcpshell: %v", err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Skip(err)
	}
	beside := filepath.Join(filepath.Dir(exe), "dun-test-beside-tool")
	if err := os.WriteFile(beside, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Skip(err)
	}
	defer os.Remove(beside)
	if p, err := FindCommand("dun-test-beside-tool"); err != nil || p != beside {
		t.Errorf("beside dun: %q, %v", p, err)
	}
	if p, err := FindCommand("./x/y"); err != nil || p != "./x/y" {
		t.Errorf("a path is used as it is: %q, %v", p, err)
	}
}
