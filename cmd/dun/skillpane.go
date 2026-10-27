package main

import (
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/iodesystems/dun"
)

// The skill reader (owner, 2026-10-27): enter on a skill in /skill opens a
// pane below the list with the skill rendered as a reply is (glamour), its
// front matter a header. A path to a file in the skill's folder is a link,
// an @path is inlined (dun/skillview.go says which is which). On a phone
// (under narrowW) the pane takes the whole screen; the list comes back when
// it closes.
//
//	↑/↓ pgup/pgdn home/end   scroll
//	n/p, tab/shift+tab        the next, the previous link
//	enter                     open the selected link
//	backspace, esc            back; esc on the skill itself closes the pane
//	/                         search the text and the links' targets;
//	                          ↓/↑ (ctrl+n/p) the next, the previous match,
//	                          enter keeps it, esc drops it
//
// The engine reads the files (control "skillfile"), so a client attached
// through dund reads the engine's disk.

// Markers put in the markdown before glamour renders it, found in what it
// renders after: a link's start and end (private-use runes: one cell each,
// never split by a highlighter), and a line that is an include's rule.
const (
	mkOpen  = ''
	mkClose = ''
	mkRule  = ''
)

// skillDoc is one file as the engine sent it.
type skillDoc struct {
	file, name, desc, text, err string
	clipped                     bool
	files                       map[string]bool
	includes                    map[string]string
}

type skillLink struct {
	target string // relative to the skill's folder
	broken bool
	line   int // the rendered line it starts on
	// Its span in the plain lines, in cells: start (line, col), end.
	sl, sc, el, ec int
}

type skillMatch struct{ line, col, n, link int }

type skillPlace struct {
	file     string
	off, sel int
}

type skillPane struct {
	skill string
	at    skillPlace   // the file shown, its scroll and selected link
	hist  []skillPlace // back
	want  string       // the file asked for and not yet here
	docs  map[string]*skillDoc

	// The layout of at.file at width w.
	w        int
	header   []string
	lines    []string // rendered, the link markers in
	plain    []string // what shows, unstyled: search reads it
	links    []skillLink
	openAt   []int // per line: the link open at its start, or -1
	firstAt  []int // per line: the index of the first link that starts on it
	rows     int   // the body rows of the last view
	searchOn bool
	query    string
	matches  []skillMatch
	mi       int
}

// openSkill asks the engine for a skill and opens its pane below the list
// (or shows it there again).
func (m *tuiModel) openSkill(name string) {
	s := m.activeScreen()
	if s == nil {
		return
	}
	p := s.skillPane()
	if p == nil {
		p = &skillPane{}
		s.panes = append(s.panes, screenPane{body: p, weight: 3, min: 5, zoomUnder: narrowW})
	}
	*p = skillPane{skill: name, docs: map[string]*skillDoc{}, at: skillPlace{file: "SKILL.md", sel: -1}}
	s.focus = len(s.panes) - 1
	m.fetchSkillFile(p, "SKILL.md")
	m.syncPager()
	m.pg.showCur() // the list is shorter now: its selected row stays in view
}

// skillPane is the screen's skill pane, if it has one.
func (s *paneScreen) skillPane() *skillPane {
	for _, sp := range s.panes {
		if p, ok := sp.body.(*skillPane); ok {
			return p
		}
	}
	return nil
}

// closeSkill takes the pane away; the list has the focus again.
func (m *tuiModel) closeSkill() {
	s := m.activeScreen()
	if s == nil {
		return
	}
	s.panes = slices.DeleteFunc(s.panes, func(sp screenPane) bool { _, ok := sp.body.(*skillPane); return ok })
	s.focus = s.pagerPane()
	m.syncPager()
}

func (m *tuiModel) fetchSkillFile(p *skillPane, file string) {
	if d := p.docs[file]; d != nil {
		p.show(file)
		return
	}
	p.want = file
	b, _ := json.Marshal(map[string]string{"skill": p.skill, "file": file})
	if !m.proc.controlCmd("skillfile", string(b)) {
		p.want = ""
		m.flashText, m.flashUntil = "no engine right now — /reconnect first", time.Now().Add(flashFor)
	}
}

