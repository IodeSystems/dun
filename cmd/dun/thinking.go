package main

import (
	"fmt"
	"strings"
	"time"
)

// Thinking: the model's reasoning, shown while it streams and folded once the
// round moves on.
//
// One block per round. The engine's `reasoning` events are deltas; the first
// one of a round opens a block, LIVE and expanded, and every later one grows
// it. The round's first reply token, its first tool call, or the end of the
// turn closes it: the block folds to a one-line summary — unless the person
// has it selected, in which case it stays exactly as they are reading it.
//
// It is an ordinary convo block in every other way: ↑/↓ select it, →/← open
// and close it in place, and enter opens it in the pager (pager.go), where a
// long chain of thought can be read and scrolled at full height.
//
// Nothing here moves the viewport. A growing block is new content like a
// streamed reply, and applyScroll decides what that does: a pinned reader
// follows, a reader who scrolled away stays put.

type thinkBlock struct {
	text       string
	start, end time.Time // end is zero while live
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
		m.convo = append(m.convo, convoEntry{think: &thinkBlock{start: time.Now(), live: true}, state: viewExpanded})
		i = len(m.convo) - 1
	}
	e := &m.convo[i]
	e.think.text += delta
	e.renderThink()
	m.renderDue = true
}

// endThinking closes the live block, if there is one: the round's thinking is
// over. It folds, unless it is the block the person has selected.
func (m *tuiModel) endThinking() {
	i := m.liveThinking()
	if i < 0 {
		return
	}
	e := &m.convo[i]
	e.think.live = false
	e.think.end = time.Now()
	if !(m.focus == focusConvo && m.sel == i) {
		e.state = viewMinimized
	}
	e.renderThink()
}

// thinkingEntry is a finished block, from history.
func thinkingEntry(text string) convoEntry {
	e := convoEntry{think: &thinkBlock{text: text}}
	e.renderThink()
	return e
}

// renderThink writes the block's collapsed and full forms from its text.
func (e *convoEntry) renderThink() {
	t := e.think
	body := strings.TrimSpace(t.text)
	head := "✻ thinking…"
	if !t.live {
		head = "✻ thought"
		if !t.end.IsZero() {
			head += " for " + thinkDur(t.end.Sub(t.start))
		}
	}
	words := len(strings.Fields(body))
	summary := head + fmt.Sprintf(" · %d words", words)
	if first, _ := fitPlain(oneLine(firstLine(body)), 40); first != "" {
		summary += " — " + first
	}
	e.collapsed = stDim.Render("▸ " + summary)
	e.full = stDim.Render("▾ "+head) + "\n" + stDim.Render(body)
	e.invalidateWrap()
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
