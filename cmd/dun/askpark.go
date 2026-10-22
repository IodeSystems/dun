package main

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
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
	m.asking, m.askParked = false, true
	m.noting, m.customAnswer = false, false
	if i := m.askEntry(); i >= 0 {
		m.convo[i].renderAsk(true)
	}
	m.input.Reset()
	m.input.placeholder = "ask dun to do something…"
	m.input.Focus()
	m.focus = focusInput
	m.refresh()
	return m, m.flash("question left open — select it in the conversation (tab) and press enter to answer")
}

// reopenAsk brings the parked question's picker back as it was left.
func (m tuiModel) reopenAsk() (tea.Model, tea.Cmd) {
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
