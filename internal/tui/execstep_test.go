package tui

import (
	"strings"
	"testing"

	"github.com/alecthomas/chroma/v2"
	"github.com/charmbracelet/x/ansi"
)

// execPage is a two-call run (so the pager draws a tree) opened on its first
// call, an exec of cmd with result, laid out at w.
func execPage(cmd, result string, failed bool, w int) pager {
	args := map[string]any{"command": cmd}
	_, body := renderToolResult(renderCtx{tool: "exec", args: args, result: result, width: w})
	it := &runItem{tool: "exec", args: args, result: result, done: true, failed: failed, output: body}
	other := &runItem{tool: "exec", args: map[string]any{"command": "true"}, done: true}
	r := &toolRun{items: []*runItem{it, other}}
	p := newPager(0, r)
	p.open[0] = true
	p.layout(r.page(), w, 40)
	return p
}

// The user's report (2026-11-19): the command once in the body, its cd as a
// dim "in" line, the output's blank runs gone.
func TestExecStep_CommandOnce(t *testing.T) {
	cmd := "cd /Users/me/src/vehicle-dwell-safety && gh run view 44594633270 2>&1 | head -12"
	res := "\n\n* dev ci app#1 · 44594633270\n\n\n\nTriggered via push\n\n"
	p := execPage(cmd, res, false, 100)
	body := p.rows[1:p.nodeAt[1]]
	var plain []string
	for _, r := range body {
		plain = append(plain, strings.TrimSpace(ansi.Strip(r)))
	}
	got := strings.Join(plain, "\n")
	want := "in /Users/me/src/vehicle-dwell-safety\n$ gh run view 44594633270 2>&1 | head -12\n\n* dev ci app#1 · 44594633270\n\nTriggered via push"
	if got != want {
		t.Fatalf("body:\n%s\nwant:\n%s", got, want)
	}
	if head := ansi.Strip(p.rows[0]); !strings.Contains(head, "✓ $ gh run view 44594633270") {
		t.Errorf("head = %q, want the command without its cd", head)
	}
}

// Only the glyph has the status colour; the head's text and the output
// are not green (nor red on a failure).
func TestExecStep_StatusColourOnGlyph(t *testing.T) {
	green, red := ansiFG(StTool), ansiFG(StErr)
	p := execPage("go test ./...", "ok  pkg\n", false, 80)
	head := p.rows[0]
	if !strings.Contains(head, StTool.Render("✓ ")) {
		t.Fatalf("head has no green ✓: %q", head)
	}
	if strings.Count(head, green) != 1 {
		t.Errorf("green beyond the glyph: %q", head)
	}
	for _, r := range p.rows[1:p.nodeAt[1]] {
		if strings.Contains(r, green) {
			t.Errorf("green in the body: %q", r)
		}
	}
	p = execPage("false", "boom\n[exit: status 1]", true, 80)
	head = p.rows[0]
	if !strings.Contains(head, StErr.Render("✗ ")) || strings.Count(head, red) != 1 {
		t.Errorf("failed head: want red on the ✗ alone: %q", head)
	}
	body := strings.Join(p.rows[1:p.nodeAt[1]], "\n")
	if !strings.Contains(body, StErr.Render("[exit: status 1]")) || strings.Contains(body, StErr.Render("boom")) {
		t.Errorf("failed body: want the exit line red, the output not: %q", body)
	}
}

// ansiFG is the SGR a style opens with.
func ansiFG(s interface{ Render(...string) string }) string {
	r := s.Render("x")
	return r[:strings.Index(r, "x")]
}

// The command is highlighted as bash: a string and a keyword get colours of
// their own, the text between them none.
func TestExecStep_Highlighted(t *testing.T) {
	rows := execCommandRows(`echo "hello world" && ls`, 80)
	if len(rows) != 1 {
		t.Fatalf("rows: %q", rows)
	}
	r := rows[0]
	if ansi.Strip(r) != `$ echo "hello world" && ls` {
		t.Fatalf("text: %q", ansi.Strip(r))
	}
	str := shellPalette().of(chroma.LiteralStringDouble)
	if str == nil || !strings.Contains(r, str.Render(`"hello world"`)) {
		t.Errorf("the string is not in its own colour: %q", r)
	}
	if kw := shellPalette().of(chroma.NameBuiltin); kw == nil || !strings.Contains(r, kw.Render("echo")) {
		t.Errorf("echo is not in the builtin colour: %q", r)
	}
}

