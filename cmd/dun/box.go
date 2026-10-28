package main

import (
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/cellbuf"
)

// Boxes: a round's thinking, and a run of tool calls (toolrun.go), are drawn
// in a box that grows with its content up to a cap and then scrolls INSIDE
// itself, so a long stream never moves the conversation by more than the
// box's own growth.
//
//	✻ thinking… 12s · 340 words               ↑ 23 more
//	│ the newest lines of the thought,
//	│ followed as they stream
//
// The cap (USER 2026-10-28): the free rows from the box's top to the bottom
// of the pane, which it fills without moving anything, then at most boxPush
// rows more, which push the conversation up; never more than the pane. The
// person can give it more: scrolling down (wheel, pgdown) while the view's
// last row is a box's hands that box the rows instead (grant).
//
// The header stays on top and says what is hidden. While live the box follows
// its tail; done, it keeps its height, scrolled to the end. A box on screen
// never gets shorter: it is drawn at the most rows it has reached (peak), and
// when its content gets shorter the rows given back show earlier content, or
// stay blank when there is none. Once the box is above the view, the peak is
// released (scrollfold.go).
// Selecting a box never scrolls it: in convo focus ↑/↓ step from block to
// block, boxes included. To scroll one you DESCEND into it (→ or l); inside,
// ↑/↓, pgup/pgdn, g/G and the wheel scroll its contents, the header says so,
// and ←, h or esc come back out, the position kept — a box left at its end
// follows its tail again. enter opens the whole thing in the pager.