// noteSkillFile takes a skill_file event: the file the open pane asked for.
func (m tuiModel) noteSkillFile(ev evMsg) tuiModel {
	s := m.activeScreen()
	if s == nil {
		return m
	}
	p := s.skillPane()
	if p == nil || p.skill != str(ev["skill"]) || p.want == "" || p.want != str(ev["file"]) {
		return m
	}
	d := &skillDoc{file: p.want, name: str(ev["name"]), desc: str(ev["description"]), text: str(ev["text"]),
		err: str(ev["error"]), files: map[string]bool{}, includes: strMap(ev["includes"])}
	d.clipped, _ = ev["clipped"].(bool)
	if fs, ok := ev["files"].([]any); ok {
		for _, f := range fs {
			d.files[str(f)] = true
		}
	}
	p.docs[d.file], p.want = d, ""
	p.show(d.file)
	return m
}

// show makes file the one shown, laid out at the next view.
func (p *skillPane) show(file string) {
	if p.at.file != file {
		p.at = skillPlace{file: file, sel: -1}
	}
	p.w = 0
	p.searchOn, p.query, p.matches = false, "", nil
}

// ── layout ──────────────────────────────────────────────────────────

// layout renders the shown file at width w.
func (p *skillPane) layout(w int) {
	p.w = w
	p.header, p.lines, p.plain, p.links, p.openAt, p.firstAt = nil, nil, nil, nil, nil, nil
	d := p.docs[p.at.file]
	if d == nil {
		return
	}
	if d.err != "" {
		p.header = wrapToRows(stErr.Render(p.at.file+": "+d.err), w)
		return
	}
	if d.desc != "" {
		p.header = append(p.header, wrapToRows(d.desc, w)...)
	}
	var rules []string
	md := p.expand(d, d.file, d.text, 0, map[string]bool{d.file: true}, &rules)
	if d.clipped {
		md += "\n\n*(cut: the file is larger than the reader shows)*"
	}
	out := renderMarkdown(newMarkdown(w), md)
	for _, line := range strings.Split(out, "\n") {
		if i := strings.IndexRune(line, mkRule); i >= 0 {
			n, _ := strconv.Atoi(strings.TrimFunc(ansi.Strip(line[i+len(string(mkRule)):]), func(r rune) bool { return !unicode.IsDigit(r) }))
			if n < len(rules) {
				line = stDim.Render(fitRule(rules[n], w))
			}
		}
		p.lines = append(p.lines, line)
	}
	// The links' lines, and what is open where.
	k, open := 0, -1
	for i, line := range p.lines {
		p.openAt = append(p.openAt, open)
		p.firstAt = append(p.firstAt, k)
		for _, r := range line {
			switch r {
			case mkOpen:
				if k < len(p.links) {
					p.links[k].line = i
				}
				open = k
				k++
			case mkClose:
				open = -1
			}
		}
	}
	p.links = p.links[:min(k, len(p.links))]
	for i := range p.lines {
		p.plain = append(p.plain, ansi.Strip(p.decorate(i, -1, true)))
	}
	if p.at.sel >= len(p.links) {
		p.at.sel = -1
	}
}

// fitRule is an include's rule: its label between two lines, to w.
func fitRule(label string, w int) string {
	label, _ = fitPlain(label, max(1, w-4))
	return "┄┄ " + label + " " + strings.Repeat("┄", max(0, w-4-lipgloss.Width(label)))
}

// expand is a file's markdown for glamour: each link wrapped in markers (and
// recorded, in order), each include inlined between two rules, to
// SkillIncludeDepth deep and each file once.
func (p *skillPane) expand(d *skillDoc, file, text string, depth int, seen map[string]bool, rules *[]string) string {
	if !isMarkdownName(file) {
		fence := "```"
		for strings.Contains(text, fence) {
			fence += "`"
		}
		return fence + strings.TrimPrefix(path.Ext(file), ".") + "\n" + strings.TrimRight(text, "\n") + "\n" + fence
	}
	rule := func(label string) string {
		*rules = append(*rules, label)
		return "\n\n" + string(mkRule) + strconv.Itoa(len(*rules)-1) + "\n\n"
	}
	var b strings.Builder
	at := 0
	for _, r := range dun.SkillRefs(text) {
		b.WriteString(text[at:r.Start])
		at = r.End
		if r.Include {
			inc, ok := dun.SkillIncludePath(r.Target)
			body, have := d.includes[inc]
			switch {
			case !ok:
				b.WriteString(rule("not included: " + r.Target + " is outside the skill"))
			case seen[inc]:
				b.WriteString(rule("included above: " + inc))
			case !have:
				b.WriteString(rule("include not found: " + inc))
			case depth >= dun.SkillIncludeDepth:
				b.WriteString(rule(fmt.Sprintf("not included: %s (deeper than %d)", inc, dun.SkillIncludeDepth)))
			default:
				seen[inc] = true
				b.WriteString(rule("included: " + inc))
				b.WriteString(p.expand(d, inc, body, depth+1, seen, rules))
				b.WriteString(rule("end of " + inc))
			}
			continue
		}
		target, ok := dun.SkillLinkPath(file, r.Target, func(f string) bool { return d.files[f] })
		if !ok || r.Bare && !d.files[target] { // a word with a dot, not a file
			b.WriteString(text[r.Start:r.End])
			continue
		}
		p.links = append(p.links, skillLink{target: target, broken: !d.files[target]})
		b.WriteString(string(mkOpen) + r.Text + string(mkClose))
	}
	b.WriteString(text[at:])
	return b.String()
}

