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
	return slices.IndexFunc(m.convo, func(e convoEntry) bool { return e.provisional && e.msgID == id })
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
		m.convo = append(m.convo[:i], m.convo[i+1:]...)
		m.endRun() // as history's user item does
		m.convo = append(m.convo, convoEntry{collapsed: userRow(text), userText: text, msgID: id})
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
	for i := range m.convo {
		if e := &m.convo[i]; e.provisional && e.msgID == "" {
			e.provisional = false
			e.collapsed = userRow(e.provisionalText)
			e.userText = e.provisionalText
			e.provisionalText = ""
			e.invalidateWrap()
		}
	}
}
