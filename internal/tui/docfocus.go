package tui

import (
	tea "charm.land/bubbletea/v2"
	"github.com/iodesystems/bubbleteandmd"
)

// Forms in an answer: a reply's directive forms (a DETAILS fold, tabs, a
// tree) take keys through its bubbleteandmd Doc (answer.go keeps one per
// reply, and a finished reply keeps its Doc, so folds and tabs survive a
// redraw and a width change).
//
// Focus enters from the conversation, never from the composer: tab is the
// composer's completion and the zone cycle there. With an answer selected
// (focusConvo), enter gives its Doc the keys; → does too when the answer has
// no block cut at the edge (→ descends into the wide blocks first, wide.go).
// The selected answer is the one: the reply the person is on, not the last
// reply with forms. Inside: tab/shift+tab move between forms, enter/space
// toggle, ←/→ switch tabs or move a tree's cursor. Leaving is one level out,
// to the answer selected, and the key is consumed: tab past the last form,
// shift+tab before the first, esc, and ← on a form that does not take it
// (as ← at a wide block's left edge). ↑/↓ and every key the Doc does not take
// do their job as before (scroll within a tall answer, move the selection);
// the forms keep the keys while the answer stays selected.
//
// The Doc has no height (height 0): dun keeps its own scroll, and a focus
// change scrolls the focused form into view (revealDoc). A fold or tab
// switch is the person's action, so the answer may shrink: its held height
// (maxH) starts over. Focus shows as the library's same-width glyph swap;
// nothing is dimmed.

// docKeyMap is the Doc's keys (bubbleteandmd.DefaultKeyMap): fixed, like a
// pager's, so the composer's key map does not apply.
var docKeyMap = bubbleteandmd.DefaultKeyMap()

// docEntry is the answer whose forms have the keys, or nil. An entry whose
// Doc has no focus is none: the conversation was replaced under docAt.
func (m tuiModel) docEntry() *convoEntry {
	i := m.docAt - 1
	if i < 0 || i >= len(m.convo) || m.convo[i].ans == nil || m.convo[i].ans.State().Focused == "" {
		return nil
	}
	return &m.convo[i]
}

// selForms says the selected answer has a form that takes keys.
func (m tuiModel) selForms() bool {
	if m.sel < 0 || m.sel >= len(m.convo) {
		return false
	}
	d := m.convo[m.sel].ans
	return d != nil && len(d.Stops()) > 0
}

// enterDoc gives the selected answer's forms the keys; false when it has
// none.
func (m *tuiModel) enterDoc() bool {
	if !m.selForms() {
		return false
	}
	e := &m.convo[m.sel]
	e.ans.Focus()
	m.docAt = m.sel + 1
	e.docChanged(false)
	m.docReveal = true
	return true
}

// leaveDoc takes the keys back from the forms.
func (m *tuiModel) leaveDoc() {
	if e := m.docEntry(); e != nil {
		e.ans.Blur()
		e.docChanged(false)
	}
	m.docAt = 0
}

// docSync takes the keys back when the answer is no longer the selection in
// the conversation (↑/↓ moved on, a click, another zone).
func (m *tuiModel) docSync() {
	if m.docAt == 0 {
		return
	}
	if m.docEntry() == nil || m.focus != focusConvo || m.sel != m.docAt-1 {
		m.leaveDoc()
	}
}

// docChanged has refresh render the answer again; layout: the forms changed
// shape (a fold, a tab), and the held height starts over.
func (e *convoEntry) docChanged(layout bool) {
	e.wrapW = 0
	e.invalidateWrap()
	if layout {
		e.maxH = 0
	}
}

// docKeys is the keyboard while an answer's forms have the keys. It reports
// whether the key was taken (and refreshed).
func (m *tuiModel) docKeys(msg tea.KeyPressMsg) bool {
	m.docSync()
	e := m.docEntry()
	if e == nil {
		return false
	}
	a, _ := docKeyMap.Action(msg.String())
	if e.ans.HandleKey(msg, docKeyMap) {
		e.docChanged(a == bubbleteandmd.ActionToggle || a == bubbleteandmd.ActionLeft || a == bubbleteandmd.ActionRight)
		m.docReveal = a == bubbleteandmd.ActionNext || a == bubbleteandmd.ActionPrev
		m.refresh()
		return true
	}
	switch {
	case e.ans.State().Focused == "": // the Doc left: tab past the last form, shift+tab before the first, esc
		m.docAt = 0
		e.docChanged(false)
	case a == bubbleteandmd.ActionLeft:
		m.leaveDoc()
	case a == bubbleteandmd.ActionRight, a == bubbleteandmd.ActionToggle:
		// Nothing to do on this form; not the conversation's either.
	default:
		return false
	}
	m.refresh()
	return true
}

// revealDoc scrolls the focused form into view after a focus change: the
// least move that shows it, its top line when it is taller than the pane.
// Called by refresh, which knows the rows.
func (m *tuiModel) revealDoc() {
	if !m.docReveal {
		return
	}
	m.docReveal = false
	e := m.docEntry()
	vh := m.vp.Height
	if e == nil || vh <= 0 {
		return
	}
	s, ok := e.ans.Focused()
	if !ok {
		return
	}
	top := e.rowOffset + s.Line - e.ansLead
	end := top + max(1, s.Height)
	// The view's top row is under the head bar when it shows (scrollOverlay).
	head := func() int {
		if m.overlayQuestion() != "" {
			return 1
		}
		return 0
	}
	y := m.vp.YOffset
	switch {
	case top < y+head():
		m.vp.SetYOffset(top)
		m.updateScrollPin()
		if top > 0 && head() > 0 {
			m.vp.SetYOffset(top - 1)
		}
	case end > y+vh:
		m.vp.SetYOffset(min(top-head(), end-vh))
	default:
		return
	}
	m.updateScrollPin()
}