func isMarkdownName(p string) bool {
	switch strings.ToLower(path.Ext(p)) {
	case ".md", ".markdown", ".txt", "":
		return true
	}
	return false
}

// SGR for a link, on and off: underlined, the selected one in reverse
// video, a broken one struck through and marked ✗.
const (
	sgrLink, sgrLinkOff     = "\x1b[4m", "\x1b[24m"
	sgrSel, sgrSelOff       = "\x1b[7m", "\x1b[27m"
	sgrBroken, sgrBrokenOff = "\x1b[9m", "\x1b[29m"
)

func (p *skillPane) linkSGR(k, sel int) (string, string) {
	switch {
	case k == sel:
		return sgrSel, sgrSelOff
	case k >= 0 && k < len(p.links) && p.links[k].broken:
		return sgrBroken, sgrBrokenOff
	}
	return sgrLink, sgrLinkOff
}

// decorate is line i with its markers turned into the links' styles; spans
// records the links' cells (the first pass).
func (p *skillPane) decorate(i, sel int, spans bool) string {
	line := p.lines[i]
	var b strings.Builder
	open, k, col := p.openAt[i], p.firstAt[i], 0
	on := ""
	if open >= 0 {
		on, _ = p.linkSGR(open, sel)
		b.WriteString(on)
	}
	for j := 0; j < len(line); {
		if line[j] == 0x1b { // a sequence: glamour's resets end ours, so ours again after
			n := ansiSeqLen(line[j:])
			b.WriteString(line[j : j+n])
			j += n
			if on != "" {
				b.WriteString(on)
			}
			continue
		}
		r, size := utf8.DecodeRuneInString(line[j:])
		j += size
		switch r {
		case mkOpen:
			open = k
			k++
			on, _ = p.linkSGR(open, sel)
			b.WriteString(on)
			if spans && open < len(p.links) {
				p.links[open].sl, p.links[open].sc = i, col
			}
		case mkClose:
			_, off := p.linkSGR(open, sel)
			b.WriteString(off)
			if open >= 0 && open < len(p.links) && p.links[open].broken {
				b.WriteString(" ✗")
				col += 2
			}
			if spans && open >= 0 && open < len(p.links) {
				p.links[open].el, p.links[open].ec = i, col
			}
			open, on = -1, ""
		default:
			b.WriteRune(r)
			col += ansi.StringWidth(string(r))
		}
	}
	if on != "" {
		_, off := p.linkSGR(open, sel)
		b.WriteString(off)
	}
	return b.String()
}

// ansiSeqLen is the length of the escape sequence at the start of s.
func ansiSeqLen(s string) int {
	if len(s) < 2 {
		return len(s)
	}
	switch s[1] {
	case '[':
		for i := 2; i < len(s); i++ {
			if s[i] >= 0x40 && s[i] <= 0x7e {
				return i + 1
			}
		}
	case ']': // OSC, to BEL or ST
		for i := 2; i < len(s); i++ {
			if s[i] == 0x07 {
				return i + 1
			}
			if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\' {
				return i + 2
			}
		}
	default:
		return 2
	}
	return len(s)
}

// ── the pane ────────────────────────────────────────────────────────

// body is every row under the title: the header, then the rendered file.
func (p *skillPane) bodyLen() int { return len(p.header) + len(p.lines) }

