package tui

import (
	"strings"
	"sync"

	"charm.land/glamour/v2/ansi"
	"charm.land/lipgloss/v2"
	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/lexers"
	xansi "github.com/charmbracelet/x/ansi"
)

// An exec step opened in the pager (toolrun.go's page), USER 2026-11-19: the
// command was drawn twice (head and body), all green, broken mid-token by the
// body's word wrap ("head -" / "12"), and the output's blank runs kept.
//
//	▾ ✓ $ gh run view 44594633270 2>&1 | head -12
//	    in /Users/me/src/app
//	    $ gh run view 44594633270 2>&1
//	        | head -12
//
//	    * dev ci app#1 · 44594633270
//
// The head is the one-line summary, coloured only on its ✓/✗. The body has
// the command once: a leading `cd <dir> &&` is a dim "in <dir>" line, the
// rest is highlighted as bash in the code fences' colours (glamour's chroma
// table for mdStyle, so light and dark follow the answer). A command too wide
// for the pane breaks before each `&&`, `||`, `|` and `;`, a part still too
// wide at its spaces, never inside a word unless the word alone is too wide.
// The output follows in stSecondary, its own SGR colours kept.

// execStepRows is an exec step's body laid out at w columns.
func execStepRows(args map[string]any, output string, w int) []string {
	cmd, _ := args["command"].(string)
	dir, rest := execCdPrefix(cmd)
	var rows []string
	if dir != "" {
		rows = append(rows, fitRow(StDim.Render("in "+dir), w))
	}
	rows = append(rows, execCommandRows(rest, w)...)
	if p := argPreview(execRestArgs(args), 200); p != "" {
		rows = append(rows, fitRow(StDim.Render("("+p+")"), w))
	}
	if strings.TrimSpace(xansi.Strip(output)) == "" {
		return rows
	}
	rows = append(rows, "")
	for _, ln := range strings.Split(output, "\n") {
		rows = append(rows, wrapOutputLine(ln, w)...)
	}
	return rows
}

// execRestArgs is exec's args other than the command (timeout, background).
func execRestArgs(args map[string]any) map[string]any {
	rest := map[string]any{}
	for k, v := range args {
		if k != "command" {
			rest[k] = v
		}
	}
	return rest
}

// wrapOutputLine wraps one output line at w, by word, keeping its styles.
func wrapOutputLine(ln string, w int) []string {
	if ln == "" {
		return []string{""}
	}
	return strings.Split(xansi.Wrap(ln, max(1, w), ""), "\n")
}

// execCdPrefix splits a leading `cd <dir> && ` off a command: the directory
// and the rest. dir is "" when the command does not start so, or <dir> is not
// one plain word (a quoted path with spaces, a substitution).
func execCdPrefix(cmd string) (dir, rest string) {
	s := strings.TrimLeft(cmd, " \t")
	if !strings.HasPrefix(s, "cd ") {
		return "", cmd
	}
	f := strings.TrimLeft(s[3:], " \t")
	end := strings.IndexAny(f, " \t\n;&|")
	if end <= 0 {
		return "", cmd
	}
	d := f[:end]
	if strings.ContainsAny(d, "'\"`$(){}<>\\") {
		return "", cmd
	}
	after := strings.TrimLeft(f[end:], " \t")
	if !strings.HasPrefix(after, "&&") {
		return "", cmd
	}
	r := strings.TrimLeft(after[2:], " \t")
	if strings.TrimSpace(r) == "" {
		return "", cmd
	}
	return d, r
}

// execCommandRows is the command at w columns: "$ " on its first row, a
// continuation indented under the command, each row highlighted.
func execCommandRows(cmd string, w int) []string {
	cmd = strings.TrimRight(strings.ReplaceAll(cmd, "\r\n", "\n"), " \t\n")
	style := shellStyles(cmd)
	var rows []string
	off := 0
	for _, line := range strings.Split(cmd, "\n") {
		for i, seg := range layoutShellLine(line, max(1, w-2), max(1, w-6)) {
			lead := "$ "
			switch {
			case len(rows) > 0 && i == 0:
				lead = "  " // a script's next line
			case i > 0:
				lead = "      "
			}
			rows = append(rows, StDim.Render(lead)+paintSpan(cmd, style, off+seg[0], off+seg[1]))
		}
		off += len(line) + 1
	}
	return rows
}

