package main

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	starlarkjson "go.starlark.net/lib/json"
	"go.starlark.net/starlark"

	"github.com/charmbracelet/lipgloss"
)

// Runtime-loaded tool-result renderers, written in Starlark. Scripts live in
// $DUN_HOME/renderers/ (or ~/.dun/renderers/), one or more per file, and register
// themselves over the SAME ToolRenderer registry as the compiled-in ones — so a
// script named for a built-in tool OVERRIDES it (loaded after, last write wins).
//
// Starlark is deterministic and sandboxed (no filesystem/network), which is the
// point: a renderer runs on untrusted tool output every turn. A script gets a
// small predeclared API and returns (preview, full):
//
//	# ~/.dun/renderers/search.star
//	def render(ctx):
//	    hits = json.decode(ctx["result"])
//	    lines = ["%s (%.2f)" % (h["title"], h["score"]) for h in hits]
//	    return dim("%d hits" % len(hits)), "\n".join(lines)
//	renderer("search", render)
//
// Predeclared: renderer(tool, fn) · dim(s)/tool(s)/bold(s) (ANSI styling) ·
// diff(s) (colorize a unified diff) · clip(s, n) · json (decode/encode/indent).
// ctx is a dict {tool, args, result, width}. A render error or bad return value
// falls back to the generic renderer, so a broken script never breaks the UI.

// loadScriptRenderers execs every *.star in the renderers dir (sorted, so
// overrides are deterministic). Best-effort: a bad script logs and is skipped.
func loadScriptRenderers() {
	dir := renderersDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return // no renderers dir → nothing to load
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".star") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, n := range names {
		if err := execRenderScript(filepath.Join(dir, n)); err != nil {
			fmt.Fprintf(os.Stderr, "dun: renderer %s: %v\n", n, err)
		}
	}
}

func renderersDir() string {
	if h := os.Getenv("DUN_HOME"); h != "" {
		return filepath.Join(h, "renderers")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".dun", "renderers")
	}
	return filepath.Join(home, ".dun", "renderers")
}

func execRenderScript(path string) error {
	src, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	thread := &starlark.Thread{Name: "load:" + filepath.Base(path)}
	pre := starlark.StringDict{
		"renderer": starlark.NewBuiltin("renderer", registerBuiltin(path)),
		"dim":      styleBuiltin("dim", stDim),
		"tool":     styleBuiltin("tool", stTool),
		"bold":     styleBuiltin("bold", lipgloss.NewStyle().Bold(true)),
		"diff":     starlark.NewBuiltin("diff", diffBuiltin),
		"clip":     starlark.NewBuiltin("clip", clipBuiltin),
		"json":     starlarkjson.Module,
	}
	_, err = starlark.ExecFile(thread, path, src, pre)
	return err
}

// starRenderer adapts a Starlark render function to the ToolRenderer interface.
type starRenderer struct {
	fn   *starlark.Function
	file string
}

func (s starRenderer) Render(rc renderCtx) (string, string) {
	thread := &starlark.Thread{Name: "render:" + rc.tool}
	ctx := starlark.NewDict(4)
	_ = ctx.SetKey(starlark.String("tool"), starlark.String(rc.tool))
	_ = ctx.SetKey(starlark.String("result"), starlark.String(rc.result))
	_ = ctx.SetKey(starlark.String("width"), starlark.MakeInt(rc.width))
	_ = ctx.SetKey(starlark.String("args"), goToStarlark(rc.args))
	res, err := starlark.Call(thread, s.fn, starlark.Tuple{ctx}, nil)
	if err != nil {
		return genericRender(rc)
	}
	return unpackRender(res, rc)
}

// unpackRender accepts (preview, full) as a 2-tuple/list, or a single string
// (full only, generic preview); anything else falls back to generic.
func unpackRender(v starlark.Value, rc renderCtx) (string, string) {
	switch t := v.(type) {
	case starlark.Tuple:
		if len(t) == 2 {
			return asString(t[0]), asString(t[1])
		}
	case *starlark.List:
		if t.Len() == 2 {
			return asString(t.Index(0)), asString(t.Index(1))
		}
	case starlark.String:
		pv, _ := genericRender(rc)
		return pv, string(t)
	}
	return genericRender(rc)
}

func asString(v starlark.Value) string {
	if s, ok := starlark.AsString(v); ok {
		return s
	}
	return v.String()
}

// ── builtins ───────────────────────────────────────────────────────

func registerBuiltin(file string) func(*starlark.Thread, *starlark.Builtin, starlark.Tuple, []starlark.Tuple) (starlark.Value, error) {
	return func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		var tool string
		var fn starlark.Value
		if err := starlark.UnpackArgs("renderer", args, kwargs, "tool", &tool, "fn", &fn); err != nil {
			return nil, err
		}
		f, ok := fn.(*starlark.Function)
		if !ok {
			return nil, fmt.Errorf("renderer: fn must be a function, got %s", fn.Type())
		}
		registerRenderer(tool, starRenderer{fn: f, file: file})
		return starlark.None, nil
	}
}

func styleBuiltin(name string, st lipgloss.Style) *starlark.Builtin {
	return starlark.NewBuiltin(name, func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		var s string
		if err := starlark.UnpackArgs(name, args, kwargs, "s", &s); err != nil {
			return nil, err
		}
		return starlark.String(st.Render(s)), nil
	})
}

func diffBuiltin(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var s string
	if err := starlark.UnpackArgs("diff", args, kwargs, "s", &s); err != nil {
		return nil, err
	}
	return starlark.String(colorizeDiff(s)), nil
}

func clipBuiltin(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var s string
	var n int
	if err := starlark.UnpackArgs("clip", args, kwargs, "s", &s, "n", &n); err != nil {
		return nil, err
	}
	return starlark.String(clip(s, n)), nil
}

// goToStarlark converts a decoded-JSON Go value (tool args) to a Starlark value.
func goToStarlark(v any) starlark.Value {
	switch t := v.(type) {
	case nil:
		return starlark.None
	case bool:
		return starlark.Bool(t)
	case string:
		return starlark.String(t)
	case float64:
		if t == math.Trunc(t) {
			return starlark.MakeInt(int(t))
		}
		return starlark.Float(t)
	case []any:
		elems := make([]starlark.Value, len(t))
		for i, e := range t {
			elems[i] = goToStarlark(e)
		}
		return starlark.NewList(elems)
	case map[string]any:
		d := starlark.NewDict(len(t))
		for k, val := range t {
			_ = d.SetKey(starlark.String(k), goToStarlark(val))
		}
		return d
	default:
		return starlark.String(fmt.Sprint(t))
	}
}