func (p *skillPane) view(m *tuiModel, w, h int) string {
	if p.w != w {
		p.layout(w)
	}
	bodyH := max(1, h-1)
	if p.searchOn {
		bodyH = max(1, h-2)
	}
	p.rows = bodyH
	p.at.off = max(0, min(p.at.off, p.bodyLen()-bodyH))
	pos := "all"
	if p.bodyLen() > bodyH {
		pos = fmt.Sprintf("%d–%d of %d", p.at.off+1, min(p.at.off+bodyH, p.bodyLen()), p.bodyLen())
	}
	if n := len(p.links); n > 0 {
		if p.at.sel >= 0 {
			pos = fmt.Sprintf("link %d/%d · ", p.at.sel+1, n) + pos
		} else {
			pos = fmt.Sprintf("%d links · ", n) + pos
		}
	}
	name := p.skill
	if d := p.docs["SKILL.md"]; d != nil && d.name != "" {
		name = d.name
	}
	title := name
	if p.at.file != "SKILL.md" {
		title += " › " + p.at.file
	}
	title, _ = fitPlain(title, max(1, w-lipgloss.Width(pos)-2))
	rows := []string{stHeader.Render(title) + stDim.Render("  "+pos)}
	cur := skillMatch{line: -1}
	if p.mi >= 0 && p.mi < len(p.matches) {
		cur = p.matches[p.mi]
	}
	switch {
	case p.want != "" && p.docs[p.want] == nil:
		rows = append(rows, stDim.Render("reading "+p.want+" …"))
	default:
		for r := p.at.off; r < min(p.at.off+bodyH, p.bodyLen()); r++ {
			if r < len(p.header) {
				rows = append(rows, p.header[r])
				continue
			}
			i := r - len(p.header)
			line := p.decorate(i, p.at.sel, false)
			if cur.line == i && cur.col >= 0 && cur.n > 0 {
				line = ansi.Cut(line, 0, cur.col) + sgrSel + ansi.Strip(ansi.Cut(line, cur.col, cur.col+cur.n)) + sgrSelOff +
					ansi.Cut(line, cur.col+cur.n, w*4)
			}
			rows = append(rows, line)
		}
	}
	if p.searchOn {
		for len(rows) < h-1 {
			rows = append(rows, "")
		}
		count := "no match"
		if len(p.matches) > 0 {
			count = fmt.Sprintf("%d/%d", p.mi+1, len(p.matches))
		}
		if p.query == "" {
			count = ""
		}
		rows = append(rows, stHeader.Render("/")+p.query+"█"+stDim.Render("  "+count))
	}
	return lipgloss.NewStyle().Width(w).MaxWidth(w).Height(h).MaxHeight(h).Render(strings.Join(rows, "\n"))
}

func (p *skillPane) key(m *tuiModel, msg tea.KeyPressMsg) bool {
	k := msg.String()
	if p.w != m.w { // laid out before the keys read it (the screen is m.w wide)
		p.layout(m.w)
	}
	if p.searchOn {
		return p.searchKey(m, k, msg.Text)
	}
	page := max(1, p.rows-1)
	switch k {
	case "up", "k":
		p.at.off = max(0, p.at.off-1)
	case "down", "j":
		p.at.off++
	case "pgup":
		p.at.off = max(0, p.at.off-page)
	case "pgdown", "space":
		p.at.off += page
	case "home", "g":
		p.at.off = 0
	case "end", "G":
		p.at.off = p.bodyLen()
	case "n", "tab":
		p.stepLink(1)
	case "p", "shift+tab":
		p.stepLink(-1)
	case "enter":
		m.followLink(p)
	case "/":
		p.searchOn = true
		p.search()
	case "backspace", "esc":
		if len(p.hist) == 0 {
			if k == "esc" {
				m.closeSkill()
			}
			return true
		}
		back := p.hist[len(p.hist)-1]
		p.hist = p.hist[:len(p.hist)-1]
		m.fetchSkillFile(p, back.file)
		p.at = back
	case "q":
		return false
	}
	return true
}

// stepLink selects the next (d 1) or previous link: from none, the first
// at or after the top row (the last above the bottom, going back).
func (p *skillPane) stepLink(d int) {
	n := len(p.links)
	if n == 0 {
		return
	}
	sel := p.at.sel
	if sel < 0 {
		top := p.at.off - len(p.header)
		sel = n - 1
		if d > 0 {
			sel = 0
		}
		for k, l := range p.links {
			if d > 0 && l.line >= top {
				sel = k
				break
			}
			if d < 0 && l.line < top+p.rows {
				sel = k
			}
		}
	} else {
		sel = (sel + d + n) % n
	}
	p.at.sel = sel
	p.reveal(p.links[sel].line)
}

// reveal scrolls so rendered line i shows, a third of the way down when it
// was out of view.
func (p *skillPane) reveal(i int) {
	r := i + len(p.header)
	if r < p.at.off || r >= p.at.off+p.rows {
		p.at.off = max(0, r-p.rows/3)
	}
}

