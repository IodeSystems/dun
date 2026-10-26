package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
)

// mcpshell's eval, rendered from what it actually emits (fixtures in
// testdata/mcpshell, captured from its MCP server).
//
// The call is CODE, so it shows as code: a one-liner inline, a script as its
// first meaningful line and "+K lines" (the whole script, coloured, in the
// tree). Two shapes are not scripts at all and read as what they are:
//
//	◇ ns.tool  k=v, …    a single hosted call — another MCP server, through mcpshell
//	help ns              help("ns")
//
// The result is one text block. A success is the value's Display: strings
// bare, everything else in mcpshell's own literal syntax — `{name: "Alice",
// tags: ["a"]}`, keys unquoted, strings quoted. An error is "ERROR: …" (and
// dun's dispatcher adds "ERROR: mcp tool error: " in front), and carries its
// position as "at line L:C" — a syntax error also quotes the line with a
// caret. There is no separate printed output: print() writes to the server's
// stdout, not into the result.
//
// So: a list of records is a table, other values are laid out and coloured,
// an error is marked and points at its line. A user's
// ~/.dun/renderers/eval.star still replaces the result renderer
// (render_script.go loads after this registers).

var (
	stKey = lipgloss.NewStyle().Foreground(lipgloss.Color("75"))
	stStr = lipgloss.NewStyle().Foreground(lipgloss.Color("114"))
	stNum = lipgloss.NewStyle().Foreground(lipgloss.Color("179"))
	stLit = lipgloss.NewStyle().Foreground(lipgloss.Color("176"))
	stKw  = lipgloss.NewStyle().Foreground(lipgloss.Color("213"))
)

var (
	reHosted  = regexp.MustCompile(`^([A-Za-z_]\w*)\.([A-Za-z_]\w*)\((.*)\)\s*;?$`)
	reHelp    = regexp.MustCompile(`^help\(\s*(?:"([^"]*)"|'([^']*)')?\s*\)\s*;?$`)
	reAtLine  = regexp.MustCompile(`at line (\d+):(\d+)`)
	reSyntax  = regexp.MustCompile(`^Syntax error at line (\d+):(\d+)`)
	mcpshKeys = map[string]bool{"let": true, "function": true, "if": true, "else": true, "while": true, "for": true,
		"of": true, "in": true, "return": true, "break": true, "continue": true, "export": true, "switch": true,
		"case": true, "default": true, "do": true, "try": true, "catch": true, "finally": true, "throw": true,
		"typeof": true, "delete": true}
)

func evalCode(args map[string]any) string {
	c, _ := args["code"].(string)
	return strings.TrimSpace(c)
}

// evalCallText is the call's one line: the hosted call, help, or the code's
// first meaningful line with how many more there are.
func evalCallText(args map[string]any, max int) (string, bool) {
	code := evalCode(args)
	if code == "" {
		return "", false
	}
	if !strings.Contains(code, "\n") {
		if m := reHelp.FindStringSubmatch(code); m != nil {
			return strings.TrimSpace("help " + m[1] + m[2]), true
		}
		if m := reHosted.FindStringSubmatch(code); m != nil {
			return clip("◇ "+m[1]+"."+m[2]+"  "+hostedArgs(m[3]), max), true
		}
		return clip(code, max), true
	}
	lines := strings.Split(code, "\n")
	first, rest := "", 0
	for i, ln := range lines {
		t := strings.TrimSpace(ln)
		if t == "" || strings.HasPrefix(t, "//") {
			continue
		}
		first, rest = t, len(lines)-i-1
		break
	}
	if rest > 0 {
		return clip(first, max) + fmt.Sprintf("  +%d lines", rest), true
	}
	return clip(first, max), true
}

// hostedArgs is a hosted call's arguments as k=v: a single object literal is
// split into its fields, anything else shown as written.
func hostedArgs(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "{") || !strings.HasSuffix(s, "}") {
		return s
	}
	var parts []string
	for _, f := range splitTop(s[1 : len(s)-1]) {
		k, v, ok := strings.Cut(f, ":")
		if !ok {
			parts = append(parts, strings.TrimSpace(f))
			continue
		}
		k = strings.Trim(strings.TrimSpace(k), `"'`)
		parts = append(parts, k+"="+clip(strings.TrimSpace(v), 24))
	}
	return strings.Join(parts, ", ")
}

