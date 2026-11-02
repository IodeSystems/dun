package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// doctor: a missing mcpshell is a warn with the exact step; --fix runs
// go install of its iodesystems module and finds the result.
func TestDoctor_MissingShellAndFix(t *testing.T) {
	bin := t.TempDir()
	// A PATH with a go and nothing else: the machine's own mcpshell must
	// not be found.
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	shell := func(d *doctor) finding {
		for _, f := range d.found {
			if strings.HasPrefix(f.detail, "mcpshell") {
				return f
			}
		}
		t.Fatalf("no mcpshell finding: %+v", d.found)
		return finding{}
	}
	d := &doctor{}
	d.tools()
	if f := shell(d); f.level != "warn" || !strings.Contains(f.fix, "dun doctor --fix") || !strings.Contains(f.fix, installModule("mcpshell")) {
		t.Errorf("without --fix: %+v", f)
	}
	var installed []string
	old := goInstall
	defer func() { goInstall = old }()
	goInstall = func(module, _ string) (string, error) {
		installed = append(installed, module)
		name, _, _ := strings.Cut(filepath.Base(module), "@")
		return bin, os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"), 0o755)
	}
	d = &doctor{fix: true}
	d.tools()
	if f := shell(d); f.level != "ok" || !slices.Contains(installed, installModule("mcpshell")) || !slices.Contains(installed, installModule("poly-lsp-mcp")) {
		t.Errorf("--fix: %+v (installed %q)", f, installed)
	}
}
