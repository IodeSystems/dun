package tui

// rebuildFrom puts the conversation on screen again from a snapshot's items:
// a reconnect that came back past the replay window (or after a dund
// restart) gets a snapshot, and what happened while this client was away is
// only in it. The conversation rows are replaced, not added to, so nothing
// shows twice; the splash at the top stays. Kept: the composer (draft), the
// open question and its picker, the pager or job view, and the scroll: a
// reader at the bottom stays at the bottom, one scrolled up stays at the
// same offset. Lost: this client's own lines (notes, command output), which
// no snapshot holds.
func (m *tuiModel) rebuildFrom(items []any) {
	pinned, yoff := m.scrollPinned, m.vp.YOffset
	selID := ""
	if m.sel >= 0 && m.sel < len(m.convo) {
		selID = m.convo[m.sel].msgID
	}
	var ask *convoEntry
	if i := m.askEntry(); i >= 0 && m.asking {
		q := m.convo[i]
		ask = &q
	}
	keep := 0
	for keep < len(m.convo) && (m.convo[keep].splash || m.convo[keep].liveSplash) {
		keep++
	}
	m.convo = m.convo[:keep:keep]
	m.cur, m.curMaxH, m.curMaxKey = "", 0, 0
	m.dropHold()
	m.quietReplay = true // nothing was resumed: the same conversation, caught up
	m.replay(items)
	// The open question's row: the state that follows reopens a question
	// only when none is open, so one still open keeps its row here.
	if ask != nil && m.askEntry() < 0 {
		m.convo = append(m.convo, *ask)
	}
	m.sel = -1
	if selID != "" {
		for i, e := range m.convo {
			if e.msgID == selID {
				m.sel = i
			}
		}
	}
	m.scrollPinned = pinned
	m.refresh()
	if !pinned {
		m.vp.SetYOffset(yoff)
		m.updateScrollPin()
	}
}
