package main

import (
	"encoding/json"
	"time"

	tea "charm.land/bubbletea/v2"
)

// Interrupting a turn from the TUI: ctrl+g (or esc, when nothing else takes
// it) while a turn runs sends the engine's interrupt command. The turn ends
// with done{reason: interrupted}; what it wrote stays, and the next message
// continues from there. ctrl+c interrupts too; a second ctrl+c within
// quitAgain quits this TUI (the session keeps running). Idle, ctrl+c quits.

// interruptKey interrupts a running turn. ctrl+g is "cancel" in emacs and
// readline; ctrl+k was tried first and took the composer's kill-to-end-of-line
// while a turn ran (USER 2026-10-22).
const interruptKey = "ctrl+g"

// quitAgain is how soon a second ctrl+c after one that interrupted a turn
// quits instead.
const quitAgain = 2 * time.Second

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
		return "ctrl+c or ctrl+g interrupt · /exit to exit"
	}
	return "ctrl+c or ctrl+g interrupt · ctrl+c twice quits"
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
