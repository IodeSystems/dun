package depver

import (
	"os"
	"runtime/debug"
	"strings"
	"testing"

	"golang.org/x/mod/modfile"
)

// Each pin is go.mod's require version: a go.mod bump that leaves Modules
// behind fails here, not as a wrong segmenter version in a release.
func TestPinsMatchGoMod(t *testing.T) {
	b, err := os.ReadFile("../../go.mod")
	if err != nil {
		t.Fatal(err)
	}
	f, err := modfile.Parse("go.mod", b, nil)
	if err != nil {
		t.Fatal(err)
	}
	req := map[string]string{}
	for _, r := range f.Require {
		req[r.Mod.Path] = r.Mod.Version
	}
	for _, m := range Modules {
		if got, ok := req[m.Path]; !ok {
			t.Errorf("%s: not required by go.mod", m.Path)
		} else if got != m.Pin {
			t.Errorf("%s: Modules pins %s, go.mod requires %s", m.Path, m.Pin, got)
		}
	}
	for _, r := range f.Replace {
		for _, m := range Modules {
			if r.Old.Path == m.Path {
				t.Errorf("go.mod replaces %s: its pin is not what is compiled in", m.Path)
			}
		}
	}
}

// The build info's version wins; a directory replace is devel; a binary
// with no dependencies in its build info (a Go 1.26 test binary) reads the
// pin.
func TestVersionSources(t *testing.T) {
	deps := map[string]*debug.Module{
		AgentKit: {Path: AgentKit, Version: "v0.27.0"},
		PolyLSP:  {Path: PolyLSP, Version: "v0.5.0", Replace: &debug.Module{Path: "../poly-lsp-mcp"}},
		Shim:     {Path: Shim, Version: "v0.6.0", Replace: &debug.Module{Path: Shim, Version: "v0.6.1"}},
	}
	for _, c := range []struct{ path, want, from string }{
		{AgentKit, "v0.27.0", "build"},
		{PolyLSP, Devel, "replaced by ../poly-lsp-mcp"},
		{Shim, "v0.6.1", "build"},
		{Raglit, "v0.9.0", "go.mod"},
		{"example.com/other", "", ""},
	} {
		if v, from := version(c.path, deps); v != c.want || from != c.from {
			t.Errorf("version(%s) = %q, %q; want %q, %q", c.path, v, from, c.want, c.from)
		}
	}
}

func TestModuleLines(t *testing.T) {
	deps := map[string]*debug.Module{}
	for _, m := range Modules {
		deps[m.Path] = &debug.Module{Path: m.Path, Version: m.Pin}
	}
	deps[PolyLSP] = &debug.Module{Path: PolyLSP, Version: "v0.5.0", Replace: &debug.Module{Path: "/src/poly-lsp-mcp"}}
	delete(deps, Gwag)
	want := []string{
		"agentkit v0.29.0",
		"openai-claude-api-shim v0.6.0",
		"raglit v0.9.0 (ragcore)",
		"poly-lsp-mcp devel (segment) — replaced by /src/poly-lsp-mcp; go.mod pins v0.5.1",
		"gwag v2.11.0 (gat) — from go.mod: this binary's build info lists no modules",
		"bubbleteandmd v0.4.0 (render)",
	}
	if got := moduleLines(deps); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("moduleLines:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestToolLine(t *testing.T) {
	for _, c := range []struct {
		t    Tool
		want string
	}{
		{Tool{Name: "mcpshell", Path: "/b/mcpshell", Version: "v0.12.0", Origin: "release v0.12.0", Pin: "v0.12.0", Release: true},
			"mcpshell v0.12.0 at /b/mcpshell (release); pinned v0.12.0"},
		{Tool{Name: "raglit", Path: "/b/raglit", Version: "v0.7.0", Origin: "release v0.7.0", Pin: "v0.8.0", Release: true},
			"raglit v0.7.0 at /b/raglit (release); pinned v0.8.0 — differs from the pin"},
		{Tool{Name: "poly-lsp-mcp", Path: "/g/poly-lsp-mcp", Version: "v0.5.1-0.20261101-abc+dirty", Origin: "built from a checkout at abc, modified", Pin: "v0.5.0"},
			"poly-lsp-mcp v0.5.1-0.20261101-abc+dirty at /g/poly-lsp-mcp (built from a checkout at abc, modified); pinned v0.5.0 — differs from the pin, not a release build"},
		{Tool{Name: "mcpshell", Path: "/g/mcpshell", Version: "v0.12.0", Origin: "go install v0.12.0", Pin: "v0.12.0"},
			"mcpshell v0.12.0 at /g/mcpshell (go install v0.12.0); pinned v0.12.0 — not a release build"},
		{Tool{Name: "raglit", Missing: true, Pin: "v0.8.0"}, "raglit: not installed; pinned v0.8.0"},
		{Tool{Name: "raglit", Path: "/s/raglit", Pin: "v0.8.0"}, "raglit at /s/raglit: version not known (no build info); pinned v0.8.0"},
	} {
		if got := c.t.Line(); got != c.want {
			t.Errorf("Line() = %q\nwant      %q", got, c.want)
		}
	}
}
