package tui

import (
	"fmt"
	"strings"
	"time"

	"charm.land/lipgloss/v2"

	"github.com/iodesystems/dun/internal/textfmt"
)

// Thinking: the model's reasoning, one line by default.
//
// One block per round. The engine's `reasoning` events are deltas; the first
// one of a round opens a block, LIVE, and every later one grows it. The
// round's first reply token, its first tool call, or the end of the turn
// closes it.
//
// FOLDED (the default) the block is ONE line (thoughtLine): the headline
// (thinkHeadline) and how long and how much. dun's prompt asks the model to
// start each step with `# Thinking: <what>` and end with `# Summary: <what>`;
// the line is the latest Thinking heading while it streams, swapped in place,
// and the Summary once it ends. ctrl+t unfolds every thought, those on
// screen and in tool runs too, to its full text in a box (box.go) that
// follows its tail while it streams; ctrl+t again folds them. A folded
// thought is one row live and finished, so the end of a round moves nothing;
// toggling is the one time a block's height changes on screen, and the view
// keeps its top row (toggleThinking).
//
// It is an ordinary convo block in every other way: ↑/↓ select it (and
// scroll inside it while selected), and enter opens it in the pager
// (pager.go), folded or not, where a long chain of thought can be read at
// full height.
//
// Nothing here moves the viewport. A growing block is new content like a
// streamed reply, and applyScroll decides what that does: a pinned reader
// follows, a reader who scrolled away stays put.

type thinkBlock struct {
	text       string
	start, end time.Time // end is zero while live; both zero from history
	live       bool
	cut        bool // the user interrupted the turn mid-thought
	full       bool // drawn as its whole text, not the one line (ctrl+t)
}

// liveThought is the thought still streaming and the block it is in — its
// own box, or a run's (toolrun.go) — or nil, -1.
func (m *tuiModel) liveThought() (*thinkBlock, int) {
	for i := len(m.convo) - 1; i >= 0; i-- {
		e := m.convo[i]
		if e.think != nil && e.think.live {
			return e.think, i
		}
		if e.run != nil && e.run.live() {
			return e.run.items[len(e.run.items)-1].think, i
		}
	}
	return nil, -1
}

// liveThinking is the index of the block with a thought still streaming, or -1.
func (m *tuiModel) liveThinking() int {
	_, i := m.liveThought()
	return i
}

// touchThought redraws block i after its thought changed.
func (m *tuiModel) touchThought(i int) {
	if m.convo[i].run != nil {
		m.touchRun(i)
		return
	}
	m.convo[i].renderThink()
}

// addThought puts a thought in its place: inside the open run, if there is
// one, else in a box of its own. From addThinking (a new, live thought) and
// from replay (a finished one).
func (m *tuiModel) addThought(t *thinkBlock) int {
	t.full = m.thinkFull
	if r := m.openRun(); r >= 0 {
		m.convo[r].run.items = append(m.convo[r].run.items, &runItem{think: t})
		m.touchRun(r)
		return r
	}
	// A new round. Anything streamed before it belongs above it.
	m.flushCur()
	m.convo = append(m.convo, thinkingEntry(t))
	return len(m.convo) - 1
}

// toggleThinking flips every thought between its one line and its full text,
// those in tool runs too. The blocks start their heights over (the held rows
// would leave blank ones), and the view keeps the row it had at its top.
func (m *tuiModel) toggleThinking() {
	m.thinkFull = !m.thinkFull
	anchor, off := -1, 0
	for i := range m.convo {
		if e := &m.convo[i]; e.rowOffset+e.lastH > m.vp.YOffset {
			anchor, off = i, e.rowOffset-m.vp.YOffset
			break
		}
	}
	for i := range m.convo {
		e := &m.convo[i]
		switch {
		case e.think != nil:
			e.think.full = m.thinkFull
		case e.run != nil:
			for _, it := range e.run.items {
				if it.think != nil {
					it.think.full = m.thinkFull
				}
			}
			e.run.ver++
		default:
			continue
		}
		e.foldEntry()
	}
	m.holdSkip = true
	m.refresh()
	if anchor >= 0 && !m.scrollPinned {
		m.vp.SetYOffset(m.convo[anchor].rowOffset - off)
		m.updateScrollPin()
	}
}

// addThinking takes one reasoning delta: it opens the round's thought if none
// is live, and grows it. Rendering is left to the tick, as for tokens.
func (m *tuiModel) addThinking(delta string) {
	t, i := m.liveThought()
	if t == nil {
		t = &thinkBlock{start: time.Now(), live: true}
		i = m.addThought(t)
	}
	t.text += delta
	m.touchThought(i)
	m.renderDue = true
}

