package main

import (
	"fmt"
	"strings"
	"time"
)

// Thinking: the model's reasoning, shown in a box (box.go) while it streams,
// and kept there once the round moves on.
//
// One block per round. The engine's `reasoning` events are deltas; the first
// one of a round opens a block, LIVE, and every later one grows it. The
// round's first reply token, its first tool call, or the end of the turn
// closes it: the header says "thought for Ns" instead of "thinking…", and
// nothing else changes — the box keeps its height, so the end of a round
// moves nothing.
//
// It is an ordinary convo block in every other way: ↑/↓ select it (and
// scroll inside it while selected), and enter opens it in the pager
// (pager.go), where a long chain of thought can be read at full height.
//
// Nothing here moves the viewport. A growing block is new content like a
// streamed reply, and applyScroll decides what that does: a pinned reader
// follows, a reader who scrolled away stays put.

type thinkBlock struct {
	text       string
	start, end time.Time // end is zero while live; both zero from history
	live       bool
}

// liveThinking is the index of the block still streaming, or -1.
func (m *tuiModel) liveThinking() int {
	for i := len(m.convo) - 1; i >= 0; i-- {
		if t := m.convo[i].think; t != nil && t.live {
			return i
		}
	}
	return -1
}

// addThinking takes one reasoning delta: it opens the round's block if none
// is live, and grows it. Rendering is left to the tick, as for tokens.
func (m *tuiModel) addThinking(delta string) {
	i := m.liveThinking()
	if i < 0 {
		// A new round. Anything streamed before it belongs above it.
		m.flushCur()
		m.convo = append(m.convo, thinkingEntry(&thinkBlock{start: time.Now(), live: true}))
		i = len(m.convo) - 1
	}
	e := &m.convo[i]
	e.think.text += delta
	e.renderThink()
	m.renderDue = true
}

// endThinking closes the live block, if there is one: the round's thinking is
// over. Only its header changes.
func (m *tuiModel) endThinking() {
	i := m.liveThinking()
	if i < 0 {
		return
	}
	e := &m.convo[i]
	e.think.live = false
	e.think.end = time.Now()
	e.renderThink()
}

// thinkingEntry is a block for t, boxed and following its tail.
func thinkingEntry(t *thinkBlock) convoEntry {
	e := convoEntry{think: t, box: &boxState{follow: true}}
	e.renderThink()
	return e
}

// renderThink writes the block's plain form, which is what search and the
// screen dump read; the screen draws the box (renderBox).
func (e *convoEntry) renderThink() {
	// Plain concatenation: this runs on every delta, and styling the whole
	// thought each time was the cost of a frame.
	e.collapsed = stDim.Render(thinkHeader(e.think)) + "\n" + strings.TrimSpace(e.think.text)
	e.invalidateWrap()
}

// thinkHeader is the box's top line: what it is, how long, how much.
func thinkHeader(t *thinkBlock) string {
	words := fmt.Sprintf(" · %d words", len(strings.Fields(t.text)))
	switch {
	case t.live:
		return "✻ thinking… " + thinkDur(time.Since(t.start)) + words
	case !t.end.IsZero():
		return "✻ thought for " + thinkDur(t.end.Sub(t.start)) + words
	}
	return "✻ thought" + words
}

func thinkDur(d time.Duration) string {
	if d < time.Second {
		return "<1s"
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
}
