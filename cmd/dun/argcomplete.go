package main

import (
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// Argument completion in the "/" palette.
//
// The palette completes command names; once the input is "/<cmd> " and the
// command has a completer, it lists the argument's choices instead: ↑/↓
// select, tab completes "/<cmd> <value> ", enter runs "/<cmd> <value>". A
// completer gets everything after "/<cmd> " and returns nil when it has
// nothing to offer there (free text after a skill name, say); the palette
// then shows the command again, as it did before completion existed.

// argChoice is one value the argument can take. value replaces the whole
// text after "/<cmd> "; label is what is shown and matched (the last word
// of value, usually); hits are the label's matched rune indexes.
type argChoice struct {
	value, label, source, desc string
	hits                       []int
}

// argList is a completer's answer: the matches, best first, out of total
// candidates, counted as noun ("skills").
type argList struct {
	choices []argChoice
	total   int
	noun    string
}

type argCompleter func(m *tuiModel, arg string) *argList

// paletteWindow caps the palette's rows, commands or arguments; the
// selection scrolls within it (the model and session pickers do the same).
const paletteWindow = 10

// matchRank ranks a query against a name and a description, case-folded:
// 0 a name prefix, 1 a name substring, 2 the query's letters in order in the
// name, 3 a description substring. ok is false for no match. hits are the
// name's matched rune indexes. An empty query matches everything at 0.
func matchRank(query, name, desc string) (rank int, hits []int, ok bool) {
	q := []rune(strings.ToLower(query))
	if len(q) == 0 {
		return 0, nil, true
	}
	n := []rune(name)
	ln := make([]rune, len(n))
	for i, r := range n {
		ln[i] = unicode.ToLower(r)
	}
	span := func(at int) []int {
		h := make([]int, len(q))
		for i := range q {
			h[i] = at + i
		}
		return h
	}
	if at := runeIndex(ln, q); at == 0 {
		return 0, span(0), true
	} else if at > 0 {
		return 1, span(at), true
	}
	j := 0
	for i := 0; i < len(ln) && j < len(q); i++ {
		if ln[i] == q[j] {
			hits = append(hits, i)
			j++
		}
	}
	if j == len(q) {
		return 2, hits, true
	}
	if strings.Contains(strings.ToLower(desc), string(q)) {
		return 3, nil, true
	}
	return 0, nil, false
}

func runeIndex(s, sub []rune) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		match := true
		for k := range sub {
			if s[i+k] != sub[k] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

// rankChoices filters cands by query and orders them by matchRank, keeping
// the given order within a rank.
func rankChoices(query string, cands []argChoice) []argChoice {
	type ranked struct {
		c    argChoice
		rank int
	}
	var rs []ranked
	for _, c := range cands {
		if r, hits, ok := matchRank(query, c.label, c.desc); ok {
			c.hits = hits
			rs = append(rs, ranked{c, r})
		}
	}
	sort.SliceStable(rs, func(i, j int) bool { return rs[i].rank < rs[j].rank })
	out := make([]argChoice, len(rs))
	for i, r := range rs {
		out[i] = r.c
	}
	return out
}

// skillChoices completes /skill's name. Free text after the name is not
// completed.
func skillChoices(m *tuiModel, arg string) *argList {
	if strings.ContainsAny(arg, " \t\n") {
		return nil
	}
	cands := make([]argChoice, len(m.skills))
	for i, s := range m.skills {
		src := s.Source
		if s.UserOnly {
			src += " · /skill only"
		}
		cands[i] = argChoice{value: s.Name, label: s.Name, source: src, desc: s.Description}
	}
	return &argList{rankChoices(arg, cands), len(cands), "skills"}
}

// mcpNeedsID is the /mcp subcommands that take a server id next.
var mcpNeedsID = map[string]bool{"tools": true, "remove": true, "refresh": true, "move": true,
	"login": true, "logout": true, "on": true, "off": true, "restart": true}

// engineChoices completes an engine command's first argument from the
// choices it declared. /mcp also completes the server id after a
// subcommand that takes one.
func engineChoices(c engineCmd) argCompleter {
	return func(m *tuiModel, arg string) *argList {
		f := strings.Fields(arg)
		trailing := strings.TrimRight(arg, " \t\n") != arg
		if c.name == "mcp" && len(f) > 0 && mcpNeedsID[f[0]] && (len(f) == 1 && trailing || len(f) == 2 && !trailing) {
			q := ""
			if len(f) == 2 {
				q = f[1]
			}
			cands := make([]argChoice, len(m.serverIDs))
			for i, id := range m.serverIDs {
				cands[i] = argChoice{value: f[0] + " " + id, label: id}
			}
			return &argList{rankChoices(q, cands), len(cands), "servers"}
		}
		if strings.ContainsAny(arg, " \t\n") {
			return nil
		}
		cands := make([]argChoice, len(c.choices))
		for i, ch := range c.choices {
			cands[i] = argChoice{value: ch.value, label: ch.value, desc: ch.desc}
		}
		return &argList{rankChoices(arg, cands), len(cands), "choices"}
	}
}

// completerFor is the completer of the command called name, or nil.
func (m tuiModel) completerFor(name string) argCompleter {
	if name == "skill" {
		return skillChoices
	}
	for _, c := range m.engineCmds {
		if c.name == name && len(c.choices) > 0 {
			return engineChoices(c)
		}
	}
	return nil
}

// resolveSlash is the command a typed name runs: the exact name, or the
// only one it is a prefix of (as runSlash dispatches).
func (m tuiModel) resolveSlash(name string) (slashCmd, bool) {
	var hit slashCmd
	n := 0
	for _, c := range m.allSlash() {
		if c.name == name {
			return c, true
		}
		if strings.HasPrefix(c.name, name) {
			hit, n = c, n+1
		}
	}
	return hit, n == 1
}

// paletteArgs is the argument completion for the input, or ok=false for
// the command list: the input is "/<cmd> …" and that command's completer
// has something to offer there.
func (m tuiModel) paletteArgs() (cmd string, l *argList, ok bool) {
	v := m.input.Value()
	sp := strings.IndexAny(v, " \t\n")
	if !strings.HasPrefix(v, "/") || sp < 0 {
		return "", nil, false
	}
	c, found := m.resolveSlash(v[1:sp])
	if !found {
		return "", nil, false
	}
	comp := m.completerFor(c.name)
	if comp == nil {
		return "", nil, false
	}
	l = comp(&m, strings.TrimLeft(v[sp:], " \t"))
	if l == nil {
		return "", nil, false
	}
	return c.name, l, true
}

// paletteRows is how many rows the palette lists: its matches, commands or
// arguments.
func (m tuiModel) paletteRows() int {
	if _, l, ok := m.paletteArgs(); ok {
		return len(l.choices)
	}
	return len(m.paletteMatches())
}

// paletteRange is the window of n rows shown around the selection.
func paletteRange(sel, n int) (lo, hi int) {
	lo = max(0, sel-paletteWindow/2)
	hi = min(n, lo+paletteWindow)
	lo = max(0, hi-paletteWindow)
	return lo, hi
}

// hiName renders a choice's label with its matched characters bright.
func hiName(label string, hits []int) string {
	if len(hits) == 0 {
		return stTool.Render(label)
	}
	on := map[int]bool{}
	for _, h := range hits {
		on[h] = true
	}
	var b strings.Builder
	for i, r := range []rune(label) {
		if on[i] {
			b.WriteString(stMatch.Render(string(r)))
		} else {
			b.WriteString(stTool.Render(string(r)))
		}
	}
	return b.String()
}

// stMatch is a matched character in a completion.
var stMatch = lipgloss.NewStyle().Bold(true).Underline(true).Foreground(lipgloss.Color("214"))

// choiceLine is one choice in one row of width w: the name, its source
// dim, and the description cut to what is left.
func choiceLine(c argChoice, w int) string {
	line := hiName(c.label, c.hits)
	if c.source != "" {
		line += stDim.Render("  " + c.source)
	}
	if d := oneLine(c.desc); d != "" {
		if room := w - lipgloss.Width(line) - 2; room > 1 {
			line += "  " + stDim.Render(ansi.Truncate(d, room, "…"))
		}
	}
	return line
}

// argPanel renders the argument choices: a window around the selection and
// the count.
func (m tuiModel) argPanel(cmd string, l *argList) string {
	lo, hi := paletteRange(m.paletteSel, len(l.choices))
	rows := make([]string, 0, hi-lo+1)
	w := max(20, m.w-2)
	for i := lo; i < hi; i++ {
		line := choiceLine(l.choices[i], w)
		if i == m.paletteSel {
			rows = append(rows, addGutter(line, "▎ ", stSel))
		} else {
			rows = append(rows, addGutter(line, "  ", lipgloss.NewStyle()))
		}
	}
	count := fmt.Sprintf("  %d of %d %s", len(l.choices), l.total, l.noun)
	if len(l.choices) == 0 {
		count = fmt.Sprintf("  no match among %d %s", l.total, l.noun)
	} else if lo > 0 || hi < len(l.choices) {
		count += fmt.Sprintf(" · %d-%d shown", lo+1, hi)
	}
	rows = append(rows, stDim.Render(count+" · /"+cmd))
	return strings.Join(rows, "\n")
}
