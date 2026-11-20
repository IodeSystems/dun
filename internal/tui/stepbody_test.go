package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// stepPage is a two-call run (so the pager draws a tree) opened on its first
// call, tool with args and result, laid out at w; body is that call's rows.
func stepPage(tool string, args map[string]any, result string, failed bool, w int) (p pager, body []string) {
	_, out := renderToolResult(renderCtx{tool: tool, args: args, result: result, width: w})
	it := &runItem{tool: tool, args: args, result: result, done: true, failed: failed, output: out}
	other := &runItem{tool: "exec", args: map[string]any{"command": "true"}, done: true}
	r := &toolRun{items: []*runItem{it, other}}
	p = newPager(0, r)
	p.open[0] = true
	p.layout(r.page(), w, 60)
	return p, p.rows[1:p.nodeAt[1]]
}

func plainRows(rows []string) string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = strings.TrimRight(ansi.Strip(r), " ")
	}
	return strings.Join(out, "\n")
}

func noGoDump(t *testing.T, s string) {
	t.Helper()
	if strings.Contains(s, "map[") || strings.Contains(s, "%!") {
		t.Errorf("Go value dump in the body:\n%s", s)
	}
}

// The user's report (2026-11-19): a failed file_edit step showed
// "edits: [map[new:… old:…] …]". It shows the path, each edit as a diff,
// and the error in red.
func TestStepBody_FailedMultiEdit(t *testing.T) {
	args := map[string]any{"path": "Makefile", "edits": []any{
		map[string]any{"old": "build-docker: ## Build the co-located image\n\tdocker build .", "new": "build-docker: ## Build the production image\n\tdocker build -t prod ."},
		map[string]any{"old": "chromium-smoke: ## old", "new": "chromium-smoke: ## new"},
	}}
	res := "ERROR: edit 2: old text not found in Makefile"
	p, body := stepPage("file_edit", args, res, true, 100)
	got := plainRows(body)
	noGoDump(t, got)
	for _, want := range []string{
		"    Makefile",
		"    @@ edit 1/2",
		"    -build-docker: ## Build the co-located image",
		"    -    docker build .",
		"    +build-docker: ## Build the production image",
		"    @@ edit 2/2",
		"    -chromium-smoke: ## old",
		"    +chromium-smoke: ## new",
		"    ERROR: edit 2: old text not found in Makefile",
	} {
		if !strings.Contains(got, want+"\n") && !strings.HasSuffix(got, want) {
			t.Errorf("body lacks %q:\n%s", want, got)
		}
	}
	for _, r := range body {
		s := ansi.Strip(r)
		switch {
		case strings.HasPrefix(s, "    -"):
			if !strings.Contains(r, ansiFG(diffDel)) {
				t.Errorf("- line not in the deletion colour: %q", r)
			}
		case strings.HasPrefix(s, "    +"):
			if !strings.Contains(r, ansiFG(diffAdd)) {
				t.Errorf("+ line not in the addition colour: %q", r)
			}
		case strings.HasPrefix(s, "    ERROR"):
			if !strings.Contains(r, ansiFG(StErr)) {
				t.Errorf("error not red: %q", r)
			}
		}
	}
	if head := ansi.Strip(p.rows[0]); !strings.Contains(head, "✗ edit Makefile") {
		t.Errorf("head = %q", head)
	}
}

// A successful edit shows the result's diff (with its context), and not the
// call's old/new again.
func TestStepBody_EditDiff(t *testing.T) {
	args := map[string]any{"path": "main.go", "edits": []any{map[string]any{"old": "hello", "new": "hi"}}}
	res := "edited main.go: +1 -1\n@@ -4,5 +4,5 @@\n func main() {\n-\tfmt.Println(\"hello\")\n+\tfmt.Println(\"hi\")\n }"
	_, body := stepPage("file_edit", args, res, false, 100)
	got := plainRows(body)
	noGoDump(t, got)
	for _, want := range []string{"main.go", "edited main.go: +1 -1", "@@ -4,5 +4,5 @@", `-    fmt.Println("hello")`, `+    fmt.Println("hi")`} {
		if !strings.Contains(got, want) {
			t.Errorf("body lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "-hello") || strings.Contains(got, "edits") {
		t.Errorf("the call's edits drawn as well as the result's diff:\n%s", got)
	}
	var add bool
	for _, r := range body {
		if strings.HasPrefix(ansi.Strip(r), "    +") && strings.Contains(r, ansiFG(diffAdd)) {
			add = true
		}
	}
	if !add {
		t.Errorf("no coloured + line:\n%q", body)
	}
}

// A tool with no renderer of its own (an MCP tool) with nested args: key:
// value lines, nested values indented, a multi-line string a block, a long
// string wrapped under its key — never a Go map dump.
func TestStepBody_GenericNested(t *testing.T) {
	args := map[string]any{
		"query": map[string]any{"filter": map[string]any{"kind": "doc", "limit": 5.0}, "tags": []any{"a", "b"}},
		"items": []any{map[string]any{"id": 1.0, "name": "one"}, map[string]any{"id": 2.0, "name": "two"}},
		"note":  "line one\nline two",
		"long":  strings.Repeat("word ", 30),
		"empty": map[string]any{},
		"flag":  true,
	}
	_, body := stepPage("srv__lookup", args, `{"ok": true}`, false, 60)
	got := plainRows(body)
	noGoDump(t, got)
	want := []string{
		"    empty: {}",
		"    flag: true",
		"    items:",
		"      - id: 1",
		"        name: one",
		"      - id: 2",
		"        name: two",
		"    long: word word",
		"    note: |",
		"      line one",
		"      line two",
		"    query:",
		"      filter:",
		"        kind: doc",
		"        limit: 5",
		"      tags:",
		"        - a",
		"        - b",
	}
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("body lacks %q:\n%s", w, got)
		}
	}
	for _, r := range body {
		if ansi.StringWidth(r) > 60 {
			t.Errorf("row wider than the pane: %q", ansi.Strip(r))
		}
	}
	if !strings.Contains(got, "\n      word word") {
		t.Errorf("long string not wrapped under its key:\n%s", got)
	}
}

// The one-line forms and the inspector's input are not Go dumps either.
func TestArgForms_NoGoDump(t *testing.T) {
	args := map[string]any{"edits": []any{map[string]any{"old": "a", "new": "b"}}, "n": 3.0}
	if p := argPreview(args, 200); p != `edits=[{"new":"b","old":"a"}], n=3` {
		t.Errorf("argPreview = %q", p)
	}
	full := stripANSI(argFull(args))
	noGoDump(t, full)
	if full != "  edits:\n    - new: b\n      old: a\n  n: 3" {
		t.Errorf("argFull =\n%s", full)
	}
	it := &runItem{tool: "srv__get", args: map[string]any{"q": map[string]any{"k": "v"}}}
	if v := callTarget(it); v != `{"k":"v"}` {
		t.Errorf("callTarget = %q", v)
	}
}
