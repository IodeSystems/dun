package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/cellbuf"
)

func (m *tuiModel) refresh() {
	start := time.Now()
	defer func() { frames.observe(stageRefresh, time.Since(start)) }()
	m.liveSplashNow()
	blocks := make([]string, 0, len(m.convo)+1)
	for _, e := range m.convo {
		blocks = append(blocks, e.view())
	}
	curAt := -1
	if replyShown(m.cur) {
		curAt = len(blocks)
		blocks = append(blocks, "") // rendered below, once the width is known
	}
	// Suggestions live inside the viewport so they don't steal height from the
	// conversation — the user can still see the last messages above them.
	if m.suggestActive() {
		// Predictions, not conversation: dimmed, the highlighted one a step
		// brighter (suggestStyle), so they never read as something said.
		for i, s := range m.suggestions {
			body := stSel.Render(fmt.Sprintf("%d", i+1)) + " " + suggestStyle(i == m.suggestSel).Render(s.text) + "  " +
				stDim.Render(fmt.Sprintf("%d%%", int(s.prob*100+0.5)))
			if i == m.suggestSel {
				blocks = append(blocks, addGutter(body, "▎ ", stSel))
			} else {
				blocks = append(blocks, addGutter(body, "  ", lipgloss.NewStyle()))
			}
		}
	}
	// In convo focus, gutter every block (selected one bright) so the highlight
	// aligns and the selected message shows a left border down its full height.
	selMode := m.focus == focusConvo && !m.asking && len(blocks) > 0
	// The gutter's column is reserved in every focus, so moving focus never
	// re-wraps: reserved only in convo focus, a row that filled the pane
	// broke in two when the conversation took the keys (USER 2026-10-24).
	width := m.vp.Width - 1
	wrapW := max(1, width)
	if curAt >= 0 {
		// The streaming reply renders the way it will once finished: its
		// wide blocks cut at the pane, not wrapped (wide.go).
		blocks[curAt], _ = renderAnswer(m.md, m.proseW(), m.cur, wrapW, 0)
	}
	rendered := make([]string, 0, len(m.vp.lines)) // display rows, not blocks
	frm := frame{
		// Sized by BLOCKS, not by convo: blocks is the convo plus the streaming
		// block (m.cur) plus any suggestion rows, and the loop below writes one
		// entry per block. Sizing by len(m.convo) panicked the moment anything
		// streamed. Convo-indexed readers are unaffected — the extra trailing
		// entries are simply never asked for.
		rowOffset: make([]int, len(blocks)),
		blockH:    make([]int, len(blocks)), // cache block heights for scroll math
		height:    m.convoHeightBudget(),
		width:     m.vp.Width,
		paneH:     m.vp.Height,
	}

	// Render all blocks. For user messages, re-render with full-width
	// background style (ignoring the pre-styled collapsed from e.view()).
	cumRow := 0 // running row offset for scrollOverlay mapping
	m.bandRaw = nil
	// Blocks wholly above the view fold now, and the view's top moves up by
	// the rows they gave back (scrollfold.go).
	foldOK, foldY, shift := m.foldAboveOK(), m.vp.YOffset, 0
	for i, b := range blocks {
		var w string
		folding := foldOK && i < len(m.convo) && m.aboveView(i, foldY, selMode)
		if folding {
			m.convo[i].foldEntry()
		}
		if i < len(m.convo) && m.convo[i].blank {
			// The turn-gap row: exactly one empty line, nothing to measure.
			w = ""
		} else if i < len(m.convo) && m.convo[i].boxed() {
			// A bounded box: its own cache, so a token re-wraps only the box
			// that grew (box.go).
			w = renderBox(&m.convo[i], wrapW, boxCapFor(&m.convo[i], wrapW, cumRow, m.convoHold, frm.height))
		} else if i < len(m.convo) && m.convo[i].docs == nil {
			e := &m.convo[i]
			// Rendered markdown bakes in the wrap width it was produced at, and a
			// pre-wrapped line has no break opportunity left for cellbuf to find
			// on a narrower pane — so re-render from the source when the width
			// moves. The key is wrapMaxOK, not wrapW: the default path below
			// leaves wrapW at 0 whenever the block already fits (the common case
			// after a re-render), so wrapW cannot say "measured at this width".
			// wrapMaxOK is set once per source — invalidateWrap() clears it here
			// after a re-render, and nothing else does between refreshes — so it
			// is true exactly when the current render matches the current width.
			// A token therefore never pays for a glamour re-render.
			if e.mdSource != "" && (e.wrapW == 0 || e.wrapW != width) {
				e.collapsed = m.renderEntry(e, wrapW)
				b = e.view()
				e.invalidateWrap()
			}
			switch {
			case e.userText != "":
				if e.wrapped == "" || e.wrapW != width || e.wrapState != e.state {
					e.wrapped, e.wrapW, e.wrapState = userRowAt(e.userText, wrapW), width, e.state
					if e.state == viewExpanded && e.stats != "" {
						for _, l := range strings.Split(e.stats, "\n") {
							e.wrapped += "\n" + hangWrap(l, wrapW)
						}
					}
				}
				w = e.wrapped
			default:
				if !e.wrapMaxOK || e.wrapState != e.state {
					// New or changed source: measure it once. Cheaper than wrapping
					// it, and it answers the question for every future width.
					e.wrapMax, e.wrapMaxOK, e.wrapState = maxLineWidth(b), true, e.state
					e.wrapped, e.wrapW = "", 0
				}
				// Record the width this measurement is valid at, even when the
				// block fits and no wrap happens: a block that fits at 96 also
				// fits at any wider width, so the fast path below stays correct —
				// but wrapW must say WHERE it was measured, or a consumer cannot
				// tell "fits here" from "never looked". Recorded after the cache
				// is checked: stamped first, it made every cached wrap look
				// current, and a notice wrapped at 48 columns stayed so after the
				// pane widened (aw4 i_1m604n7h2551hy4fzhjmcb).
				switch {
				case e.wrapMax <= wrapW:
					// Already fits — no wrapping at this width or any wider.
					w, e.wrapped = b, ""
				case e.wrapped != "" && e.wrapW == width:
					w = e.wrapped
				default:
					if e.hang {
						e.wrapped = hangWrap(b, wrapW)
					} else {
						e.wrapped = cellbuf.Wrap(b, wrapW, "")
					}
					w = e.wrapped
				}
				e.wrapW = width
			}
		} else {
			w = cellbuf.Wrap(b, wrapW, "")
		}
		// Never shorter than it has been (convoEntry.maxH). Docs blocks change
		// only when the person opens or closes something in them.
		switch {
		case i < len(m.convo) && m.convo[i].docs == nil && !m.convo[i].blank:
			e := &m.convo[i]
			w = holdHeight(w, &e.maxH, &e.maxKey, heightKey(wrapW, e.state))
		case i == len(m.convo) && replyShown(m.cur):
			w = holdHeight(w, &m.curMaxH, &m.curMaxKey, heightKey(wrapW, viewMinimized))
		}
		if selMode && i == m.sel {
			w = addGutter(w, "▎", stSel)
		} else {
			w = addGutter(w, " ", lipgloss.NewStyle())
		}
		if selMode && i == m.sel && band.on() {
			// The band is drawn over the finished rows, and they are kept as
			// they were so a pulse step can redraw just these (repaintBand).
			m.bandRaw, m.bandAt = strings.Split(w, "\n"), len(rendered)
			rendered = append(rendered, band.rows(m.bandRaw, m.vp.Width, m.pulse)...)
		} else {
			rendered = append(rendered, strings.Split(w, "\n")...)
		}
		h := len(rendered) - cumRow
		frm.blockH[i] = h
		frm.rowOffset[i] = cumRow
		if i < len(m.convo) {
			// The entry keeps its own copy for the trace/replay path and for
			// scrollOverlay's row mapping; the frame is the authoritative one
			// that View and applyScroll read.
			if folding {
				shift += m.convo[i].lastH - h
			}
			m.convo[i].rowOffset, m.convo[i].lastH = cumRow, h
		}
		cumRow += h
	}
	if shift != 0 && m.convoHold > 0 {
		m.convoHold -= shift // it counts rows from the top, the folded ones too
	}
	// A block that got shorter (closed, collapsed, a line taken away) does
	// not pull the view back: the top row stays and blank rows fill in
	// below, but never past the bottom of the view as it is now, so the
	// furthest scroll is where the view already is (holdShrink).
	if m.holdSkip {
		m.holdSkip = false
	} else {
		m.holdShrink(len(rendered), shift)
	}
	// A closed question's rows stay in the pane until new rows take them
	// (holdConvo): blank rows at the bottom, so nothing above them moves.
	if m.convoHold > 0 {
		if m.vp.Width != m.convoHoldW || len(rendered) >= m.convoHold {
			m.convoHold = 0
		} else {
			for len(rendered) < m.convoHold {
				rendered = append(rendered, "")
			}
		}
	}
	// Hand the pane the rows themselves. Joining them into one string only for
	// it to split them back was ~12ms of every refresh on a long conversation —
	// and every streamed token is a refresh.
	m.vp.SetLines(rendered)
	if shift != 0 {
		m.vp.SetYOffset(foldY - shift)
		m.foldedRows += shift
	}
	m.contentGen++
	// The frame is now the single geometry truth. refresh is the ONLY writer:
	// it populated frm above and stored it, so every reader (View, convoHeight,
	// applyScroll, scrollOverlay) sees the same snapshot instead of re-deriving
	// a copy that could drift.
	m.frame = frm

	m.applyScroll(selMode)
}

