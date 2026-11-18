package main

import (
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/iodesystems/dun/internal/buildinfo"
	"github.com/iodesystems/dun/internal/mcpserver"
	"github.com/iodesystems/dun/release"
)

// toolsHome is a DUN_HOME with no tools and a PATH with only a go, every
// override unset.
func toolsHome(t *testing.T) (bin string) {
	t.Helper()
	bin = t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("DUN_HOME", t.TempDir())
	for _, s := range release.Supported {
		t.Setenv(mcpserver.ToolEnv(s.Name), "")
	}
	return bin
}

// toolFinding is the first "tool" finding about name.
func toolFinding(t *testing.T, d *doctor, name string) finding {
	t.Helper()
	for _, f := range d.found {
		if f.area == "tool" && strings.HasPrefix(f.detail, name+":") {
			return f
		}
	}
	t.Fatalf("no %s finding: %+v", name, d.found)
	return finding{}
}

// doctor: a missing mcpshell is a warn with the exact step; --fix runs
// go install of its iodesystems module into its managed directory, and it
// is found there.
func TestDoctor_MissingShellAndFix(t *testing.T) {
	toolsHome(t)
	d := &doctor{}
	d.tools()
	if f := toolFinding(t, d, "mcpshell"); f.level != "warn" || !strings.Contains(f.fix, "dun doctor --fix") ||
		!strings.Contains(f.fix, installModule("mcpshell")) || !strings.Contains(f.detail, "not installed (dun looks in "+mcpserver.ToolsDir()) {
		t.Errorf("without --fix: %+v", f)
	}
	var installed []string
	old := goInstall
	defer func() { goInstall = old }()
	goInstall = func(module, dir string) (string, error) {
		installed = append(installed, module)
		mod, _, _ := strings.Cut(module, "@")
		return dir, os.WriteFile(filepath.Join(dir, path.Base(mod)), []byte("#!/bin/sh\n"), 0o755)
	}
	d = &doctor{fix: true}
	d.tools()
	want := mcpserver.PinnedPath("mcpshell", mcpserver.PinVersion("mcpshell"))
	if f := toolFinding(t, d, "mcpshell"); f.level != "ok" || f.detail != "mcpshell: pinned "+mcpserver.PinVersion("mcpshell")+" at "+want ||
		!slices.Contains(installed, installModule("mcpshell")) || !slices.Contains(installed, installModule("poly-lsp-mcp")) {
		t.Errorf("--fix: %+v (installed %q)", f, installed)
	}
	if slices.Contains(installed, installModule("raglit")) {
		t.Error("--fix go-installed raglit, which is opt-in")
	}
}

// The doctor lines for each source: pinned, your build (an override),
// unused copies on PATH and beside dun, and missing.
func TestDoctor_ToolSources(t *testing.T) {
	bin := toolsHome(t)
	pin := mcpserver.PinVersion("mcpshell")
	pinned := mcpserver.PinnedPath("mcpshell", pin)
	os.MkdirAll(filepath.Dir(pinned), 0o755)
	os.WriteFile(pinned, []byte("#!/bin/sh\n"), 0o755)
	onPath := filepath.Join(bin, "mcpshell")
	os.WriteFile(onPath, []byte("#!/bin/sh\n"), 0o755)
	mine := filepath.Join(t.TempDir(), "poly-lsp-mcp")
	os.WriteFile(mine, []byte("#!/bin/sh\n"), 0o755)
	b, _ := json.Marshal(map[string]any{"tools": map[string]any{"poly-lsp-mcp": map[string]string{"path": mine}}})
	os.WriteFile(mcpserver.UserServersFile(), b, 0o600)
	// A copy beside dun (here, the test binary), as doctor --fix left them.
	exe, err := buildinfo.SelfExe()
	if err != nil {
		t.Fatal(err)
	}
	beside := filepath.Join(filepath.Dir(exe), "raglit")
	if err := os.WriteFile(beside, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Skip(err)
	}
	defer os.Remove(beside)

	d := &doctor{}
	d.tools()
	lines := map[string]finding{}
	for _, f := range d.found {
		if f.area == "tool" {
			lines[f.detail] = f
		}
	}
	file := mcpserver.UserServersFile()
	for detail, want := range map[string]finding{
		"mcpshell: pinned " + pin + " at " + pinned: {level: "ok"},
		"mcpshell: " + onPath + " is unused (dun uses " + pinned + ")": {level: "ok",
			fix: "rm " + onPath + " — or, to keep using it, set tools.mcpshell.path to it in " + file},
		"poly-lsp-mcp: your build at " + mine + " (from tools.poly-lsp-mcp.path in " + file + ")":                                                         {level: "ok"},
		"raglit: not installed (dun looks in " + filepath.Dir(mcpserver.PinnedPath("raglit", mcpserver.PinVersion("raglit"))) + "), needed only for /rag": {level: "warn"},
		"raglit: " + beside + " is unused (dun uses " + mcpserver.PinnedPath("raglit", mcpserver.PinVersion("raglit")) + ", not installed yet)": {level: "ok",
			fix: "rm " + beside + " — or, to keep using it, set tools.raglit.path to it in " + file},
	} {
		f, ok := lines[detail]
		if !ok || f.level != want.level || (want.fix != "" && f.fix != want.fix) {
			t.Errorf("want %s %q → %q; got %+v (found: %v)", want.level, detail, want.fix, f, ok)
		}
	}
	if len(lines) != 6 { // + git
		t.Errorf("tool lines: %+v", d.found)
	}

	// An env override wins, and doctor says dund sees it only if started with it.
	t.Setenv("DUN_MCPSHELL", onPath)
	d = &doctor{}
	d.tools()
	f := toolFinding(t, d, "mcpshell")
	if f.detail != "mcpshell: your build at "+onPath+" (from $DUN_MCPSHELL)" || !strings.Contains(f.fix, "only when dund was started with it") {
		t.Errorf("env override: %+v", f)
	}
}
