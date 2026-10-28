package main

// Folding under scroll pressure (owner: "not yielding matters most").
//
// A block gets shorter in two ways: its content folds (a run's finished
// thought becomes one line), or it holds rows it no longer fills (maxH, a
// box's peak — the blank rows that keep the conversation from moving). Both
// wait until the block is above the view: a block on screen keeps its full
// height and its content, so the rows the person is reading never move and
// no blank rows open among them. Once the scroll has pushed a block off the
// top — the stream pinned to the bottom, or the person scrolling down — the
// next refresh folds it and draws it at its own height, and the view's top
// moves up by the rows it gave back (refresh), so every visible row stays
// where it was. Scrolling back up shows it folded.
//
// Not while something else holds the view (a closed question's blank rows,
// a panel's hold, a mouse highlight, a width change): those keep offsets of
// their own, and the fold waits for a later refresh.

// foldAboveOK says this refresh may fold the blocks above the view.
func (m *tuiModel) foldAboveOK() bool {
	return m.frame.width != 0 && m.frame.width == m.vp.Width && m.vp.Height > 0 &&
		m.convoHold == 0 && !m.panelHeld && !m.panelOpen() && !m.drag.on && !m.holdSkip
}

// aboveView says entry i was wholly above the view's top row y at the last
// refresh, and is not the selection (selMode: the selected block is the
// person's, and applyScroll is about to bring it into view).
func (m *tuiModel) aboveView(i, y int, selMode bool) bool {
	e := &m.convo[i]
	if e.blank || e.docs != nil || e.lastH == 0 || (selMode && i == m.sel) {
		return false
	}
	return e.rowOffset+e.lastH <= y
}

// foldEntry readies entry i, above the view, to draw at its own height: a
// run's finished thoughts fold to one line, and the held heights start over.
func (e *convoEntry) foldEntry() {
	if e.run != nil {
		for _, it := range e.run.items {
			if it.think != nil && !it.think.live && !it.folded {
				it.folded = true
				e.run.ver++
			}
		}
	}
	if e.box != nil {
		e.box.peak = 0
	}
	e.maxH = 0
}
