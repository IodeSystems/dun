package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Tool-result renderers. A renderer turns one tool's result into a collapsed
// one-line preview + a full expanded body (the TUI folds the call line + ▸/▾
// around them). Keyed by tool name; unknown tools fall to genericRender.
//
// Loading is COMPILED-IN for now (registerRenderer in init). The ToolRenderer
// interface is the seam: a later runtime mechanism — an external `dun-render-
// <tool>` process, or a script/config-backed renderer — implements the same
// interface and registers itself, with no change to the call sites here or in
// the event handler.

type renderCtx struct {
	tool   string
	args   map[string]any
	result string
	width  int
}

// ToolRenderer turns a tool result into (collapsed preview, full body).
type ToolRenderer interface {
	Render(rc renderCtx) (preview, full string)
}

type rendererFunc func(renderCtx) (string, string)

func (f rendererFunc) Render(rc renderCtx) (string, string) { return f(rc) }

var toolRenderers = map[string]ToolRenderer{}

func registerRenderer(tool string, r ToolRenderer) { toolRenderers[tool] = r }

// renderToolResult dispatches to a registered renderer or the generic fallback.
func renderToolResult(rc renderCtx) (preview, full string) {
	if r, ok := toolRenderers[rc.tool]; ok {
		return r.Render(rc)
	}
	return genericRender(rc)
}

// genericRender: a clipped one-line preview + the raw body, diff-colorized when
// it looks like a unified diff (the prior default behavior, now the fallback).
func genericRender(rc renderCtx) (string, string) {
	preview := stDim.Render("  → " + clip(oneLine(rc.result), 100))
	if isDiff(rc.result) {
		return preview, colorizeDiff(rc.result)
	}
	return preview, stDim.Render(rc.result)
}

// diffRender always colorizes; the preview is an add/del line stat.
func diffRender(rc renderCtx) (string, string) {
	return stDim.Render("  → " + diffStat(rc.result)), colorizeDiff(rc.result)
}

// jsonRender pretty-prints a JSON result; the preview summarizes shape (item
// count / top-level keys). Non-JSON falls back to generic.
func jsonRender(rc renderCtx) (string, string) {
	var v any
	if json.Unmarshal([]byte(strings.TrimSpace(rc.result)), &v) != nil {
		return genericRender(rc)
	}
	pretty, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return genericRender(rc)
	}
	return stDim.Render("  → " + jsonSummary(v)), stDim.Render(string(pretty))
}

func jsonSummary(v any) string {
	switch t := v.(type) {
	case []any:
		return fmt.Sprintf("%d item(s)", len(t))
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return clip(strings.Join(keys, ", "), 100)
	default:
		return clip(oneLine(fmt.Sprint(v)), 100)
	}
}

// diffStat counts +/- body lines of a unified diff, e.g. "+3 -1".
func diffStat(s string) string {
	add, del := 0, 0
	for _, ln := range strings.Split(s, "\n") {
		switch {
		case strings.HasPrefix(ln, "+") && !strings.HasPrefix(ln, "+++"):
			add++
		case strings.HasPrefix(ln, "-") && !strings.HasPrefix(ln, "---"):
			del++
		}
	}
	if add == 0 && del == 0 {
		return clip(oneLine(s), 100)
	}
	return fmt.Sprintf("+%d -%d", add, del)
}

func init() {
	// Code edits return unified diffs (+ diagnostics) — always colorize.
	registerRenderer("node_edit", rendererFunc(diffRender))
	// These commonly return JSON payloads; pretty-print, else generic.
	registerRenderer("search", rendererFunc(jsonRender))
	registerRenderer("node_query", rendererFunc(jsonRender))
	// node_read / exec / eval fall to generic (code/terminal text shown raw).
}