// applyScroll is where the viewport is allowed to move on its own: keep the
// selection in view, else stick to the bottom (live tail). It reads the block
// geometry refresh() just measured (rowOffset + blockH) rather than a rendered
// slice, so it can also be re-run when only the WINDOW changed — a resize, the
// task line appearing, the input growing a line. Height is as much an input to
// this decision as content is, and running it on content alone left the
// viewport a row short of the bottom whenever a frame's row budget moved.
func (m *tuiModel) applyScroll(selMode bool) {
	vh := m.vp.Height
	if vh <= 0 {
		return
	}
	// The block geometry below (selGeom, the last-user-row scan) reads the
	// frame — the same snapshot refresh just wrote. The frame is the single
	// value every reader agrees on, so a resize or a content change can never
	// leave this decision looking at a different geometry than View did.
	if selMode && m.sel >= 0 && m.sel < len(m.frame.blockH) {
		top, h := m.selGeom()
		if h >= vh {
			// Taller than the window: don't fight intra-message scroll — only snap
			// when the selection is entirely off-screen. ↑/↓ scroll within it.
			switch {
			case top >= m.vp.YOffset+vh: // fully below the fold
				m.vp.SetYOffset(top)
			case top+h <= m.vp.YOffset: // fully above the fold
				m.vp.SetYOffset(top + h - vh)
			}
			return
		}
		switch {
		case top < m.vp.YOffset:
			m.vp.SetYOffset(top)
		case top+h > m.vp.YOffset+vh:
			m.vp.SetYOffset(top + h - vh)
		}
		return
	}
	if !m.scrollPinned {
		return
	}
	// A panel below took rows from the pane: the view stays where it was
	// while the panel is open and nothing new has arrived (see the shrink in
	// Update). New rows, or the panel gone, end the hold.
	if m.panelHeld {
		switch {
		case !m.panelOpen():
			// Just closed — with a line added or not (the /mcp add
			// "adding …" line comes with the close): stay until the pane
			// has grown back (Update's funnel keeps this top row and then
			// ends the hold); after that, new rows move the view only as
			// far as they need.
			m.vp.SetYOffset(m.panelYOff)
			return
		case len(m.vp.lines) <= m.panelRows:
			m.vp.SetYOffset(m.panelYOff) // still open, nothing new: stay
			return
		}
		m.panelHeld = false
		// New rows with a panel still open: they are followed, and if the
		// panel grows in the same update (a form giving way to a taller
		// picker), the shrink keeps them in view (Update's funnel).
		m.panelFollow = true
	}
	// Follow the tail, and only the tail. This used to hold the last user
	// message on screen while a reply streamed, and that moved the view on its
	// own: a reader who scrolled down to the bottom was re-pinned and pulled
	// back up to the message, and between a reply's text and its tool calls
	// the view swung from the message to the bottom and back (USER
	// 2026-10-28: zero bounce). The message stays readable as the head bar
	// (scrollOverlay) instead.
	m.vp.GotoBottom()
}

