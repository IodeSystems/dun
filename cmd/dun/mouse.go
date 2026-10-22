package main

import (
	"encoding/base64"
	"os"
	"os/exec"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// Mouse: the wheel scrolls (tui.go, pane.go, pager.go); the left button
// selects and copies.
//
//   - conversation: a click selects the block under it, the state ← and ↑/↓
//     reach; a click on the selected block is enter; a click in an open box's
//     body descends into it, as →. A press that moves before its release is a
//     drag instead: it highlights the cells from the press to the pointer and,
//     on release, copies their text (OSC 52, and tmux's buffer inside tmux).
//     Any key clears the highlight; esc only clears it.
//   - input: a click focuses it and puts the cursor under the pointer.
//   - activity: a click on the collapsed line opens the list; on a row,
//     selects it; on the selected row, opens it, as →.
//
// Only the left button. Motion with no button held, and every other button,
// is ignored.

// cellPos is a cell of the conversation's CONTENT (a pane line index, not a
// screen row), so a highlight stays on its text when the pane scrolls.
type cellPos struct{ line, col int }

func (a cellPos) before(b cellPos) bool {
	return a.line < b.line || a.line == b.line && a.col < b.col
}

// mouseSel is a left-button gesture that started in the conversation pane.
type mouseSel struct {
	on    bool    // pressed in the pane; a highlight is live while moved
	moved bool    // the pointer left the press cell: a drag, not a click
	held  bool    // released after a drag: the highlight stays until a key
	a, b  cellPos // press cell, pointer cell
}

// shown says a highlight is on screen.
func (s mouseSel) shown() bool { return s.on && s.moved }

// span is the selection in reading order, both ends inclusive.
func (s mouseSel) span() (from, to cellPos) {
	if s.b.before(s.a) {
		return s.b, s.a
	}
	return s.a, s.b
}

// blockAtRow is the block whose rows contain pane line row, or -1.
func blockAtRow(rowOffset, blockH []int, row int) int {
	for i := range rowOffset {
		if i < len(blockH) && row >= rowOffset[i] && row < rowOffset[i]+blockH[i] {
			return i
		}
	}
	return -1
}

// Screen geometry, top-down, as View lays it out: the head, the activity strip,
// the conversation pane, the divider, the lower pane.
func (m tuiModel) actTopRow() int { return lipgloss.Height(m.headView()) }

func (m tuiModel) convoTopRow() int {
	top := m.actTopRow()
	if a := m.activityView(); a != "" {
		top += lipgloss.Height(a)
	}
	return top
}

func (m tuiModel) lowerTopRow() int { return m.convoTopRow() + m.convoHeight() + 1 }

// inputShown says the lower pane is the plain composer (not a picker, a form,
// or the palette).
func (m tuiModel) inputShown() bool {
	return !m.asking && !m.picking && !m.modelPicking && !m.mcpForm && !m.paletteActive()
}

// cellAt maps a screen cell inside the pane to content, clamping to the pane.
func (m tuiModel) cellAt(x, y int) cellPos {
	h := m.convoHeight()
	r := min(max(y-m.convoTopRow(), 0), h-1)
	return cellPos{line: m.vp.YOffset + r, col: max(x, 0)}
}

// mouseLeft handles the left button. It owns no wheel event.
func (m tuiModel) mouseLeft(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if m.inspecting || m.searching || m.confirm.q != "" {
		return m, nil
	}
	switch {
	case msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft:
		m.drag = mouseSel{}
		top, h := m.convoTopRow(), m.convoHeight()
		switch {
		case msg.Y >= top && msg.Y < top+h:
			if m.asking || m.picking || m.modelPicking || m.mcpForm {
				return m, nil
			}
			c := m.cellAt(msg.X, msg.Y)
			m.drag = mouseSel{on: true, a: c, b: c}
		case msg.Y >= m.actTopRow() && msg.Y < top:
			return m.activityClick(msg.Y - m.actTopRow())
		case msg.Y >= m.lowerTopRow() && msg.Y < m.lowerTopRow()+lipgloss.Height(m.lowerView()):
			if !m.inputShown() {
				return m, nil
			}
			m.setFocus(focusInput)
			if m.input.Value() != "" {
				m.input.clickAt(msg.Y-m.lowerTopRow(), msg.X)
			}
			m.refresh()
			return m, textinput.Blink
		}
		return m, nil
	case msg.Action == tea.MouseActionMotion && msg.Button == tea.MouseButtonLeft && m.drag.on && !m.drag.held:
		// Past the pane's edge, the pane scrolls a row per motion event.
		top, h := m.convoTopRow(), m.convoHeight()
		if msg.Y < top {
			m.vp.SetYOffset(m.vp.YOffset - 1)
			m.updateScrollPin()
		} else if msg.Y >= top+h {
			m.vp.SetYOffset(m.vp.YOffset + 1)
			m.updateScrollPin()
		}
		c := m.cellAt(msg.X, msg.Y)
		m.drag.b = c
		if c != m.drag.a {
			m.drag.moved = true
		}
		return m, nil
	case msg.Action == tea.MouseActionRelease && m.drag.on && !m.drag.held:
		if !m.drag.moved {
			at := m.drag.a.line
			m.drag = mouseSel{}
			return m.convoClick(at)
		}
		m.drag.held = true
		from, to := m.drag.span()
		text := selText(m.vp.lines, from, to, m.gutterW())
		if text == "" {
			return m, nil
		}
		return m, tea.Batch(copyCmd(text), m.flash(commaf("copied %d chars", len([]rune(text)))))
	}
	return m, nil
}

// gutterW is the columns refresh puts left of every block in convo focus.
func (m tuiModel) gutterW() int {
	if m.focus == focusConvo && !m.asking && len(m.convo) > 0 {
		return 1
	}
	return 0
}

// convoClick is a click on pane line `line`.
func (m tuiModel) convoClick(line int) (tea.Model, tea.Cmd) {
	i := blockAtRow(m.frame.rowOffset, m.frame.blockH, line)
	if i < 0 || i >= len(m.convo) || m.selSkippable(i) {
		return m, nil
	}
	e := &m.convo[i]
	body := e.boxed() && line > m.frame.rowOffset[i] // below a box's header row
	if m.focus == focusConvo && m.sel == i {
		if body {
			if !e.box.inside {
				e.box.inside = true
				m.refresh()
			}
			return m, nil
		}
		return m.update(tea.KeyMsg{Type: tea.KeyEnter})
	}
	if m.sel >= 0 && m.sel < len(m.convo) && m.convo[m.sel].box != nil && m.convo[m.sel].box.inside {
		leaveBox(m.convo[m.sel].box)
	}
	if m.focus != focusConvo {
		m.setFocus(focusConvo)
	}
	m.sel = i
	if body {
		e.box.inside = true
	}
	m.refresh()
	return m, nil
}

// activityClick is a click on row r of the activity strip.
func (m tuiModel) activityClick(r int) (tea.Model, tea.Cmd) {
	rows := m.actRows()
	if len(rows) == 0 || m.asking {
		return m, nil
	}
	if m.actLevel == actCollapsed {
		m.setFocus(focusActivity)
		m.actLevel = actList
		m.refresh()
		return m, nil
	}
	if r == 0 { // the "▾ counts" head
		m.setFocus(focusActivity)
		m.refresh()
		return m, nil
	}
	idx := actWindow(m.actIndex(rows), len(rows)) + r - 1
	if idx >= len(rows) {
		return m, nil
	}
	if m.focus == focusActivity && rows[idx].key == m.actSel {
		cmd := m.actDescend()
		m.refresh()
		return m, cmd
	}
	m.setFocus(focusActivity)
	m.actSel = rows[idx].key
	m.refresh()
	return m, nil
}

// selText is the plain text of cells from..to (inclusive) of lines, skip
// columns of gutter left off: ANSI stripped, trailing spaces trimmed per line,
// one line per pane row. Wrapped rows of one logical line are NOT joined: the
// pane keeps no record of which row breaks were soft.
func selText(lines []string, from, to cellPos, skip int) string {
	var out []string
	for l := from.line; l <= to.line && l < len(lines); l++ {
		if l < 0 {
			continue
		}
		plain := ansi.Strip(lines[l])
		lo, hi := skip, ansi.StringWidth(plain)
		if l == from.line {
			lo = max(lo, from.col)
		}
		if l == to.line {
			hi = min(hi, to.col+1)
		}
		s := ""
		if hi > lo {
			s = ansi.Cut(plain, lo, hi)
		}
		out = append(out, strings.TrimRight(s, " \t"))
	}
	return strings.TrimRight(strings.Join(out, "\n"), "\n")
}

// highlight draws the selection over the pane's visible rows (first = the pane
// line of rows[0]) in reverse video. Row count and widths are unchanged.
func highlight(rows []string, first int, from, to cellPos) []string {
	out := make([]string, len(rows))
	for i, row := range rows {
		out[i] = row
		l := first + i
		if l < from.line || l > to.line {
			continue
		}
		w := ansi.StringWidth(row)
		lo, hi := 0, w
		if l == from.line {
			lo = from.col
		}
		if l == to.line {
			hi = min(w, to.col+1)
		}
		if hi <= lo {
			continue
		}
		out[i] = ansi.Cut(row, 0, lo) + "\x1b[0;7m" + ansi.Strip(ansi.Cut(row, lo, hi)) + "\x1b[0m" + ansi.Cut(row, hi, w)
	}
	return out
}

// selOverlay is the pane's rendered rows with the highlight on, when there is
// one.
func (m tuiModel) selOverlay(pane string, yoff int) string {
	if !m.drag.shown() {
		return pane
	}
	from, to := m.drag.span()
	return strings.Join(highlight(strings.Split(pane, "\n"), yoff, from, to), "\n")
}

// osc52 is the terminal's set-clipboard sequence for text.
func osc52(text string) string {
	return "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(text)) + "\x07"
}

// Injected by tests: the terminal write, the environment, and the tmux call.
var (
	termWrite = func(s string) { _, _ = os.Stdout.WriteString(s) }
	clipEnv   = os.Getenv
	tmuxLoad  = func(text string) error {
		c := exec.Command("tmux", "load-buffer", "-w", "-")
		c.Stdin = strings.NewReader(text)
		return c.Run()
	}
)

// copyText puts text on the clipboard: OSC 52 to the terminal, and inside tmux
// also tmux's paste buffer, which -w hands on to the clipboard whatever
// set-clipboard says.
func copyText(text string) {
	termWrite(osc52(text))
	if clipEnv("TMUX") != "" {
		_ = tmuxLoad(text)
	}
}

func copyCmd(text string) tea.Cmd {
	return func() tea.Msg {
		copyText(text)
		return nil
	}
}
