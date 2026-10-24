package main

import (
	"fmt"

	tea "charm.land/bubbletea/v2"
)

// Killing background jobs from the TUI. The engine does the work (/jobs,
// commands.go jobsCmd), as for every client; this file is the TUI's two ways
// in, each behind a y/n question on the status line:
//
//   - x on a running job in the activity pane: "kill job N? (y/n)";
//   - /jobs kill all: "kill every running job? (y/n)".

// confirmState is one pending y/n question. While q is set it owns the keys:
// y runs yes, anything else drops it.
type confirmState struct {
	q   string
	yes func(m *tuiModel)
}

// askConfirm puts a y/n question on the status line.
func (m *tuiModel) askConfirm(q string, yes func(m *tuiModel)) {
	m.confirm = confirmState{q: q, yes: yes}
	m.refresh()
}

// updateConfirm answers the pending question with one key.
func (m tuiModel) updateConfirm(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	c := m.confirm
	m.confirm = confirmState{}
	if k := msg.String(); k == "y" || k == "Y" {
		c.yes(&m)
	} else {
		m.append(stDim.Render("not done: " + c.q))
	}
	m.refresh()
	return m, nil
}

// killJobsCmd sends a /jobs line to the engine.
func (m *tuiModel) killJobsCmd(line string) {
	if !m.proc.controlCmd("slash", line) {
		m.append(stErr.Render("no engine right now — /reconnect first"))
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
		m.append(stDim.Render(fmt.Sprintf("job %d already finished (%s)", j.id, j.state)))
		m.refresh()
		return
	}
	id := j.id
	m.askConfirm(fmt.Sprintf("kill job %d? (y/n)", id), func(m *tuiModel) {
		m.killJobsCmd(fmt.Sprintf("jobs kill %d", id))
	})
}
