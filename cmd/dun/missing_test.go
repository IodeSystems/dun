package main

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/iodesystems/dun"
)

// mcpshell not installed (a binary-only install) is a setup step, not a
// failure: the splash says "not installed" in the secondary colour, never
// the failure red, and the one line under it says what is lost and where
// the install step is.
func TestSplash_MissingShellIsQuiet(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("DUN_HOME", t.TempDir())
	var sent bytes.Buffer
	m := newTUIModel(&dunProc{stdin: bufCloser{&sent}}, t.TempDir())
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = nm.(tuiModel)
	m = m.handleEvent(evMsg{"type": "session", "id": "s1"})
	err := (&dun.MissingError{Command: "mcpshell"}).Error()
	m = m.handleEvent(evMsg{"type": "ready", "tools": []any{"read"},
		"hint":    serverHint([]dun.ServerState{{ID: dun.ServerShell, Err: err, Missing: true}}),
		"servers": []any{map[string]any{"id": "shell", "err": err, "missing": true}}})
	m = m.handleEvent(evMsg{"type": "settings", "model": "", "url": ""})
	s := stripANSI(convoText(m))
	if !strings.Contains(s, "shell not installed") || strings.Contains(s, "did not start") {
		t.Errorf("splash:\n%s", s)
	}
	if !strings.Contains(s, "shell off: mcpshell is not installed: no eval tool") || !strings.Contains(s, "dun doctor") {
		t.Errorf("no line naming what is lost and the fix:\n%s", s)
	}
	red := stErr.Render("x")
	if on, _, _ := strings.Cut(red, "x"); on != "" && strings.Contains(m.convo[0].collapsed, on) {
		t.Errorf("a missing tool is in the failure colour: %q", m.convo[0].collapsed)
	}
}

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
