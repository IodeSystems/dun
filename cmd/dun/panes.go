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
	// fixed: its height is its content's (statusLines), up to half the
	// screen, before the others share the rest. A fixed pane takes no focus.
	fixed bool
}

// statusBody is a fixed pane's content: its lines at width w.
type statusBody interface {
	paneBody
	lines(m *tuiModel, w int) []string
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

// heights splits rows among the panes, a divider between each two: a fixed
// pane gets its content's rows (half the screen at most), the others their
// min, then what is left goes by weight, the remainder to the first that
// has a weight.
func (s *paneScreen) heights(m *tuiModel, w, rows int) []int {
	n := len(s.panes)
	hs := make([]int, n)
	free := rows - (n - 1)
	weights, first := 0, -1
	for i, p := range s.panes {
		if p.fixed {
			if sb, ok := p.body.(statusBody); ok {
				hs[i] = max(1, min(len(sb.lines(m, w)), rows/2))
			}
		} else {
			hs[i] = p.min
			weights += p.weight
			if first < 0 {
				first = i
			}
		}
		free -= hs[i]
	}
	if free <= 0 || weights == 0 {
		return hs
	}
	given := 0
	for i, p := range s.panes {
		if p.fixed {
			continue
		}
		add := free * p.weight / weights
		hs[i] += add
		given += add
	}
	hs[first] += free - given
	return hs
}

// paneHeight is pane i's rows on the screen now.
func (m tuiModel) paneHeight(s *paneScreen, i int) int {
	return s.heights(&m, m.w, m.screenRows())[i]
}

// pagerPane is the pane the pager draws in: the first one that is not
// fixed.
func (s *paneScreen) pagerPane() int {
	for i, p := range s.panes {
		if !p.fixed {
			return i
		}
	}
	return 0
}

func (s *paneScreen) view(m *tuiModel, w, rows int) string {
	hs := s.heights(m, w, rows)
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

// focusables is the panes tab visits, in order.
func (s *paneScreen) focusables() []int {
	var out []int
	for i, p := range s.panes {
		if !p.fixed {
			out = append(out, i)
		}
	}
	return out
}

// moveFocus steps the focus d places through the focusable panes.
func (s *paneScreen) moveFocus(d int) {
	f := s.focusables()
	if len(f) == 0 {
		return
	}
	at := 0
	for i, p := range f {
		if p == s.focus {
			at = i
		}
	}
	s.focus = f[(at+d+len(f))%len(f)]
}

// key routes a key on the screen: tab and shift+tab move the focus, the
// rest go to the focused pane; esc it does not take pops the screen. True:
// the screen had the key.
func (s *paneScreen) key(m *tuiModel, msg tea.KeyPressMsg) bool {
	// With one pane to focus, tab is that pane's (a switch list cycles its
	// mode with it).
	switch msg.String() {
	case "tab":
		if len(s.focusables()) > 1 {
			s.moveFocus(1)
			return true
		}
	case "shift+tab":
		if len(s.focusables()) > 1 {
			s.moveFocus(-1)
			return true
		}
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
	if len(s.focusables()) > 1 {
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
		panes: []screenPane{{body: mcpStatusPane{}, fixed: true}, {body: tablePane{}, weight: 3, min: 4},
			{body: &serverLogPane{follow: true}, weight: 2, min: 3}},
		focus: 1,
		valid: func(m *tuiModel) bool { return m.serversPaging() != nil },
	}
}

// mcpStatusPane is /mcp's status: the servers, how many are up, what they
// cost.
type mcpStatusPane struct{}

func (mcpStatusPane) lines(m *tuiModel, w int) []string {
	if v := m.serversPaging(); v != nil {
		return v.lines(m, w)
	}
	return nil
}

func (p mcpStatusPane) view(m *tuiModel, w, h int) string {
	rows := p.lines(m, w)
	if len(rows) > h {
		rows = rows[:h]
	}
	return lipgloss.NewStyle().Width(w).MaxWidth(w).Height(h).MaxHeight(h).Render(strings.Join(rows, "\n"))
}

func (mcpStatusPane) key(*tuiModel, tea.KeyPressMsg) bool { return false }
func (mcpStatusPane) hint(*tuiModel) string               { return "" }

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

// ── a switch list (/rules, /skill) ──────────────────────────────────

// switchScreen is a kind's status pane over its list.
func switchScreen() *paneScreen {
	return &paneScreen{
		panes: []screenPane{{body: switchStatusPane{}, fixed: true}, {body: switchListPane{}, weight: 1, min: 3}},
		focus: 1,
		valid: func(m *tuiModel) bool { return m.switchesPaging() != nil },
	}
}

// switchStatusPane is the kind's status: what it is, its mode, the cost of
// what is on, its facts and its warning.
type switchStatusPane struct{}

func (switchStatusPane) lines(m *tuiModel, w int) []string {
	if v := m.switchesPaging(); v != nil {
		return v.lines(m, w)
	}
	return nil
}

func (p switchStatusPane) view(m *tuiModel, w, h int) string {
	rows := p.lines(m, w)
	if len(rows) > h {
		rows = rows[:h]
	}
	return lipgloss.NewStyle().Width(w).MaxWidth(w).Height(h).MaxHeight(h).Render(strings.Join(rows, "\n"))
}

func (switchStatusPane) key(*tuiModel, tea.KeyPressMsg) bool { return false }
func (switchStatusPane) hint(*tuiModel) string               { return "" }

// switchListPane is the pager showing the list; its keys are the list's
// (switchlist.go) and the pager's.
type switchListPane struct{}

func (switchListPane) view(m *tuiModel, w, h int) string { return m.pg.view(w, h) }

func (switchListPane) key(m *tuiModel, msg tea.KeyPressMsg) bool {
	if v := m.switchesPaging(); v != nil && m.switchKey(v, msg.String(), msg.Text) {
		return true
	}
	return m.pg.key(msg.String())
}

func (switchListPane) hint(m *tuiModel) string {
	if v := m.switchesPaging(); v != nil {
		return stDim.Render(v.hint())
	}
	return ""
}
