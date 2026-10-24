package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
)

// The fixtures are real mcpshell eval results, captured from its MCP server
// (`mcpshell mcp --web --connect lsp=poly-lsp-mcp …`): the code sent and the
// text it answered.
type evalFixture struct {
	Code    string `json:"code"`
	IsError bool   `json:"isError"`
	Text    string `json:"text"`
}

func evalFixtures(t *testing.T) map[string]evalFixture {
	t.Helper()
	b, err := os.ReadFile("testdata/mcpshell/eval_results.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx map[string]evalFixture
	if err := json.Unmarshal(b, &fx); err != nil {
		t.Fatal(err)
	}
	return fx
}

// asDunSees is a fixture's result as it reaches dun: an MCP error comes
// through agentkit (which formats the text blocks as a list) and dun's
// dispatcher as "ERROR: mcp tool error: [<text>]" — seen in a live run.
func asDunSees(f evalFixture) string {
	if f.IsError {
		return "ERROR: mcp tool error: [" + f.Text + "]"
	}
	return f.Text
}

func renderFixture(f evalFixture) (string, string) {
	res := asDunSees(f)
	return evalRender(renderCtx{tool: "eval", args: map[string]any{"code": f.Code}, result: res, width: 100})
}

func TestEvalCallText(t *testing.T) {
	fx := evalFixtures(t)
	for _, c := range []struct{ code, want string }{
		{`"hello" |> upper()`, `"hello" |> upper()`},
		{fx["multiline"].Code, "let xs = range(10)  +5 lines"},
		{"// sum the evens\n\nlet s = sum([2, 4])\ns", "let s = sum([2, 4])  +1 lines"},
		{fx["hosted_lsp"].Code, `◇ lsp.node_query  selector="file > func", limit=5`},
		{fx["hosted_call"].Code, `◇ Html.text  "<h1>Hi</h1><p>there</p>", "h1"`},
		{fx["help_ns"].Code, "help Html"},
		{fx["help"].Code, "help"},
	} {
		if got := callText("eval", map[string]any{"code": c.code}, 80); got != c.want {
			t.Errorf("callText(%q)\n got %q\nwant %q", c.code, got, c.want)
		}
	}
	if got := callText("eval", map[string]any{}, 80); got != "eval" {
		t.Errorf("no code falls back to the generic form: %q", got)
	}
}

// A list of records is a table: a header, a rule, one aligned row each.
func TestEvalRender_Table(t *testing.T) {
	pv, body := renderFixture(evalFixtures(t)["records"])
	if !strings.Contains(stripANSI(pv), "✓ 3 rows") {
		t.Errorf("preview: %q", stripANSI(pv))
	}
	rows := strings.Split(stripANSI(body), "\n")
	if len(rows) != 5 || !strings.HasPrefix(rows[0], "id  name   email") || !strings.HasPrefix(rows[1], "──") {
		t.Fatalf("table:\n%s", stripANSI(body))
	}
	for _, r := range rows[2:] {
		if lipgloss.Width(r) != lipgloss.Width(rows[0]) {
			t.Errorf("row %q is not aligned to the header %q", r, rows[0])
		}
	}
	if !strings.Contains(rows[4], "carol  c@x.io") {
		t.Errorf("last row: %q", rows[4])
	}
}

// A value that fits stays one line, coloured; one that does not is laid out.
func TestEvalRender_Values(t *testing.T) {
	fx := evalFixtures(t)
	pv, body := renderFixture(fx["object"])
	if !strings.Contains(stripANSI(pv), "✓ 4 keys") || stripANSI(body) != fx["object"].Text {
		t.Errorf("object: %q / %q", stripANSI(pv), stripANSI(body))
	}
	if body == fx["object"].Text {
		t.Error("the object should be coloured")
	}
	pv, body = renderFixture(fx["hosted_lsp"])
	plain := stripANSI(body)
	if !strings.Contains(stripANSI(pv), "✓ 5 keys") || !strings.Contains(plain, "\n  matches: [") || !strings.Contains(plain, `node: "main.go#Hello"`) {
		t.Errorf("a hosted call's result, laid out:\n%s\n%s", stripANSI(pv), plain)
	}
	for _, ln := range strings.Split(plain, "\n") {
		if lipgloss.Width(ln) > 94 {
			t.Errorf("line wider than the box: %q", ln)
		}
	}
	if pv, _ := renderFixture(fx["number"]); !strings.Contains(stripANSI(pv), "✓ 1180591620717411303424") {
		t.Errorf("a scalar shows itself: %q", stripANSI(pv))
	}
	if pv, _ := renderFixture(fx["array"]); !strings.Contains(stripANSI(pv), "✓ 3 items") {
		t.Errorf("array: %q", stripANSI(pv))
	}
}

// Errors: the message, and where. A runtime error quotes its line and marks
// the column; a syntax error already does both.
func TestEvalRender_Errors(t *testing.T) {
	fx := evalFixtures(t)
	pv, body := renderFixture(fx["runtime_error"])
	if !strings.Contains(stripANSI(pv), "✗ Type mismatch in upper · line 2") {
		t.Errorf("preview: %q", stripANSI(pv))
	}
	lines := strings.Split(stripANSI(body), "\n")
	n := len(lines)
	if !strings.Contains(lines[n-2], "2 │ let b = a |> upper()") || lipgloss.Width(lines[n-1][:strings.Index(lines[n-1], "^")]) != lipgloss.Width(lines[n-2][:strings.Index(lines[n-2], "let")])+8 {
		t.Errorf("the offending line, marked at its column:\n%s", stripANSI(body))
	}
	pv, body = renderFixture(fx["syntax_error"])
	if !strings.Contains(stripANSI(pv), "✗ Syntax error at line 2:0") || !strings.Contains(stripANSI(body), "  let y = 3\n  ^") {
		t.Errorf("syntax error:\n%s\n%s", stripANSI(pv), stripANSI(body))
	}
	if strings.Contains(stripANSI(body), "│") {
		t.Error("a syntax error quotes its own line; it should not be quoted twice")
	}
	if pv, _ := renderFixture(fx["hosted_lsp_error"]); !strings.Contains(stripANSI(pv), `✗ MCP tool error: no node matches "nope.go#Missing" · line 1`) {
		t.Errorf("hosted error: %q", stripANSI(pv))
	}
}

// A long script: its first meaningful line in the step list, the whole thing
// coloured in the tree.
func TestEvalRender_LongScript(t *testing.T) {
	code := evalFixtures(t)["multiline"].Code
	full := evalCallFull(map[string]any{"code": code})
	if stripANSI(full) != code {
		t.Errorf("the tree shows the whole script:\n%s", stripANSI(full))
	}
	if !strings.Contains(full, stKw.Render("let")) {
		t.Error("keywords are coloured")
	}
}

// A user's eval.star still wins over the built-in renderer.
func TestEvalRender_ScriptOverrideWins(t *testing.T) {
	writeScript(t, `
def render(ctx):
    return "mine", "my body"

renderer("eval", render)
`)
	loadScriptRenderers()
	if pv, body := renderToolResult(renderCtx{tool: "eval", result: "[1, 2]"}); pv != "mine" || body != "my body" {
		t.Errorf("the user's eval.star did not win: %q %q", pv, body)
	}
}

// Under its step in a run, a failure keeps what follows its headline — not a
// listing of every command it knows.
func TestEvalFailLines(t *testing.T) {
	fx := evalFixtures(t)
	if got := evalFailLines(asDunSees(fx["unknown_command"])); len(got) != 2 || got[0] != "Did you mean?" || got[1] != "match" {
		t.Errorf("unknown command: %q", got)
	}
	if got := evalFailLines(asDunSees(fx["runtime_error"])); len(got) != 2 || got[0] != "Expected: string" {
		t.Errorf("runtime error: %q", got)
	}
}
