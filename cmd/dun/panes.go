package main

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/iodesystems/dun"
)

// A pushed screen (docs/panes.md): panes stacked between the head row and
// the status row, in place of the conversation, the input and the activity
// strip, which go away while it is up (owner, 2026-10-25). Tab moves the
// focus between its panes; a divider between two panes leans toward the
// focused one; the status row is the focused pane's keys; esc in a pane that
// does not take it pops the screen.
//
// /mcp is the first: the server table on top, the selected server's
// activity log below it (USER 2026-10-25). The base screen (activity,
// conversation, input) is not on panes yet.

// paneBody is what a pane shows and the keys it takes.
type paneBody interface {
	// view is exactly h rows of w columns.
	view(m *tuiModel, w, h int) string
	// key takes a key; false: not its key (esc then pops the screen).
	key(m *tuiModel, msg tea.KeyPressMsg) bool
	hint(m *tuiModel) string
}

type screenPane struct {
	body   paneBody
	weight int // its share of the height
	min    int // rows it keeps however small the terminal
}

type paneScreen struct {
	panes []screenPane
	focus int
	// valid reports whether the screen still stands: /mcp's stands while
	// the pager shows its table.
	valid func(m *tuiModel) bool
}

// activeScreen is the screen up, or nil.
func (m *tuiModel) activeScreen() *paneScreen {
	if m.scr != nil && m.scr.valid(m) {
		return m.scr
	}
	return nil
}

// screenRows is the rows a screen has: the terminal less the head row, the
// upgrade bar and the status row.
func (m tuiModel) screenRows() int {
	h := m.h - 1 - lipgloss.Height(m.headView())
	if u := m.upgradeView(); u != "" {
		h -= lipgloss.Height(u)
	}
	return max(1, h)
}

// heights splits rows among the panes, a divider between each two: each
// gets its min, then what is left goes by weight, the remainder to the
// first.
func (s *paneScreen) heights(rows int) []int {
	n := len(s.panes)
	hs := make([]int, n)
	free := rows - (n - 1)
	weights := 0
	for i, p := range s.panes {
		hs[i] = p.min
		free -= p.min
		weights += p.weight
	}
	if free <= 0 || weights == 0 {
		return hs
	}
	given := 0
	for i, p := range s.panes {
		add := free * p.weight / weights
		hs[i] += add
		given += add
	}
	hs[0] += free - given
	return hs
}

// paneHeight is pane i's rows on the screen now.
func (m tuiModel) paneHeight(s *paneScreen, i int) int {
	return s.heights(m.screenRows())[i]
}

func (s *paneScreen) view(m *tuiModel, w, rows int) string {
	hs := s.heights(rows)
	out := make([]string, 0, 2*len(s.panes))
	for i, p := range s.panes {
		if i > 0 {
			out = append(out, paneDivider(w, i-1, s.focus))
		}
		out = append(out, p.body.view(m, w, hs[i]))
	}
	return strings.Join(out, "\n")
}

// paneDivider is the divider below pane i: it leans toward the focused pane
// when that is one of the two it divides, and is dim between two others.
func paneDivider(w, i, focus int) string {
	switch focus {
	case i:
		return divider(w, true)
	case i + 1:
		return divider(w, false)
	}
	return stDim.Render(strings.Repeat("▁", max(2, w)))
}

// key routes a key on the screen: tab and shift+tab move the focus, the
// rest go to the focused pane; esc it does not take pops the screen. True:
// the screen had the key.
func (s *paneScreen) key(m *tuiModel, msg tea.KeyPressMsg) bool {
	switch msg.String() {
	case "tab":
		s.focus = (s.focus + 1) % len(s.panes)
		return true
	case "shift+tab":
		s.focus = (s.focus + len(s.panes) - 1) % len(s.panes)
		return true
	}
	if s.panes[s.focus].body.key(m, msg) {
		return true
	}
	if msg.String() == "esc" || msg.String() == "q" {
		m.popScreen()
	}
	return true
}

func (s *paneScreen) hint(m *tuiModel) string {
	h := s.panes[s.focus].body.hint(m)
	if len(s.panes) > 1 {
		h += stDim.Render(" · tab next pane")
	}
	return h
}

// popScreen takes the screen down, and the pager it stood on.
func (m *tuiModel) popScreen() {
	m.scr = nil
	m.paging = false
	m.refresh()
}

