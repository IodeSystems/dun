package tui

import (
	"slices"
	"strings"
)

// A fold in progress (dun's compacting event): one row that turns with the
// status spinner while the summary call runs, replaced in place by the
// compaction's 🗜 line when the fold applies, or by a dim line (or nothing)
// when dund says it ended without one. dund sends the end on every path, so
// the TUI does not time it out.

// compactingRow is the index of the fold-in-progress row, or -1.
func (m *tuiModel) compactingRow() int {
	return slices.IndexFunc(m.convo, func(e convoEntry) bool { return e.compacting })
}

// compactingLine is the row's text at the spinner's current frame.
func (m *tuiModel) compactingLine() string {
	return strings.TrimRight(m.spin.View(), " ") + " " + StDim.Render("Compacting…")
}

// startCompacting shows the row. The reply streamed so far is finished
// first: a fold runs between model requests, so the reply is whole, and the
// row belongs after it.
func (m *tuiModel) startCompacting() {
	m.flushCur()
	if m.compactingRow() >= 0 {
		return
	}
	m.convo = append(m.convo, convoEntry{collapsed: m.compactingLine(), compacting: true})
}

// endCompacting puts line where the row is, or appends it when there is no
// row (an engine before the compacting event). An empty line takes the row
// out and appends nothing.
func (m *tuiModel) endCompacting(line string) {
	i := m.compactingRow()
	switch {
	case i >= 0 && line != "":
		m.convo[i] = convoEntry{collapsed: line}
	case i >= 0:
		m.convo = slices.Delete(m.convo, i, i+1)
		if m.sel > i {
			m.sel--
		}
	case line != "":
		m.flushCur()
		m.append(line)
	}
}

// turnCompacting steps the row's spinner; it reports whether there is a row.
func (m *tuiModel) turnCompacting() bool {
	i := m.compactingRow()
	if i < 0 {
		return false
	}
	if l := m.compactingLine(); l != m.convo[i].collapsed {
		m.convo[i].collapsed = l
		m.convo[i].invalidateWrap()
	}
	return true
}