type boxState struct {
	off    int  // first body row shown
	follow bool // pinned to the newest row: new content scrolls the box
	max    int  // the largest off, as of the last render
	shown  int  // body rows shown, as of the last render
	peak   int  // the most body rows it has shown at peakW: it never shows fewer
	peakW  int
	grant  int // rows the person gave it by scrolling down at its last row
	// capAt is its cap without the grant, measured once, when it first drew
	// at this width. Measured every frame, a box grew whenever something
	// above it folded: its top moved up, so it had more free rows below.
	capAt  int
	inside bool // descended into: the keys scroll it (box.go, insideBox)

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

// boxPush is the most rows a box may push the conversation up by, past the
// free rows it fills: 15, or half a short pane (a phone's).
func boxPush(paneH int) int { return min(15, max(2, paneH/2)) }

// boxCap is a box's height, header included, when its top is at row top of
// a pane of paneH rows whose content reaches at least bottom rows (the pane,
// or a hold's blank rows): the free rows below its top, plus boxPush, plus
// what it was granted; within [3, paneH].
func boxCap(top, bottom, paneH, grant int) int {
	free := max(0, max(bottom, paneH)-top)
	return max(3, min(paneH, free+boxPush(paneH)+grant))
}

// boxed says the entry is drawn as a box right now.
func (e convoEntry) boxed() bool { return e.box != nil && e.state == viewMinimized }

// wrapBody brings b.rows up to date with body at width w, each row behind
// edge. plain says the body is unstyled text, dimmed row by row — which is
// what lets it be wrapped a line at a time.
func (b *boxState) wrapBody(body string, w int, plain bool, edge string) {
	ww := max(1, w-lipgloss.Width(edge))
	if !plain {
		if b.src == body && b.w == w && b.rows != nil {
			return
		}
		b.src, b.w, b.rows = body, w, b.rows[:0]
		if strings.TrimSpace(stripANSI(body)) == "" {
			return
		}
		rows := strings.Split(cellbuf.Wrap(body, ww, ""), "\n")
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
			b.rows = append(b.rows, edge+stSecondary.Render(r))
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

// boxContent is the box's header and body rows at width w.
func (e *convoEntry) boxContent(w int) (string, []string) {
	switch {
	case e.run != nil:
		if it := e.run.single(); it != nil {
			// On a phone the result's preview takes rows of its own.
			if w <= narrowCall && it.done && !it.failed {
				if rows := callPreviewRows(it.preview, w); rows != nil {
					head := stTool.Render("✓ ") + callText(it.tool, it.args, 80)
					if e.run.interrupted {
						head += stDim.Render(" · ⏹ interrupted")
					}
					return head, rows
				}
			}
			return e.run.singleHead(it), singleRows(it, w)
		}
		return e.run.summary(), e.run.boxRows(w)
	case e.think != nil:
		e.box.wrapBody(strings.TrimSpace(e.think.text), w, true, stDim.Render("│ "))
		return stDim.Render(thinkHeader(e.think)), e.box.rows
	case e.msg != nil:
		edge := stDim.Render("│ ")
		e.box.wrapBody(e.msg.body(max(1, w-lipgloss.Width(edge))), w, false, edge)
		return e.msg.header(), e.box.rows
	}
	return e.collapsed, nil
}

// boxCapFor is e's box cap for a box whose top is at row top: measured the
// first time (capAt), then that plus the grant, within the pane.
func boxCapFor(e *convoEntry, w, top, bottom, paneH int) int {
	b := e.box
	if b.capAt == 0 || b.peakW != w {
		b.capAt = boxCap(top, bottom, paneH, 0)
	}
	return max(3, min(paneH, b.capAt+b.grant))
}

// renderBox draws the box at width w, at most capRows rows tall with its
// header (boxCapFor).
func renderBox(e *convoEntry, w, capRows int) string {
	b := e.box
	head, rows := e.boxContent(w)
	if b.peakW != w { // a resize re-wraps; that is the one time a box may shrink
		b.peak, b.peakW = 0, w // capAt was re-measured for w by boxCapFor
	}
	b.peak = max(b.peak, min(len(rows), capRows-1))
	b.shown = b.peak
	b.max = max(0, len(rows)-b.shown)
	if b.follow || b.off > b.max {
		b.off = b.max
	}
	b.off = max(0, b.off)

	var ind []string
	if b.off > 0 {
		ind = append(ind, "↑ "+strconv.Itoa(b.off)+" more")
	}
	if below := len(rows) - b.off - b.shown; below > 0 {
		ind = append(ind, "↓ "+strconv.Itoa(below)+" more")
	}
	if b.inside {
		ind = append(ind, "scrolling — ← to leave")
	}
	tail := ""
	if len(ind) > 0 {
		tail = "  " + stDim.Render(strings.Join(ind, " · "))
	}
	head = fitRow(head, w-lipgloss.Width(tail))
	out := make([]string, 0, 1+b.shown)
	out = append(out, head+tail)
	out = append(out, rows[b.off:min(len(rows), b.off+b.shown)]...)
	for len(out) < 1+b.shown { // held height: what the content gave back
		out = append(out, "")
	}
	if e.msg != nil { // a sent message's status, under the box (agentmsg.go)
		if f := e.msg.footer(); f != "" {
			out = append(out, fitRow(f, w))
		}
	}
	return strings.Join(out, "\n")
}

// grantBaseline gives n rows to the box whose rows end the view, when the
// view is already at the bottom: scrolling down there has nowhere to go, so
// it makes the box taller instead (up to the pane; boxCap). It reports
// whether it did.
func (m *tuiModel) grantBaseline(n int) bool {
	if !m.vp.AtBottom() {
		return false
	}
	last := m.vp.YOffset + m.vp.Height - 1
	for i := range m.convo {
		if i >= len(m.frame.rowOffset) || !m.convo[i].boxed() {
			continue
		}
		top := m.frame.rowOffset[i]
		if last < top || last >= top+m.frame.blockH[i] {
			continue
		}
		b := m.convo[i].box
		if b.shown+1 >= m.vp.Height || b.max == 0 {
			return false // as tall as the pane, or nothing hidden to show
		}
		b.grant += n
		m.refresh()
		return true
	}
	return false
}

// selBox is the selected block's box, if it is drawn as one, in convo focus.
func (m *tuiModel) selBox() *boxState {
	if m.focus != focusConvo || m.asking || m.sel < 0 || m.sel >= len(m.convo) || !m.convo[m.sel].boxed() {
		return nil
	}
	return m.convo[m.sel].box
}

// insideBox is the box the person has descended into, or nil.
func (m *tuiModel) insideBox() *boxState {
	if b := m.selBox(); b != nil && b.inside {
		return b
	}
	return nil
}

// leaveBox comes back out of b, keeping its position; at its end, it follows
// its tail again.
func leaveBox(b *boxState) {
	b.inside = false
	b.follow = b.off >= b.max
}

// scroll moves b's window by n rows, clamped.
func (b *boxState) scroll(n int) {
	b.off = min(max(b.off+n, 0), b.max)
	b.follow = b.off >= b.max
}

// insideKeys is the keyboard while descended into a box: it scrolls, and
// ←/h/esc leave. It reports whether the key was taken.
func (m *tuiModel) insideKeys(k string) bool {
	b := m.insideBox()
	if b == nil {
		return false
	}
	page := max(1, b.shown-1)
	switch k {
	case "up", "k":
		b.scroll(-1)
	case "down", "j":
		b.scroll(1)
	case "pgup":
		b.scroll(-page)
	case "pgdown":
		b.scroll(page)
	case "g", "home":
		b.scroll(-b.max - 1)
	case "G", "end":
		b.scroll(b.max + 1)
	case "left", "h", "esc":
		leaveBox(b)
	default:
		return false
	}
	return true
}
