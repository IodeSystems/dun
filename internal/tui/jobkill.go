package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// Killing background jobs from the TUI. The engine does the work (/jobs,
// commands.go jobsCmd), as for every client; this file is the TUI's two ways
// in, each behind a y/n question on the status line:
//
//   - x on a running job in the activity pane: "kill job N? (y/n)";
//   - /jobs kill all: "kill every running job? (y/n)".

// confirmState is one pending one-key question. While q is set it owns the
// keys: y runs yes (or, with keys, the key's own answer), anything else
// drops it.
type confirmState struct {
	q    string
	yes  func(m *tuiModel)
	keys map[string]func(m *tuiModel) // a question with more answers than y/n (clearconfirm.go)
}

// askConfirm puts a y/n question on the status line.
func (m *tuiModel) askConfirm(q string, yes func(m *tuiModel)) {
	m.confirm = confirmState{q: q, yes: yes}
	m.refresh()
}

// askChoice puts a question answered by one of keys' keys on the status
// line; any other key drops it.
func (m *tuiModel) askChoice(q string, keys map[string]func(m *tuiModel)) {
	m.confirm = confirmState{q: q, keys: keys}
	m.refresh()
}

// updateConfirm answers the pending question with one key.
func (m tuiModel) updateConfirm(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	c := m.confirm
	m.confirm = confirmState{}
	if f := c.keys[strings.ToLower(msg.String())]; f != nil {
		f(&m)
	} else if k := msg.String(); c.keys == nil && (k == "y" || k == "Y") {
		c.yes(&m)
	} else {
		m.append(StDim.Render("not done: " + c.q))
	}
	m.refresh()
	return m, nil
}

// killJobsCmd sends a /jobs line to the engine.
func (m *tuiModel) killJobsCmd(line string) {
	if !m.proc.controlCmd("slash", line) {
		m.append(StErr.Render("no engine right now — /reconnect first"))
	}
}

// actKill is x in the activity pane: ask, then kill the selected job.
func (m *tuiModel) actKill() {
	row := m.actSelected()
	if row == nil || row.key.agent {
		return
	}
	j := m.jobByID(row.key.id)
	if j == nil {
		return
	}
	if j.state != "running" {
		m.append(StDim.Render(fmt.Sprintf("job %d already finished (%s)", j.id, j.state)))
		m.refresh()
		return
	}
	id := j.id
	m.askConfirm(fmt.Sprintf("kill job %d? (y/n)", id), func(m *tuiModel) {
		m.killJobsCmd(fmt.Sprintf("jobs kill %d", id))
	})
}