// layoutShellLine breaks one line of shell into rows: [start, end) byte
// ranges of line. All of it on one row when it fits first; else a row per
// pipeline part (each starting at its operator), a part wider than cont
// wrapped at its spaces, a word wider than that cut.
func layoutShellLine(line string, first, cont int) [][2]int {
	if xansi.StringWidth(line) <= first {
		return [][2]int{{0, len(line)}}
	}
	var out [][2]int
	parts := shellParts(line)
	for pi, p := range parts {
		width := cont
		if pi == 0 {
			width = first
		}
		out = append(out, wrapSpan(line, p[0], p[1], width, cont)...)
	}
	return out
}

// shellParts splits a line before each `&&`, `||`, `|` and `;` outside
// quotes; the spaces around a break are dropped.
func shellParts(line string) [][2]int {
	var cuts []int
	var q byte
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case q != 0:
			if c == '\\' && q == '"' {
				i++
			} else if c == q {
				q = 0
			}
		case c == '\\':
			i++
		case c == '\'' || c == '"' || c == '`':
			q = c
		case c == '&' && i+1 < len(line) && line[i+1] == '&',
			c == '|' && i+1 < len(line) && line[i+1] == '|':
			cuts = append(cuts, i)
			i++
		case c == '|' && (i == 0 || line[i-1] != '>'), c == ';':
			cuts = append(cuts, i)
		}
	}
	var parts [][2]int
	start := 0
	for _, c := range append(cuts, len(line)) {
		a, b := trimSpan(line, start, c)
		if a < b {
			parts = append(parts, [2]int{a, b})
		}
		start = c
	}
	if len(parts) == 0 {
		parts = [][2]int{{0, len(line)}}
	}
	return parts
}

func trimSpan(s string, a, b int) (int, int) {
	for a < b && (s[a] == ' ' || s[a] == '\t') {
		a++
	}
	for b > a && (s[b-1] == ' ' || s[b-1] == '\t') {
		b--
	}
	return a, b
}

// wrapSpan wraps s[a:b] at spaces: the first row at most first columns, the
// rest at most cont; a word wider than its row is cut at the edge.
func wrapSpan(s string, a, b, first, cont int) [][2]int {
	var out [][2]int
	width := first
	for a < b {
		if xansi.StringWidth(s[a:b]) <= width {
			out = append(out, [2]int{a, b})
			break
		}
		// The last space at which s[a:end] still fits.
		end, cut := -1, a
		col := 0
		for i, r := range s[a:b] {
			col += xansi.StringWidth(string(r))
			if col > width {
				break
			}
			cut = a + i + len(string(r))
			if r == ' ' && i > 0 {
				end = a + i
			}
		}
		if end < 0 {
			end = max(cut, a+1) // one word wider than the row
		}
		x, y := trimSpan(s, a, end)
		out = append(out, [2]int{x, y})
		a, _ = trimSpan(s, end, b)
		width = cont
	}
	return out
}

// shellStyles is the style of every byte of cmd, from chroma's bash lexer in
// the code fences' colours; nil when it cannot lex (then it is drawn plain).
func shellStyles(cmd string) []*lipgloss.Style {
	lx := lexers.Get("bash")
	if lx == nil {
		return nil
	}
	it, err := chroma.Coalesce(lx).Tokenise(nil, cmd)
	if err != nil {
		return nil
	}
	table := shellPalette()
	out := make([]*lipgloss.Style, len(cmd))
	at := 0
	for tok := it(); tok != chroma.EOF; tok = it() {
		st := table.of(tok.Type)
		for k := 0; k < len(tok.Value) && at < len(out); k++ {
			out[at] = st
			at++
		}
	}
	return out
}

// paintSpan is s[a:b], each run of one style rendered in it.
func paintSpan(s string, style []*lipgloss.Style, a, b int) string {
	if len(style) != len(s) {
		return s[a:b]
	}
	var sb strings.Builder
	for i := a; i < b; {
		j := i + 1
		for j < b && style[j] == style[i] {
			j++
		}
		if style[i] == nil {
			sb.WriteString(s[i:j])
		} else {
			sb.WriteString(style[i].Render(s[i:j]))
		}
		i = j
	}
	return sb.String()
}

// shellTable maps chroma token types to styles for one mdStyle.
type shellTable struct {
	byType map[chroma.TokenType]*lipgloss.Style
}