// endThinking closes the live thought, if there is one: the round's thinking
// is over. Its block keeps its height: one row folded, the box as tall as it
// got unfolded.
func (m *tuiModel) endThinking() {
	t, i := m.liveThought()
	if t == nil {
		return
	}
	t.live = false
	t.end = time.Now()
	m.touchThought(i)
}

// thinkingEntry is a block for t, boxed and following its tail.
func thinkingEntry(t *thinkBlock) convoEntry {
	e := convoEntry{think: t, box: &boxState{follow: true}}
	e.renderThink()
	return e
}

// renderThink writes the block's plain form, which is what search and the
// screen dump read; the screen draws the box (renderBox). It is the full
// text, folded or not, so a fold hides nothing from search.
func (e *convoEntry) renderThink() {
	// Plain concatenation: this runs on every delta, and styling the whole
	// thought each time was the cost of a frame.
	e.collapsed = StDim.Render(thinkHeader(e.think)) + "\n" + strings.TrimSpace(e.think.text)
	e.invalidateWrap()
}

// thinkHeader is the unfolded box's top line: what it is, how long, how much.
func thinkHeader(t *thinkBlock) string {
	words := fmt.Sprintf(" · %d words", len(strings.Fields(t.text)))
	switch {
	case t.live:
		return "✻ thinking… " + thinkDur(time.Since(t.start)) + words
	case t.cut && !t.end.IsZero():
		return "✻ interrupted after " + thinkDur(t.end.Sub(t.start)) + words
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

// thinkHeadline is the line a folded thought shows, from its text. A line
// `# Thinking: <what>` is a step's heading and `# Summary: <what>` the
// conclusion, matched without regard to case, leading spaces or extra #s.
// Live, it is the latest Thinking heading; finished, the last Summary. A
// model that ignores the instruction gets the last Thinking, else its first
// non-blank line. A line still arriving counts as it stands, so a heading
// grows in place; "# Thin" is not one yet. A heading inside a code fence
// counts like any other. "" for a thought with no text.
func thinkHeadline(text string, live bool) string {
	var thinking, summary, first string
	for _, ln := range strings.Split(text, "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		if first == "" {
			first = ln
		}
		if h, ok := strings.CutPrefix(ln, "#"); ok {
			h = strings.TrimSpace(strings.TrimLeft(h, "#"))
			if len(h) >= 8 && strings.EqualFold(h[:8], "thinking") {
				if v := headingValue(h[8:]); v != "" {
					thinking = v
				}
			} else if len(h) >= 7 && strings.EqualFold(h[:7], "summary") {
				if v := headingValue(h[7:]); v != "" {
					summary = v
				}
			}
		}
	}
	switch {
	case !live && summary != "":
		return summary
	case thinking != "":
		return thinking
	}
	return strings.TrimSpace(strings.TrimLeft(first, "#"))
}

// headingValue is what follows a heading's keyword: ": text", else "".
func headingValue(rest string) string {
	rest = strings.TrimSpace(rest)
	if !strings.HasPrefix(rest, ":") {
		return ""
	}
	return strings.TrimSpace(textfmt.OneLine(rest[1:]))
}

// thoughtLine is a folded thought as one line: its headline, then how long
// and how much. A thought with no text (Claude returns none by default) says
// what it is instead, and has no word count to give.
func thoughtLine(t *thinkBlock, w int) string {
	head := thinkHeadline(t.text, t.live)
	if head == "" {
		switch {
		case t.live:
			head = "thinking"
		case t.cut:
			head = "interrupted"
		default:
			head = "thought"
		}
	}
	meta := func(words string) string {
		var parts []string
		if d := thoughtTime(t); d != "–" || !t.live {
			if t.live {
				d += "…"
			}
			parts = append(parts, d)
		}
		if n := len(strings.Fields(t.text)); n > 0 {
			parts = append(parts, fmt.Sprintf(words, n))
		}
		s := strings.Join(parts, ", ")
		if t.cut {
			s = "⏹ " + s
		}
		return "  (" + strings.TrimSpace(s) + ")"
	}
	m := meta("%d words")
	if w-2-lipgloss.Width(m) < 14 { // a phone: the count in a letter
		m = meta("%dw")
	}
	head, _ = fitPlain(head, max(1, w-2-lipgloss.Width(m)))
	return stSecondary.Render("✻ "+head) + StDim.Render(m)
}

func thoughtTime(t *thinkBlock) string {
	if t.live {
		return thinkDur(time.Since(t.start))
	}
	if t.end.IsZero() || t.start.IsZero() {
		return "–"
	}
	return thinkDur(t.end.Sub(t.start))
}
