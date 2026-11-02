package main

import (
	"os"
	"strings"
	"testing"

	"github.com/iodesystems/dun/internal/update"
)

// doctor names how dun and each tool were built.
func TestDoctor_Origins(t *testing.T) {
	d := &doctor{}
	d.build()
	if len(d.found) == 0 || d.found[0].area != "origin" || !strings.HasPrefix(d.found[0].detail, "dun: ") {
		t.Fatalf("%+v", d.found)
	}
	exe, err := os.Executable() // this test binary: a go build with build info
	if err != nil {
		t.Fatal(err)
	}
	d = &doctor{}
	d.toolOrigin("mcpshell", exe)
	if len(d.found) != 1 || !strings.HasPrefix(d.found[0].detail, "mcpshell: ") {
		t.Fatalf("%+v", d.found)
	}
	d = &doctor{}
	d.toolOrigin("mcpshell", "/bin/sh-not-there")
	if len(d.found) != 0 {
		t.Fatalf("a missing file: %+v", d.found)
	}
}

// On a release build, --fix downloads a pinned tool; without --fix the fix
// says so.
func TestDoctor_FixesFromThePin(t *testing.T) {
	defer func(p func(string) (update.Artifact, update.File, bool), i func(string) (string, error)) {
		pinnedTool, installPinned = p, i
	}(pinnedTool, installPinned)
	pinnedTool = func(name string) (update.Artifact, update.File, bool) {
		return update.Artifact{Version: "v0.9.2"}, update.File{}, name == "poly-lsp-mcp"
	}
	got := ""
	installPinned = func(name string) (string, error) { got = name; return "/x/" + name, nil }
	if p, err := (&doctor{fix: true}).install("poly-lsp-mcp"); err != nil || p != "/x/poly-lsp-mcp" || got != "poly-lsp-mcp" {
		t.Fatalf("%q %v %q", p, err, got)
	}
	_, err := (&doctor{}).install("poly-lsp-mcp")
	if fix := toolFix("poly-lsp-mcp", err); fix != "dun doctor --fix (downloads poly-lsp-mcp v0.9.2 from its release; the sha256 is pinned in this build)" {
		t.Fatalf("fix %q", fix)
	}
}
