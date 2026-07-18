package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// restoreRenderers snapshots the global registry and restores it after the test
// (script tests register real tool names, which would leak into other tests).
func restoreRenderers(t *testing.T) {
	t.Helper()
	saved := make(map[string]ToolRenderer, len(toolRenderers))
	for k, v := range toolRenderers {
		saved[k] = v
	}
	t.Cleanup(func() { toolRenderers = saved })
}

func writeScript(t *testing.T, body string) {
	t.Helper()
	restoreRenderers(t)
	dir := t.TempDir()
	t.Setenv("DUN_HOME", dir)
	rdir := filepath.Join(dir, "renderers")
	if err := os.MkdirAll(rdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rdir, "r.star"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A Starlark renderer registers itself and renders via json.decode + helpers.
func TestScriptRenderer_Loads(t *testing.T) {
	writeScript(t, `
def render(ctx):
    rows = json.decode(ctx["result"])
    return dim("%d rows" % len(rows)), "\n".join([r["name"] for r in rows])

renderer("startest", render)
`)
	loadScriptRenderers()

	pv, full := renderToolResult(renderCtx{tool: "startest", result: `[{"name":"a"},{"name":"b"}]`})
	if !strings.Contains(pv, "2 rows") {
		t.Fatalf("script preview not used: %q", pv)
	}
	if !strings.Contains(full, "a") || !strings.Contains(full, "b") {
		t.Fatalf("script body not used: %q", full)
	}
}

// The shipped example script must parse and register (docs stay runnable).
func TestScriptRenderer_ExampleParses(t *testing.T) {
	restoreRenderers(t)
	if err := execRenderScript("../../examples/renderers/search.star"); err != nil {
		t.Fatalf("shipped example failed to load: %v", err)
	}
	pv, _ := renderToolResult(renderCtx{tool: "search", result: `[{"title":"T","score":1.0,"line":"x"}]`})
	if !strings.Contains(pv, "1 result") {
		t.Fatalf("example renderer not active: %q", pv)
	}
}

// A script that errors at render time falls back to the generic renderer rather
// than breaking the UI.
func TestScriptRenderer_ErrorFallsBack(t *testing.T) {
	writeScript(t, `
def render(ctx):
    return json.decode(ctx["result"])["missing"]  # KeyError at render time

renderer("startbad", render)
`)
	loadScriptRenderers()

	pv, _ := renderToolResult(renderCtx{tool: "startbad", result: `{"present":1}`})
	if !strings.Contains(pv, "present") { // generic preview of the raw result
		t.Fatalf("errored script should fall back to generic, got %q", pv)
	}
}