// splitTop splits s at commas outside brackets and quotes.
func splitTop(s string) []string {
	var out []string
	depth, start := 0, 0
	var q byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case q != 0:
			if c == '\\' {
				i++
			} else if c == q {
				q = 0
			}
		case c == '"' || c == '\'' || c == '`':
			q = c
		case c == '{' || c == '[' || c == '(':
			depth++
		case c == '}' || c == ']' || c == ')':
			depth--
		case c == ',' && depth == 0:
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if strings.TrimSpace(s[start:]) != "" {
		out = append(out, s[start:])
	}
	return out
}

// evalCallFull is the whole call for the tree: the code, coloured.
func evalCallFull(args map[string]any) string {
	head, _ := evalCallText(args, 200)
	code := evalCode(args)
	if !strings.Contains(code, "\n") {
		return stTool.Render(head)
	}
	return highlightShell(code)
}

// highlightShell colours mcpshell code by token: keywords, strings, numbers,
// comments. Everything else as written.
func highlightShell(code string) string {
	var b strings.Builder
	for i := 0; i < len(code); {
		c := code[i]
		switch {
		case c == '/' && i+1 < len(code) && code[i+1] == '/':
			j := strings.IndexByte(code[i:], '\n')
			if j < 0 {
				j = len(code) - i
			}
			b.WriteString(stDim.Render(code[i : i+j]))
			i += j
		case c == '"' || c == '\'' || c == '`':
			j := i + 1
			for j < len(code) && code[j] != c && code[j] != '\n' {
				if code[j] == '\\' {
					j++
				}
				j++
			}
			j = min(j+1, len(code))
			b.WriteString(stStr.Render(code[i:j]))
			i = j
		case c >= '0' && c <= '9':
			j := i
			for j < len(code) && (code[j] >= '0' && code[j] <= '9' || code[j] == '.' || code[j] == '_') {
				j++
			}
			b.WriteString(stNum.Render(code[i:j]))
			i = j
		case c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z':
			j := i
			for j < len(code) && (code[j] == '_' || code[j] >= 'a' && code[j] <= 'z' || code[j] >= 'A' && code[j] <= 'Z' || code[j] >= '0' && code[j] <= '9') {
				j++
			}
			w := code[i:j]
			switch {
			case mcpshKeys[w]:
				b.WriteString(stKw.Render(w))
			case w == "true" || w == "false" || w == "null":
				b.WriteString(stLit.Render(w))
			default:
				b.WriteString(w)
			}
			i = j
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

// evalError is the message of a failed eval, without what the layers put
// around it; ok=false for a success. As it reaches dun:
//
//	ERROR: mcp tool error: [ERROR: Type mismatch in upper …]
//
// mcpshell's own "ERROR: ", agentkit's "mcp tool error: " around the list of
// the result's text blocks (hence the brackets), and dun's "ERROR: ".
func evalError(result string) (string, bool) {
	r := strings.TrimSpace(result)
	ok := false
	for {
		switch {
		case strings.HasPrefix(r, "ERROR: "):
			r, ok = strings.TrimPrefix(r, "ERROR: "), true
		case ok && strings.HasPrefix(r, "mcp tool error: "):
			r = strings.TrimPrefix(r, "mcp tool error: ")
			if strings.HasPrefix(r, "[") && strings.HasSuffix(r, "]") {
				r = r[1 : len(r)-1]
			}
		default:
			return r, ok
		}
	}
}

// evalFailLines are the lines of an eval error worth keeping under its step:
// what came after the headline, up to where a long listing starts.
func evalFailLines(result string) []string {
	msg, _ := evalError(result)
	lines := meaningfulLines(msg)
	var out []string
	for _, ln := range lines[min(1, len(lines)):] {
		if strings.HasPrefix(ln, "Available commands") || len(out) == 2 {
			break
		}
		out = append(out, ln)
	}
	return out
}

func evalRender(rc renderCtx) (string, string) {
	result, warns := evalWarnings(rc.result)
	if msg, failed := evalError(result); failed {
		pv, body := evalErrorRender(msg, evalCode(rc.args), rc.width)
		return pv, withWarnings(body, warns)
	}
	r := strings.TrimRight(result, "\n")
	if strings.TrimSpace(r) == "" {
		return stDim.Render("  → ") + stTool.Render("✓") + stSecondary.Render(" (no output)"), withWarnings("", warns)
	}
	v, parsed := parseShellValue(r)
	if !parsed {
		// A string result is shown bare; it is just text.
		return stDim.Render("  → ") + stTool.Render("✓") + stSecondary.Render(" "+evalShape(nil, r)), withWarnings(stSecondary.Render(r), warns)
	}
	w := max(20, rc.width-6)
	body := ""
	if rows, cols, ok := asTable(v); ok {
		body = renderTable(rows, cols, w)
	} else {
		body = strings.Join(prettyValue(v, 0, w), "\n")
	}
	return stDim.Render("  → ") + stTool.Render("✓") + stSecondary.Render(" "+evalShape(v, r)), withWarnings(body, warns)
}

// evalWarnings takes the "warning: line N: …" lines mcpshell (v0.6.0)
// appends after a value or an error off the end of a result, so the value
// still parses (a table stays a table).
func evalWarnings(result string) (string, []string) {
	lines := strings.Split(strings.TrimRight(result, "\n"), "\n")
	end := len(lines)
	for end > 0 && strings.HasPrefix(lines[end-1], "warning: line ") {
		end--
	}
	if end == len(lines) {
		return result, nil
	}
	return strings.Join(lines[:end], "\n"), lines[end:]
}

// withWarnings is body with the warnings under it, in the attention colour.
func withWarnings(body string, warns []string) string {
	for _, w := range warns {
		if body != "" {
			body += "\n"
		}
		body += stNote.Render(w)
	}
	return body
}

// evalShape is the preview's word for a result: how many rows, items or
// keys, or the value itself when it is short.
func evalShape(v any, raw string) string {
	switch x := v.(type) {
	case []any:
		if _, _, ok := asTable(x); ok {
			return plural(len(x), "row")
		}
		return plural(len(x), "item")
	case *shellObj:
		return plural(len(x.keys), "key")
	}
	return clip(oneLine(firstLine(raw)), 70)
}

func evalErrorRender(msg, code string, width int) (string, string) {
	first := firstLine(msg)
	where := ""
	line, col := 0, 0
	if m := reAtLine.FindStringSubmatch(msg); m != nil {
		line, _ = strconv.Atoi(m[1])
		col, _ = strconv.Atoi(m[2])
		if !reSyntax.MatchString(first) {
			where = " · line " + m[1]
		}
	}
	preview := stDim.Render("  → ") + stErr.Render("✗ "+clip(first, 80)) + stSecondary.Render(where)
	var out []string
	for _, ln := range strings.Split(strings.TrimRight(msg, "\n"), "\n") {
		out = append(out, stErr.Render(ln))
	}
	// A runtime error names the line but does not quote it: show it, marked.
	if line > 0 && !reSyntax.MatchString(first) {
		if lines := strings.Split(code, "\n"); line <= len(lines) {
			src, gutter := lines[line-1], fmt.Sprintf("  %d │ ", line)
			out = append(out, "", stDim.Render(gutter)+highlightShell(src),
				strings.Repeat(" ", lipgloss.Width(gutter)+col)+stErr.Render("^"))
		}
	}
	return preview, strings.Join(out, "\n")
}

// ── mcpshell values ────────────────────────────────────────────────

// shellObj is a parsed mcpshell object, keys in their order.
type shellObj struct {
	keys []string
	vals map[string]any
}

// shellNum is a number as mcpshell printed it (exact: kept as text).
type shellNum string

// parseShellValue reads a value in mcpshell's Display syntax. Strings inside
// are printed without escaping, so a quote inside one is resolved by what
// follows it; anything that does not read cleanly is not a value (ok=false)
// and is shown as text.
func parseShellValue(s string) (any, bool) {
	p := &shellParser{s: s}
	v, ok := p.value()
	p.space()
	if !ok || p.i != len(p.s) {
		return nil, false
	}
	switch v.(type) {
	case []any, *shellObj:
		return v, true
	}
	return nil, false // a scalar reads fine as text
}

type shellParser struct {
	s string
	i int
}

func (p *shellParser) space() {
	for p.i < len(p.s) && (p.s[p.i] == ' ' || p.s[p.i] == '\n' || p.s[p.i] == '\t') {
		p.i++
	}
}

func (p *shellParser) value() (any, bool) {
	p.space()
	if p.i >= len(p.s) {
		return nil, false
	}
	switch c := p.s[p.i]; {
	case c == '[':
		p.i++
		arr := []any{}
		p.space()
		if p.i < len(p.s) && p.s[p.i] == ']' {
			p.i++
			return arr, true
		}
		for {
			v, ok := p.value()
			if !ok {
				return nil, false
			}
			arr = append(arr, v)
			p.space()
			if p.i < len(p.s) && p.s[p.i] == ',' {
				p.i++
				continue
			}
			if p.i < len(p.s) && p.s[p.i] == ']' {
				p.i++
				return arr, true
			}
			return nil, false
		}
	case c == '{':
		p.i++
		o := &shellObj{vals: map[string]any{}}
		p.space()
		if p.i < len(p.s) && p.s[p.i] == '}' {
			p.i++
			return o, true
		}
		for {
			p.space()
			j := strings.Index(p.s[p.i:], ": ")
			if j <= 0 {
				return nil, false
			}
			k := p.s[p.i : p.i+j]
			p.i += j + 2
			v, ok := p.value()
			if !ok {
				return nil, false
			}
			if _, dup := o.vals[k]; !dup {
				o.keys = append(o.keys, k)
			}
			o.vals[k] = v
			p.space()
			if p.i < len(p.s) && p.s[p.i] == ',' {
				p.i++
				continue
			}
			if p.i < len(p.s) && p.s[p.i] == '}' {
				p.i++
				return o, true
			}
			return nil, false
		}
	case c == '"':
		// The closing quote is the first one followed by a delimiter.
		for j := p.i + 1; j < len(p.s); j++ {
			if p.s[j] != '"' {
				continue
			}
			rest := strings.TrimLeft(p.s[j+1:], " \n")
			if rest == "" || rest[0] == ',' || rest[0] == ']' || rest[0] == '}' {
				v := p.s[p.i+1 : j]
				p.i = j + 1
				return v, true
			}
		}
		return nil, false
	default:
		j := p.i
		for j < len(p.s) && !strings.ContainsRune(",]} \n", rune(p.s[j])) {
			j++
		}
		w := p.s[p.i:j]
		p.i = j
		switch w {
		case "true":
			return true, true
		case "false":
			return false, true
		case "null":
			return nil, true
		}
		if _, err := strconv.ParseFloat(strings.TrimPrefix(w, "-"), 64); err == nil || isRatio(w) {
			return shellNum(w), true
		}
		return nil, false
	}
}

func isRatio(w string) bool {
	a, b, ok := strings.Cut(w, "/")
	if !ok {
		return false
	}
	_, e1 := strconv.Atoi(strings.TrimPrefix(a, "-"))
	_, e2 := strconv.Atoi(b)
	return e1 == nil && e2 == nil
}

// asTable reports whether v is a list of records: every element an object.
// The columns are the keys in order of first appearance.
func asTable(v any) ([]*shellObj, []string, bool) {
	arr, ok := v.([]any)
	if !ok || len(arr) == 0 {
		return nil, nil, false
	}
	var rows []*shellObj
	var cols []string
	seen := map[string]bool{}
	for _, e := range arr {
		o, ok := e.(*shellObj)
		if !ok || len(o.keys) == 0 {
			return nil, nil, false
		}
		rows = append(rows, o)
		for _, k := range o.keys {
			if !seen[k] {
				seen[k] = true
				cols = append(cols, k)
			}
		}
	}
	return rows, cols, true
}

// renderTable lays records out as aligned columns, clipped to w: each column
// at most 30 wide, and columns that do not fit dropped with a note.
func renderTable(rows []*shellObj, cols []string, w int) string {
	cell := func(o *shellObj, k string) string {
		v, ok := o.vals[k]
		if !ok {
			return ""
		}
		return oneLine(compactValue(v))
	}
	widths := make([]int, len(cols))
	for i, c := range cols {
		widths[i] = lipgloss.Width(c)
		for _, r := range rows {
			widths[i] = max(widths[i], lipgloss.Width(cell(r, c)))
		}
		widths[i] = min(widths[i], 30)
	}
	shown, used := 0, 0
	for i := range cols {
		if used+widths[i]+2 > w && shown > 0 {
			break
		}
		used += widths[i] + 2
		shown++
	}
	pad := func(s string, n int) string {
		s, _ = fitPlain(s, n)
		return s + strings.Repeat(" ", max(0, n-lipgloss.Width(s)))
	}
	var head, rule []string
	for i := 0; i < shown; i++ {
		head = append(head, stKey.Render(pad(cols[i], widths[i])))
		rule = append(rule, stDim.Render(strings.Repeat("─", widths[i])))
	}
	out := []string{strings.Join(head, "  "), strings.Join(rule, "  ")}
	for _, r := range rows {
		var cs []string
		for i := 0; i < shown; i++ {
			cs = append(cs, styleScalar(r.vals[cols[i]], pad(cell(r, cols[i]), widths[i])))
		}
		out = append(out, strings.Join(cs, "  "))
	}
	if shown < len(cols) {
		out = append(out, stSecondary.Render(fmt.Sprintf("+%d more columns: %s", len(cols)-shown, strings.Join(cols[shown:], ", "))))
	}
	return strings.Join(out, "\n")
}

// styleScalar colours a table cell by the type of its value.
func styleScalar(v any, text string) string {
	switch v.(type) {
	case string:
		return stStr.Render(text)
	case shellNum:
		return stNum.Render(text)
	case bool, nil:
		return stLit.Render(text)
	}
	return text
}

// compactValue is v on one line in mcpshell's syntax, strings bare at the top.
func compactValue(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case shellNum:
		return string(x)
	case bool:
		return strconv.FormatBool(x)
	case nil:
		return "null"
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = inspectValue(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case *shellObj:
		parts := make([]string, len(x.keys))
		for i, k := range x.keys {
			parts[i] = k + ": " + inspectValue(x.vals[k])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return fmt.Sprint(v)
}

func inspectValue(v any) string {
	if s, ok := v.(string); ok {
		return `"` + s + `"`
	}
	return compactValue(v)
}

// prettyValue lays v out over lines, coloured: a container that fits on one
// line stays on one; otherwise one entry per line, indented.
func prettyValue(v any, indent, w int) []string {
	pre := strings.Repeat("  ", indent)
	if flat := inspectValue(v); lipgloss.Width(pre+flat) <= w {
		return []string{pre + colourValue(v)}
	}
	switch x := v.(type) {
	case []any:
		out := []string{pre + "["}
		for _, e := range x {
			sub := prettyValue(e, indent+1, w)
			sub[len(sub)-1] += ","
			out = append(out, sub...)
		}
		return append(out, pre+"]")
	case *shellObj:
		out := []string{pre + "{"}
		for _, k := range x.keys {
			sub := prettyValue(x.vals[k], indent+1, w)
			sub[0] = pre + "  " + stKey.Render(k) + ": " + strings.TrimLeft(sub[0], " ")
			sub[len(sub)-1] += ","
			out = append(out, sub...)
		}
		return append(out, pre+"}")
	}
	s, _ := fitPlain(inspectValue(v), max(1, w-len(pre)))
	return []string{pre + styleScalar(v, s)}
}

// colourValue is v on one line, coloured by type.
func colourValue(v any) string {
	switch x := v.(type) {
	case string:
		return stStr.Render(`"` + x + `"`)
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = colourValue(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case *shellObj:
		parts := make([]string, len(x.keys))
		for i, k := range x.keys {
			parts[i] = stKey.Render(k) + ": " + colourValue(x.vals[k])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return styleScalar(v, compactValue(v))
}