// of is the style of a token type: its own, or its category's, or nil
// (plain). An Error token is plain: chroma's lexer times out under load and
// names valid text an error (bubbleteandmd's lextimeout.go).
func (t shellTable) of(tt chroma.TokenType) *lipgloss.Style {
	if tt == chroma.Error {
		return nil
	}
	if s, ok := t.byType[tt]; ok {
		return s
	}
	if s, ok := t.byType[tt.SubCategory()]; ok {
		return s
	}
	return t.byType[tt.Category()]
}

var (
	shellPaletteMu sync.Mutex
	shellPalettes  = map[string]shellTable{}
)

// shellPalette is the table for the current mdStyle, built once per style
// from glamour's code-block chroma colours.
func shellPalette() shellTable {
	shellPaletteMu.Lock()
	defer shellPaletteMu.Unlock()
	if t, ok := shellPalettes[mdStyle]; ok {
		return t
	}
	t := shellTable{byType: map[chroma.TokenType]*lipgloss.Style{}}
	if c := mdStyleConfig().CodeBlock.Chroma; c != nil {
		for tt, p := range map[chroma.TokenType]ansi.StylePrimitive{
			chroma.Comment:             c.Comment,
			chroma.CommentPreproc:      c.CommentPreproc,
			chroma.Keyword:             c.Keyword,
			chroma.KeywordReserved:     c.KeywordReserved,
			chroma.KeywordNamespace:    c.KeywordNamespace,
			chroma.KeywordType:         c.KeywordType,
			chroma.Operator:            c.Operator,
			chroma.Punctuation:         c.Punctuation,
			chroma.NameBuiltin:         c.NameBuiltin,
			chroma.NameFunction:        c.NameFunction,
			chroma.NameVariable:        c.NameOther,
			chroma.LiteralNumber:       c.LiteralNumber,
			chroma.LiteralString:       c.LiteralString,
			chroma.LiteralStringEscape: c.LiteralStringEscape,
		} {
			if p.Color == nil {
				continue
			}
			s := lipgloss.NewStyle().Foreground(lipgloss.Color(*p.Color))
			if p.Bold != nil && *p.Bold {
				s = s.Bold(true)
			}
			t.byType[tt] = &s
		}
	}
	shellPalettes[mdStyle] = t
	return t
}

// ── the output ─────────────────────────────────────────────────────

// execOutput is a command's output as the step's body shows it: SGR colours
// kept and every other control sequence dropped (a cursor move or a title
// set would draw on the TUI's screen), a carriage return's overwrite kept
// as the terminal would show it, a run of blank lines one, and the edges
// trimmed. A line with no colours of its own is stSecondary; exec's own
// "[exit: …]" line is StErr.
func execOutput(result string) string {
	lines := strings.Split(strings.ReplaceAll(result, "\r\n", "\n"), "\n")
	var out []string
	blank := true // drops leading blank lines
	for _, ln := range lines {
		if i := strings.LastIndexByte(ln, '\r'); i >= 0 {
			ln = ln[i+1:]
		}
		ln = keepSGR(ln)
		if strings.TrimSpace(xansi.Strip(ln)) == "" {
			if !blank {
				out = append(out, "")
			}
			blank = true
			continue
		}
		blank = false
		out = append(out, ln)
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	if len(out) == 0 {
		return ""
	}
	marker := -1
	if _, failed := execExitMarker(result); failed {
		marker = len(out) - 1
	}
	for i, ln := range out {
		switch {
		case ln == "":
		case i == marker:
			out[i] = StErr.Render(ln)
		case !strings.Contains(ln, "\x1b"):
			out[i] = stSecondary.Render(ln)
		}
	}
	return strings.Join(out, "\n")
}

// keepSGR is s with its SGR sequences (colours) and printable text, and no
// other escape or control character; a tab is kept.
func keepSGR(s string) string {
	if !strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) }) {
		return s
	}
	var b strings.Builder
	var st byte
	p := xansi.GetParser()
	defer xansi.PutParser(p)
	for len(s) > 0 {
		seq, _, n, newState := xansi.DecodeSequence(s, st, p)
		st = newState
		switch {
		case xansi.HasCsiPrefix(seq) && strings.HasSuffix(seq, "m"):
			b.WriteString(seq)
		case len(seq) == 1 && seq[0] == '\t':
			b.WriteString(seq)
		case len(seq) == 1 && (seq[0] < 0x20 || seq[0] == 0x7f):
		case strings.HasPrefix(seq, "\x1b") || strings.HasPrefix(seq, "\u009b") || strings.HasPrefix(seq, "\u009d"):
		default:
			b.WriteString(seq)
		}
		s = s[n:]
	}
	return b.String()
}