// ── /mcp ───────────────────────────────────────────────────────────

// mcpScreen is the server table over the selected server's log.
func mcpScreen() *paneScreen {
	return &paneScreen{
		panes: []screenPane{{body: tablePane{}, weight: 3, min: 4}, {body: &serverLogPane{follow: true}, weight: 2, min: 3}},
		valid: func(m *tuiModel) bool { return m.serversPaging() != nil },
	}
}

// tablePane is the pager showing /mcp's table; its keys are the pager's
// and the table's (mcpview.go).
type tablePane struct{}

func (tablePane) view(m *tuiModel, w, h int) string { return m.pg.view(w, h) }

func (tablePane) key(m *tuiModel, msg tea.KeyPressMsg) bool {
	if v := m.serversPaging(); v != nil && m.serverKey(v, msg.String()) {
		return true
	}
	return m.pg.key(msg.String())
}

func (tablePane) hint(m *tuiModel) string {
	if v := m.serversPaging(); v != nil {
		return v.hint()
	}
	return stDim.Render(m.pg.hint())
}

// serverLogPane is the activity log of the server selected in the table,
// newest at the bottom. It follows the newest line until scrolled up, and
// again once scrolled back to the end.
type serverLogPane struct {
	off     int // the first row shown, when not following
	follow  bool
	rows    int // the body rows of the last view, for the scroll keys
	lastEnd int // the last view's end offset: scrolling to it follows again
}

// selected is the table's selected server.
func (p *serverLogPane) selected(m *tuiModel) (dun.ServerState, bool) {
	v := m.serversPaging()
	if v == nil || m.pg.cur < 0 || m.pg.cur >= len(v.rows) {
		return dun.ServerState{}, false
	}
	return v.rows[m.pg.cur], true
}

// lines is the selected server's log, wrapped to w.
func (p *serverLogPane) lines(m *tuiModel, w int) []string {
	st, ok := p.selected(m)
	if !ok {
		return nil
	}
	var out []string
	for _, l := range st.Log {
		text := l.Text
		if l.Stderr {
			text = "stderr: " + text
		}
		row := stDim.Render(l.At.Format("15:04:05")) + " " + text
		out = append(out, wrapToRows(row, max(1, w))...)
	}
	return out
}

func (p *serverLogPane) view(m *tuiModel, w, h int) string {
	st, _ := p.selected(m)
	lines := p.lines(m, w)
	bodyH := max(1, h-1)
	p.rows = bodyH
	end := max(0, len(lines)-bodyH)
	p.lastEnd = end
	if p.follow || p.off > end {
		p.off = end
	}
	pos := "all"
	if len(lines) > bodyH {
		pos = fmt.Sprintf("%d–%d of %d", p.off+1, min(p.off+bodyH, len(lines)), len(lines))
	}
	title, _ := fitPlain("activity · "+st.ID, max(1, w-len(pos)-2))
	rows := []string{stHeader.Render(title) + stDim.Render("  "+pos)}
	if len(lines) == 0 {
		rows = append(rows, stDim.Render("nothing yet: a start, its stderr and its stop are written here"))
	}
	rows = append(rows, lines[p.off:min(p.off+bodyH, len(lines))]...)
	return lipgloss.NewStyle().Width(w).MaxWidth(w).Height(h).MaxHeight(h).Render(strings.Join(rows, "\n"))
}

func (p *serverLogPane) key(m *tuiModel, msg tea.KeyPressMsg) bool {
	page := max(1, p.rows-1)
	switch msg.String() {
	case "up", "k":
		p.scroll(-1)
	case "down", "j":
		p.scroll(1)
	case "pgup":
		p.scroll(-page)
	case "pgdown", "space":
		p.scroll(page)
	case "home", "g":
		p.follow, p.off = false, 0
	case "end", "G":
		p.follow = true
	default:
		return false
	}
	return true
}

// scroll moves by d rows; reaching the end follows again. The view clamps.
func (p *serverLogPane) scroll(d int) {
	if p.follow && d < 0 {
		p.follow = false // off is the end, from the last view
	}
	p.off = max(0, p.off+d)
	if d > 0 && p.off >= p.lastEnd {
		p.follow = true
	}
}

func (p *serverLogPane) hint(*tuiModel) string {
	return stDim.Render("↑/↓ pgup/pgdn scroll · g/G start, end (end follows) · esc back")
}