// scrollToEnd jumps to the newest output and re-pins, so the view follows the
// stream again — the way ctrl+end ends a scroll-back everywhere else.
func (m *tuiModel) scrollToEnd() {
	m.vp.GotoBottom()
	m.scrollPinned, m.userJump = true, -1
	m.traceScroll()
}

// scrollToLastUser jumps to the top of the most recent user message: the start
// of the current exchange, which is what you want after reading back — "show me
// what I asked and everything since". Pressed again with the view still where
// it put it, it goes one user message further back, so repeated presses read
// what you asked in a session (USER 2026-10-29). Unpins, because it is a
// position the stream would otherwise scroll away from.
func (m *tuiModel) scrollToLastUser() {
	ro := m.frame.rowOffset
	before := len(m.convo)
	if m.userJump >= 0 && m.vp.YOffset == m.userJumpAt {
		before = m.userJump
	}
	at := -1
	for i := 0; i < before && i < len(ro); i++ {
		if m.convo[i].userText != "" {
			at = i
		}
	}
	if at < 0 {
		return // at the first one already, or none
	}
	m.vp.SetYOffset(ro[at])
	m.userJump, m.userJumpAt = at, m.vp.YOffset
	m.updateScrollPin()
}

// updateScrollPin unpins the scroll when the user scrolls away from the bottom,
// and re-pins when they scroll back. This prevents new messages from jumping
// the viewport while the user is reading older content.
func (m *tuiModel) updateScrollPin() {
	if m.vp.AtBottom() {
		m.scrollPinned = true
	} else {
		m.scrollPinned = false
	}
	m.traceScroll()
}

// traceScroll writes the current scroll state to the trace file if tracing is on.
func (m *tuiModel) traceScroll() {
	if m.traceFile == nil || m.vp.YOffset == m.tracePrevYOff {
		return
	}
	m.tracePrevYOff = m.vp.YOffset
	fmt.Fprintf(m.traceFile, "scroll yoff=%d pinned=%v\n", m.vp.YOffset, m.scrollPinned)
}

// traceEvent writes an engine event to the trace file if tracing is on.
func (m *tuiModel) traceEvent(ev evMsg) {
	if m.traceFile == nil {
		return
	}
	etype, _ := ev["type"].(string)
	data, _ := json.Marshal(ev)
	fmt.Fprintf(m.traceFile, "event %s %s\n", etype, data)
}

// traceResize writes a resize event to the trace file if tracing is on.
func (m *tuiModel) traceResize(msg tea.WindowSizeMsg) {
	if m.traceFile == nil {
		return
	}
	fmt.Fprintf(m.traceFile, "resize w=%d h=%d\n", msg.Width, msg.Height)
}