// A command too wide breaks before its operators, a part still too wide at
// its spaces; never mid-word, every word kept in order.
func TestExecStep_Wrap(t *testing.T) {
	cmd := "gh run view 44594633270 --log-failed 2>&1 | grep -v '^$' | head -12 && echo done-and-dusted"
	rows := execCommandRows(cmd, 30)
	var plain []string
	for _, r := range rows {
		s := ansi.Strip(r)
		if w := ansi.StringWidth(s); w > 30 {
			t.Errorf("row %q is %d wide", s, w)
		}
		plain = append(plain, s)
	}
	if !strings.HasPrefix(plain[0], "$ gh run view") {
		t.Errorf("first row: %q", plain[0])
	}
	want := []string{"| grep -v '^$'", "| head -12", "&& echo done-and-dusted"}
	for _, w := range want {
		found := false
		for _, p := range plain[1:] {
			if strings.TrimSpace(p) == w {
				found = true
			}
		}
		if !found {
			t.Errorf("no row %q in:\n%s", w, strings.Join(plain, "\n"))
		}
	}
	all := strings.Fields(strings.TrimPrefix(strings.Join(plain, " "), "$ "))
	if strings.Join(all, " ") != strings.Join(strings.Fields(cmd), " ") {
		t.Errorf("words changed:\n%q\nwant\n%q", all, strings.Fields(cmd))
	}
	// A part that fits is never cut: 3 rows for 3 parts.
	if got := execCommandRows("make build && make test && make lint", 20); len(got) != 3 {
		t.Errorf("parts: %q", got)
	}
	// A quoted | is not an operator.
	if got := shellParts(`echo "a | b" | wc`); len(got) != 2 {
		t.Errorf("quoted pipe split: %v", got)
	}
}

func TestExecStep_CdPrefix(t *testing.T) {
	for _, c := range []struct{ in, dir, rest string }{
		{"cd /a/b && ls", "/a/b", "ls"},
		{"cd /a/b; ls", "", "cd /a/b; ls"},
		{`cd "/a b" && ls`, "", `cd "/a b" && ls`},
		{"cd /a && ", "", "cd /a && "},
		{"ls", "", "ls"},
	} {
		d, r := execCdPrefix(c.in)
		if d != c.dir || r != c.rest {
			t.Errorf("%q: got (%q, %q), want (%q, %q)", c.in, d, r, c.dir, c.rest)
		}
	}
}

// Output: blank runs one, edges trimmed, colours kept, other control
// sequences dropped, a carriage return's overwrite resolved.
func TestExecOutput(t *testing.T) {
	if got := ansi.Strip(execOutput("\n\na\n\n\n\nb\n\n\n")); got != "a\n\nb" {
		t.Errorf("blank runs: %q", got)
	}
	red := "\x1b[31mFAIL\x1b[0m"
	got := execOutput(red + " x\x1b]0;title\x07\x1b[2J\x1b[H\n10%\r50%\r100%\n")
	if !strings.Contains(got, red+" x") {
		t.Errorf("SGR lost: %q", got)
	}
	if strings.Contains(got, "\x1b]") || strings.Contains(got, "\x1b[2J") || strings.Contains(got, "\x1b[H") || strings.Contains(got, "\r") {
		t.Errorf("control sequence kept: %q", got)
	}
	if !strings.HasSuffix(ansi.Strip(got), "\n100%") {
		t.Errorf("carriage return: %q", ansi.Strip(got))
	}
	if strings.Contains(got, ansiFG(stSecondary)+red) {
		t.Errorf("a coloured line was restyled: %q", got)
	}
}
