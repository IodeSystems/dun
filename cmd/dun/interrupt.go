package main

import (
	"encoding/json"

	tea "github.com/charmbracelet/bubbletea"
)

// Interrupting a turn from the TUI: ctrl+k (or esc, when nothing else takes
// it) while a turn runs sends the engine's interrupt command. The turn ends
// with done{reason: interrupted}; what it wrote stays, and the next message
// continues from there. ctrl+c does NOT interrupt: it quits this TUI, and the
// turn keeps running in the session.

// interruptedRow is the line an interrupted turn ends with.
const interruptedRow = "⏹ interrupted — your next message continues from here"

// interrupt asks the engine to stop the turn in flight.
func (p *dunProc) interrupt() bool {
	if p == nil {
		return false
	}
	return json.NewEncoder(p.stdin).Encode(map[string]string{"type": "interrupt"}) == nil
}

// turnRunning says there is a turn to interrupt: busy covers a turn waiting
// on a question too.
func (m tuiModel) turnRunning() bool { return m.busy }

// interrupt sends the interrupt and says so on the status line until the
// turn ends.
func (m tuiModel) interrupt() (tea.Model, tea.Cmd) {
	if m.interrupting {
		return m, nil // already asked; the engine answers once
	}
	if !m.proc.interrupt() {
		m.append(stErr.Render("no engine right now — nothing interrupted"))
		return m, nil
	}
	m.interrupting = true
	m.refresh()
	return m, nil
}

// busyHint is the key hint on the busy status line: what each key does.
func (m tuiModel) busyHint() string {
	if m.disableExit {
		return "ctrl+k interrupt · /exit to exit"
	}
	return "ctrl+k interrupt · ctrl+c quit (the turn keeps running)"
}

// markInterrupted folds the turn's open blocks as cut: the live thought says
// it was interrupted, and the open run marks it and fails its unfinished
// calls. Only adds or relabels rows (the no-shrink rule).
func (m *tuiModel) markInterrupted() {
	if t, _ := m.liveThought(); t != nil {
		t.cut = true
	}
	if i := m.openRun(); i >= 0 {
		r := m.convo[i].run
		r.interrupted = true
		for _, it := range r.items {
			if it.tool != "" && !it.done {
				it.done, it.failed = true, true
				if it.result == "" {
					it.result = "ERROR: interrupted"
				}
			}
		}
		m.touchRun(i)
	}
}
