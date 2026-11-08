package tui

import "time"

// presence.go — the person is here. The engine delays a "settle" (the agent's
// report after a turn that used tools) until the person has been away for a
// while (dun/settle.go). It cannot see the keyboard, so the TUI says so: a
// keystroke, a paste or a scroll of the wheel (which also covers reading in a
// pane) sends control "presence", at most every presencePing. Same signal as
// the idle clock for suggestions (armIdleSuggest), minus its conditions: a
// person scrolling back through a long reply is present with a non-empty
// input too. Not during a turn, and not before one has ended in this TUI (as for
// suggestions): no settle is pending then, and a turn's end starts the delay.
const presencePing = 10 * time.Second

func (m *tuiModel) notePresence() {
	if m.busy || !m.liveTurnDone || m.proc == nil || m.proc.stdin == nil || time.Since(m.lastPresence) < presencePing {
		return
	}
	if m.proc.controlCmd("presence", "") {
		m.lastPresence = time.Now()
	}
}
