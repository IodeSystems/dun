package main

import (
	"time"

	tea "charm.land/bubbletea/v2"
)

// A question can be put down and picked up again.
//
// esc in the answer picker used to quit dun. Now it PARKS the question: the
// picker closes, the composer comes back, and the question stays open — the
// engine is still waiting on it, so nothing is sent. The question's row in the
// conversation says it is unanswered; selecting it (tab, ↑/↓) and pressing
// enter reopens the picker where it was left.
//
// A message typed while a question is parked goes out as any message sent
// mid-turn does: the engine buffers it into the running turn (the ask is a
// tool call, so the turn is still running), and the question stays open.
//
// esc on its own no longer quits anywhere. Where it used to, it says how to:
// ctrl+c, or /exit.

const flashFor = 3 * time.Second

type flashMsg struct{}

// flash shows a short note in the status line for flashFor.
func (m *tuiModel) flash(s string) tea.Cmd {
	m.flashText, m.flashUntil = s, time.Now().Add(flashFor)
	return tea.Tick(flashFor, func(time.Time) tea.Msg { return flashMsg{} })
}

func (m tuiModel) flashing() bool {
	return m.flashText != "" && time.Now().Before(m.flashUntil)
}

// exitFlash is what esc says where it used to quit.
func (m *tuiModel) exitFlash() tea.Cmd {
	if m.disableExit {
		return m.flash("to exit: /exit")
	}
	return m.flash("to exit: ctrl+c or /exit")
}

// askEntry is the conversation row of the open question, or -1.
func (m *tuiModel) askEntry() int {
	for i := len(m.convo) - 1; i >= 0; i-- {
		if m.convo[i].askText != "" && m.convo[i].askOpen {
			return i
		}
	}
	return -1
}

// renderAsk draws a question row: plain while the picker is up or once it is
// answered, marked while it waits.
func (e *convoEntry) renderAsk(parked bool) {
	e.collapsed = stAsk.Render("❓ " + e.askText)
	if parked {
		e.collapsed += stDim.Render("  · unanswered — select it, enter to answer")
	}
	e.invalidateWrap()
}

// parkAsk closes the picker and leaves the question open.
func (m tuiModel) parkAsk() (tea.Model, tea.Cmd) {
	if i := m.askEntry(); i >= 0 {
		m.convo[i].renderAsk(true)
	}
	m.setFocus(focusInput)
	m.dropAsk()
	m.askParked = true
	m.refresh()
	return m, m.flash("question left open — select it in the conversation (tab) and press enter to answer")
}

// reopenAsk brings the parked question's picker back as it was left.
func (m tuiModel) reopenAsk() (tea.Model, tea.Cmd) {
	m.keepDraft()
	m.asking, m.askParked = true, false
	m.flashText = "" // the picker's own keys belong in the status line now
	if i := m.askEntry(); i >= 0 {
		m.convo[i].renderAsk(false)
	}
	m.input.Blur()
	if len(m.askOptions) == 0 {
		// A free-text question is only its text field.
		m.customAnswer = true
		m.input.Reset()
		m.input.placeholder = "type your answer…"
		m.input.Focus()
	}
	m.refresh()
	return m, nil
}

// settleAsk marks the question row answered (or gone) and drops the parked
// state.
func (m *tuiModel) settleAsk() {
	if i := m.askEntry(); i >= 0 {
		m.convo[i].askOpen = false
		m.convo[i].renderAsk(false)
	}
	m.askParked = false
}

// holdConvo keeps the conversation where it is when the question panel
// closes. Call it after the panel has gone (asking is false).
//
// The panel is several rows and the composer one, so the conversation pane
// grows by the difference. Pinned to the bottom, the pane then showed that many
// older rows at the top and every row moved down; the answer and the next reply
// arrived a moment later and moved them all back up. That was the bounce.
//
// Instead the top row stays put: the pane is given blank rows at the bottom up
// to the new height (refresh pads to convoHold), and the rows that arrive next
// fill them. The hold ends when the content reaches it, or at a new width.
func (m *tuiModel) holdConvo() {
	if h := m.convoHeightBudget(); m.vp.YOffset > 0 && h > m.vp.Height {
		m.holdTop(h)
	}
}

// holdTop grows the pane to h rows and keeps its top row: whatever closed
// below the conversation, the rows on screen stay put, and the new rows at
// the bottom are blank (convoHold) until content takes them. The padding
// reaches the bottom of the grown view and no further.
func (m *tuiModel) holdTop(h int) {
	yoff := m.vp.YOffset
	m.convoHold, m.convoHoldW = max(m.convoHold, yoff+h), m.vp.Width
	m.vp.setHeight(h)
	m.refresh()
	m.vp.SetYOffset(yoff)
}

// dropAsk closes the question panel, however the question ended: answered,
// parked, cut with the turn, or gone with the engine. The composer gets back
// the draft it had when the question opened.
func (m *tuiModel) dropAsk() {
	if !m.asking {
		return
	}
	m.asking, m.noting, m.customAnswer = false, false, false
	m.restoreDraft()
	m.input.placeholder = "ask dun to do something…"
	m.input.Focus()
	m.holdConvo()
}

// keepDraft sets the composer aside while a question uses it for a detail or
// a custom answer. The whole input is kept: text, cursor, paste tokens.
func (m *tuiModel) keepDraft() {
	if !m.askDraftSet {
		m.askDraft, m.askDraftSet = m.input, true
	}
}

// restoreDraft puts the kept draft back in the composer (empty if none). The
// size is the composer's current one: the terminal may have been resized.
func (m *tuiModel) restoreDraft() {
	if !m.askDraftSet {
		m.input.Reset()
		return
	}
	d := m.askDraft
	d.width, d.maxLines, d.lay = m.input.width, m.input.maxLines, m.input.lay
	m.input, m.askDraft, m.askDraftSet = d, multilineInput{}, false
	m.input.clampScroll()
}

// holdShrink is the one rule for a conversation that got shorter — a block
// closed or collapsed, a line taken away, a live row rewritten: the view keeps
// its top row. The pane is padded with blank rows up to the bottom of the view
// as it stands (YOffset + Height) and no further, so the furthest the person
// can scroll is where they already are and there is never more than a screen
// of blank. The rows that arrive next fill the padding (refresh drops the hold
// once the content reaches it); a new width, a reset or a new session drops
// it (dropHold).
//
// shift is the rows folded away above the view in this refresh
// (scrollfold.go): the old rows and the view are measured without them.
func (m *tuiModel) holdShrink(n, shift int) {
	prev := len(m.vp.lines) - shift
	if prev <= 0 || n >= prev || m.frame.width == 0 || m.frame.width != m.vp.Width {
		return
	}
	hold := min(prev, m.vp.YOffset-shift+m.vp.Height)
	if hold > n && hold > m.convoHold {
		m.convoHold, m.convoHoldW = hold, m.vp.Width
	}
}

// dropHold ends any hold: the conversation is about to be replaced (a reset,
// a restart, /repaint), so the next refresh starts it over at its own height.
func (m *tuiModel) dropHold() {
	m.convoHold, m.convoHoldW = 0, 0
	m.holdSkip = true
	m.panelHeld = false
}
