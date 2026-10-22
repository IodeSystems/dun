package main

import (
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/cellbuf"
)

// Boxes: a round's thinking and a tool call's output are drawn in a box that
// grows with its content up to a cap and then scrolls INSIDE itself, so a
// long stream never moves the conversation by more than the box's own growth.
//
//	✻ thinking… 12s · 340 words               ↑ 23 more
//	│ the newest lines of the thought,
//	│ followed as they stream
//
// The header stays on top and says what is hidden. While live the box follows
// its tail; done, it keeps its height, scrolled to the end — there is no fold
// and no shrink, so the end of a round moves nothing. Selected in convo
// focus, ↑/↓ and pgup/pgdn scroll inside it; ↑ at its top and ↓ at its bottom
// move the selection on, as for any block. enter opens the whole thing in the
// pager.

type boxState struct {
	off    int  // first body row shown
	follow bool // pinned to the newest row: new content scrolls the box
	max    int  // the largest off, as of the last render
	shown  int  // body rows shown, as of the last render

	// The wrapped body, cached by source and width. A box that only grew —
	// a thought streaming in — keeps the rows of its finished lines (stable
	// bytes of src, stableRows rows) and wraps just what came after them, so
	// a frame costs the new text, not the whole thought.
	src        string
	w          int
	rows       []string
	stable     int
	stableRows int
}

// boxCap is a box's height, header included, on a pane of h rows: ten rows,
// or a third of a short pane, never under three.
func boxCap(h int) int { return max(3, min(10, h/3)) }

// boxed says the entry is drawn as a box right now: a thinking block, or a
// tool call with its result, in its default state (→ still opens a tool
// call's full output inline).
func (e convoEntry) boxed() bool { return e.box != nil && e.state == viewMinimized }

// boxSource is the box's header and body. plain says the body is unstyled
// text, dimmed row by row — which is what lets it be wrapped a line at a time.
func (e convoEntry) boxSource() (head, body string, plain bool) {
	switch {
	case e.think != nil:
		return stDim.Render(thinkHeader(e.think)), strings.TrimSpace(e.think.text), true
	case e.tool != nil:
		return e.tool.head, strings.TrimRight(e.tool.output, "\n"), false
	}
	return e.collapsed, "", false
}

// wrapBody brings b.rows up to date with body at width w.
func (b *boxState) wrapBody(body string, w int, plain bool) {
	ww := max(1, w-2)
	edge := stDim.Render("│ ")
	if !plain {
		if b.src == body && b.w == w && b.rows != nil {
			return
		}
		b.src, b.w, b.rows = body, w, b.rows[:0]
		if strings.TrimSpace(stripANSI(body)) == "" {
			return
		}
		rows := strings.Split(packSGR(cellbuf.Wrap(body, ww, "")), "\n")
		// A styled body can end in rows that are only padding and escapes.
		for len(rows) > 0 && strings.TrimSpace(stripANSI(rows[len(rows)-1])) == "" {
			rows = rows[:len(rows)-1]
		}
		for _, r := range rows {
			b.rows = append(b.rows, edge+r)
		}
		return
	}
	if b.w != w || b.rows == nil || b.stable > len(body) || body[:b.stable] != b.src[:b.stable] {
		b.stable, b.stableRows, b.rows = 0, 0, b.rows[:0]
	}
	b.src, b.w = body, w
	b.rows = b.rows[:b.stableRows]
	lines := strings.Split(body[b.stable:], "\n")
	for k, ln := range lines {
		for _, r := range strings.Split(cellbuf.Wrap(ln, ww, ""), "\n") {
			b.rows = append(b.rows, edge+stDim.Render(r))
		}
		if k < len(lines)-1 { // a finished line: its rows will not change
			b.stable += len(ln) + 1
			b.stableRows = len(b.rows)
		}
	}
	if body == "" {
		b.rows = b.rows[:0]
	}
}

// renderBox draws the box at width w on a pane of paneH rows.
func renderBox(e *convoEntry, w, paneH int) string {
	b := e.box
	head, body, plain := e.boxSource()
	b.wrapBody(body, w, plain)
	b.shown = min(len(b.rows), boxCap(paneH)-1)
	b.max = len(b.rows) - b.shown
	if b.follow || b.off > b.max {
		b.off = b.max
	}
	b.off = max(0, b.off)

	var ind []string
	if b.off > 0 {
		ind = append(ind, "↑ "+strconv.Itoa(b.off)+" more")
	}
	if below := len(b.rows) - b.off - b.shown; below > 0 {
		ind = append(ind, "↓ "+strconv.Itoa(below)+" more")
	}
	tail := ""
	if len(ind) > 0 {
		tail = "  " + stDim.Render(strings.Join(ind, " · "))
	}
	head = lipgloss.NewStyle().MaxWidth(max(1, w-lipgloss.Width(tail))).Render(head)
	out := make([]string, 0, 1+b.shown)
	out = append(out, head+tail)
	out = append(out, b.rows[b.off:b.off+b.shown]...)
	return strings.Join(out, "\n")
}

// scrollBox moves the selected box's window by n rows and reports whether it
// moved — false at the edge, where the key belongs to the selection.
func (m *tuiModel) scrollBox(n int) bool {
	if m.sel < 0 || m.sel >= len(m.convo) || !m.convo[m.sel].boxed() {
		return false
	}
	b := m.convo[m.sel].box
	off := min(max(b.off+n, 0), b.max)
	if off == b.off {
		return false
	}
	b.off = off
	b.follow = off >= b.max
	return true
}
