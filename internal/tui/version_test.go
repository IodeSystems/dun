package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/dun/internal/buildinfo"
)

// /version says which dun this is and whether it updates itself, and from
// where; a source build says it does not look releases up (USER 2026-10-28).
func TestVersion_SourceBuildSaysItsPolicy(t *testing.T) {
	oldV, oldSrc := buildinfo.Version, buildinfo.SrcDir
	t.Cleanup(func() { buildinfo.Version, buildinfo.SrcDir = oldV, oldSrc })
	buildinfo.Version = "v0.1.11-3-gabcdef1-dirty"

	buildinfo.SrcDir = ""
	lines := versionLines(time.Now())
	if !strings.HasPrefix(lines[0], "dun dev abcdef1* · source build") || !strings.Contains(lines[1], "updates: none") {
		t.Errorf("plain build:\n%s", strings.Join(lines, "\n"))
	}
	buildinfo.SrcDir = "/src/dun"
	t.Setenv("DUN_NO_AUTOBUILD", "")
	if l := versionLines(time.Now())[1]; !strings.Contains(l, "rebuilds itself from /src/dun") || !strings.Contains(l, "releases are not looked up") {
		t.Errorf("checkout build: %q", l)
	}
	t.Setenv("DUN_NO_AUTOBUILD", "1")
	if l := versionLines(time.Now())[1]; !strings.Contains(l, "DUN_NO_AUTOBUILD=1") {
		t.Errorf("rebuild off: %q", l)
	}

	old := runTools
	t.Cleanup(func() { runTools = old })
	runTools = func(bool) ([]toolState, error) { return nil, nil }
	v := newVtui(100, 30)
	v.event(map[string]any{"type": "ready", "tools": []any{"exec"}})
	cmd := versionSlash(&v.m, nil)
	if msg, ok := cmd().(versionLatestMsg); !ok || strings.Contains(msg.line, "latest:") {
		t.Errorf("a source build must look up the tools only, not releases: %#v", cmd())
	}
	if !strings.Contains(v.convo(), "updates: ") {
		t.Errorf("/version printed nothing:\n%s", v.convo())
	}
}

// /version lists the libraries compiled in, then a line per tool with its
// path, its version and this dun's pin, marking a mismatch and a build that
// is not a release (USER 2026-11-09).
func TestVersion_ModulesAndTools(t *testing.T) {
	lines := strings.Join(versionLines(time.Now()), "\n")
	for _, want := range []string{"\nmodules:\n  agentkit v", "\n  raglit v", " (ragcore)", "\n  poly-lsp-mcp v", " (segment)", "\n  gwag v", "\n  openai-claude-api-shim v"} {
		if !strings.Contains(lines, want) {
			t.Errorf("no %q in:\n%s", want, lines)
		}
	}

	old := runTools
	t.Cleanup(func() { runTools = old })
	runTools = func(upgrade bool) ([]toolState, error) {
		if upgrade {
			t.Error("/version must not upgrade the tools")
		}
		return []toolState{
			{Name: "mcpshell", Path: "/b/mcpshell", Version: "v0.12.0", Origin: "release v0.12.0", Pin: "v0.12.0", Release: true},
			{Name: "poly-lsp-mcp", Path: "/g/poly-lsp-mcp", Version: "v0.4.0", Origin: "go install v0.4.0", Pin: "v0.5.0"},
			{Name: "raglit", Missing: true, Pin: "v0.8.0"},
		}, nil
	}
	v := newVtui(100, 30)
	got := versionSlash(&v.m, nil)().(versionLatestMsg).line
	want := strings.Join([]string{
		"tools:",
		"  mcpshell v0.12.0 at /b/mcpshell (release); pinned v0.12.0",
		"  poly-lsp-mcp v0.4.0 at /g/poly-lsp-mcp (go install v0.4.0); pinned v0.5.0 — older than the pin",
		"  raglit: not installed; pinned v0.8.0",
	}, "\n")
	if got != want {
		t.Errorf("tools block:\n%s\nwant:\n%s", got, want)
	}
}
