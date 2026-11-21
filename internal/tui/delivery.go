package tui

import "slices"

// Queued messages, by id (dun/delivery.go). The engine says when each one
// reached the model (delivered) or was taken back (withdrawn); the TUI does
// not guess it from a tool result or a turn's end.

// queuedRow is the index of the provisional row for message id, or -1.
func (m *tuiModel) queuedRow(id string) int {
	if id == "" {
		return -1
	}
	return slices.IndexFunc(m.convo, func(e convoEntry) bool { return (e.provisional || e.lost) && e.msgID == id })
}

// settleDelivered makes each delivered message an ordinary user row, after
// what is on screen: where history puts it (after the tool result that
// carried it), so a reconnect shows the same order. A row already last stays
// where it is.
func (m *tuiModel) settleDelivered(ids []string) {
	for _, id := range ids {
		i := m.queuedRow(id)
		if i < 0 {
			continue
		}
		text := m.convo[i].provisionalText
		sel := m.sel == i
		m.convo = append(m.convo[:i], m.convo[i+1:]...)
		if m.sel > i {
			m.sel--
		}
		// Delivered: the engine put the message into the conversation, so
		// the reply streamed before it is finished, and goes above it.
		m.flushCur()
		m.endRun() // as history's user item does
		m.convo = append(m.convo, convoEntry{collapsed: userRow(text), userText: text, msgID: id})
		if sel {
			m.sel = len(m.convo) - 1
		}
	}
}

// withdraw takes the rows of messages taken back out, and returns their
// texts, in the order the ids came (oldest first).
func (m *tuiModel) withdraw(ids []string) []string {
	var texts []string
	for _, id := range ids {
		if i := m.queuedRow(id); i >= 0 {
			texts = append(texts, m.convo[i].provisionalText)
			m.convo = append(m.convo[:i], m.convo[i+1:]...)
		}
	}
	return texts
}

// settleUnnamedQueued is the fallback for an event log from before message
// ids (a DUN_TRACE recorded by an older engine): a queued row with no id can
// get no delivered event, so the turn's end settles it, as the TUI used to.
// Rows with an id are left to the engine's events.
func (m *tuiModel) settleUnnamedQueued() {
	m.sinkRows(func(e *convoEntry) bool {
		if !e.provisional || e.msgID != "" {
			return false
		}
		e.provisional = false
		e.collapsed = userRow(e.provisionalText)
		e.userText = e.provisionalText
		e.provisionalText = ""
		e.invalidateWrap()
		return true
	})
}

// sinkRows moves the rows settle reports true for (it may change them) to
// the end of the conversation, in their order: a provisional row settled is
// drawn where it was, below the rest (drawOrder). The selection keeps its
// row.
func (m *tuiModel) sinkRows(settle func(*convoEntry) bool) {
	var kept, sunk []convoEntry
	sel, selSunk := -1, false
	for i := range m.convo {
		if settle(&m.convo[i]) {
			if i == m.sel {
				sel, selSunk = len(sunk), true
			}
			sunk = append(sunk, m.convo[i])
			continue
		}
		if i == m.sel {
			sel = len(kept)
		}
		kept = append(kept, m.convo[i])
	}
	if len(sunk) == 0 {
		return
	}
	if selSunk {
		sel += len(kept)
	}
	m.convo = append(kept, sunk...)
	if m.sel >= 0 {
		m.sel = sel
	}
}