// followLink opens the selected link's file, the place left on the back
// stack.
func (m *tuiModel) followLink(p *skillPane) {
	if p.at.sel < 0 || p.at.sel >= len(p.links) {
		m.flashText, m.flashUntil = "no link selected — n/p or tab selects one", time.Now().Add(flashFor)
		return
	}
	l := p.links[p.at.sel]
	if l.broken {
		m.flashText, m.flashUntil = "no such file in the skill: "+l.target, time.Now().Add(flashFor)
		return
	}
	p.hist = append(p.hist, p.at)
	m.fetchSkillFile(p, l.target)
	if p.docs[l.target] == nil {
		p.at = skillPlace{file: l.target, sel: -1}
	}
}

func (p *skillPane) searchKey(m *tuiModel, k, text string) bool {
	switch k {
	case "esc":
		p.searchOn, p.query, p.matches = false, "", nil
	case "enter":
		p.searchOn = false
	case "backspace":
		if p.query == "" {
			p.searchOn = false
			return true
		}
		_, size := utf8.DecodeLastRuneInString(p.query)
		p.query = p.query[:len(p.query)-size]
		p.search()
	case "down", "ctrl+n":
		p.stepMatch(1)
	case "up", "ctrl+p":
		p.stepMatch(-1)
	default:
		if text == "" || utf8.RuneCountInString(text) != 1 || !unicode.IsPrint([]rune(text)[0]) {
			return true
		}
		p.query += text
		p.search()
	}
	return true
}

// search finds the query in the text and in the links' targets, and goes
// to the first match at or below the top row.
func (p *skillPane) search() {
	p.matches, p.mi = nil, 0
	q := strings.ToLower(p.query)
	if q == "" {
		return
	}
	inLink := map[int]bool{}
	for i, line := range p.plain {
		low := strings.ToLower(line)
		for from := 0; ; {
			j := strings.Index(low[from:], q)
			if j < 0 {
				break
			}
			col := ansi.StringWidth(line[:from+j])
			mt := skillMatch{line: i, col: col, n: ansi.StringWidth(line[from+j : from+j+len(q)]), link: p.linkAt(i, col)}
			if mt.link >= 0 {
				inLink[mt.link] = true
			}
			p.matches = append(p.matches, mt)
			from += j + len(q)
		}
	}
	for k, l := range p.links { // a link whose target matches but its text does not
		if !inLink[k] && strings.Contains(strings.ToLower(l.target), q) {
			p.matches = append(p.matches, skillMatch{line: l.line, col: -1, link: k})
		}
	}
	slices.SortStableFunc(p.matches, func(a, b skillMatch) int {
		if a.line != b.line {
			return a.line - b.line
		}
		ac, bc := a.col, b.col
		if ac < 0 {
			ac = p.links[a.link].sc
		}
		if bc < 0 {
			bc = p.links[b.link].sc
		}
		return ac - bc
	})
	if len(p.matches) == 0 {
		return
	}
	top := p.at.off - len(p.header)
	for i, mt := range p.matches {
		if mt.line >= top {
			p.mi = i
			break
		}
	}
	p.goMatch()
}

func (p *skillPane) stepMatch(d int) {
	if n := len(p.matches); n > 0 {
		p.mi = (p.mi + d + n) % n
		p.goMatch()
	}
}

// goMatch shows the current match; one in a link selects it, for enter.
func (p *skillPane) goMatch() {
	mt := p.matches[p.mi]
	if mt.link >= 0 {
		p.at.sel = mt.link
	}
	p.reveal(mt.line)
}

// linkAt is the link that covers (line, col), or -1.
func (p *skillPane) linkAt(line, col int) int {
	for k, l := range p.links {
		after := line > l.sl || line == l.sl && col >= l.sc
		before := line < l.el || line == l.el && col < l.ec
		if after && before {
			return k
		}
	}
	return -1
}

func (p *skillPane) hint(m *tuiModel) string {
	narrow := m.w < narrowW
	switch {
	case p.searchOn && narrow:
		return stDim.Render("type · ↓/↑ match · ⏎ keep · esc")
	case p.searchOn:
		return stDim.Render("type to search text and link targets · ↓/↑ (ctrl+n/p) next, previous match · enter keep · esc drop")
	case narrow:
		return stDim.Render("n/p link · ⏎ open · / find · esc back")
	}
	back := "esc close"
	if len(p.hist) > 0 {
		back = "backspace/esc back"
	}
	return stDim.Render("↑/↓ pgup/pgdn scroll · n/p (tab) next, previous link · enter open · / search · " + back)
}

// takesTab: tab is the next link here, not the next pane.
func (*skillPane) takesTab() bool { return true }
