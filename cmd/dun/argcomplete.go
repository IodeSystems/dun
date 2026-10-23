package main

import (
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/iodesystems/dun"
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

// mcpArgs is what each /mcp subcommand takes after it, position by
// position: a server id, a preset, a mode, a tool switch, a scope flag, or
// "" for free text (a tool name). The /raglit and /poly-lsp switches take a scope.
var mcpArgs = map[string][]string{
	"tools": {"id"}, "remove": {"id"}, "rm": {"id"}, "refresh": {"id"}, "login": {"id"}, "logout": {"id"}, "restart": {"id"},
	"on": {"id", "scope"}, "off": {"id", "scope"}, "unset": {"id", "unsetScope"},
	"mode": {"id", "mode", "scope"}, "via": {"id", "mode", "scope"}, "move": {"id", "mode", "scope"},
	"toggle": {"id", "", "tool", "scope"},
	"add":    {"preset"},
}

var srvArgs = map[string][]string{"on": {"scope"}, "off": {"scope"}, "unset": {"unsetScope"}}

// mcpArgCands are the candidates of one argument kind.
func mcpArgCands(m *tuiModel, kind string) ([]argChoice, string) {
	var out []argChoice
	add := func(v, d string) { out = append(out, argChoice{value: v, label: v, desc: d}) }
	switch kind {
	case "id":
		for _, id := range m.serverIDs {
			add(id, "")
		}
		return out, "servers"
	case "preset":
		for _, p := range dun.ServerPresets {
			add(p.ID, p.Description)
		}
		return out, "presets"
	case "mode":
		add(dun.ModeStandalone, "its own tools, schemas in every request")
		add(dun.ModeScripted, "in mcpshell, called through eval, schemas out of the prompt")
		add("unset", "clear this scope's mode")
		return out, "modes"
	case "tool":
		add("on", "")
		add("off", "")
		add("unset", "clear this scope's switch")
		return out, "switches"
	case "scope", "unsetScope":
		if kind == "scope" {
			add("--global", "every workspace ($DUN_HOME/mcp.json)")
		}
		add("--project", "this project")
		add("--session", "this session (the default)")
		return out, "scopes"
	}
	return nil, ""
}

// positionalChoices completes the argument after a subcommand from kinds:
// the argument being typed is f's last word, or the next one when arg ends
// in a space.
func positionalChoices(m *tuiModel, arg string, kinds map[string][]string) *argList {
	f := strings.Fields(arg)
	trailing := strings.TrimRight(arg, " \t\n") != arg
	if len(f) == 0 {
		return nil
	}
	spec, ok := kinds[f[0]]
	if !ok {
		return nil
	}
	pos, q := len(f)-1, f[len(f)-1]
	if trailing {
		pos, q = len(f), ""
	}
	if pos == 0 || pos > len(spec) {
		return nil
	}
	cands, what := mcpArgCands(m, spec[pos-1])
	if cands == nil {
		return nil
	}
	prefix := strings.Join(f[:pos], " ") + " "
	for i := range cands {
		cands[i].value = prefix + cands[i].value
	}
	return &argList{rankChoices(q, cands), len(cands), what}
}

// engineChoices completes an engine command's first argument from the
// choices it declared. /mcp also completes the server id after a
// subcommand that takes one.
func engineChoices(c engineCmd) argCompleter {
	return func(m *tuiModel, arg string) *argList {
		switch c.name {
		case "mcp":
			if l := positionalChoices(m, arg, mcpArgs); l != nil {
				return l
			}
		case "raglit", "poly-lsp":
			if l := positionalChoices(m, arg, srvArgs); l != nil {
				return l
			}
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
