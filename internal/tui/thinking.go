package tui

import (
	"fmt"
	"strings"
	"sync"
	"time"

	glamouransi "charm.land/glamour/v2/ansi"
	"charm.land/lipgloss/v2"
	"github.com/iodesystems/bubbleteandmd"

	"github.com/iodesystems/dun/internal/textfmt"
)

// Thinking: the model's reasoning, one line by default.
//
// One block per round. The engine's `reasoning` events are deltas, each
// naming its model round; the first one of a round opens a block, LIVE, and
// every later one grows it. The engine's `reasoning_end` closes it
// (docs/session-events.md, "Thinking rounds"); a delta of another round opens
// a new one. An event log from before round ids has neither: then the
// round's first reply token, its first tool call, or the end of the turn
// closes it (endThinkingOld).
//
// FOLDED (the default) the block is ONE line (thoughtLine): the headline
// (thinkHeadline) and how long and how much. dun's prompt asks the model to
// open each step with a bold title line, `**<what>**`; the line is the
// latest title, swapped in place as the thought streams (thinkTitle has the
// rules, and the older `# Thinking:` / `# Summary:` headings). ctrl+t
// unfolds every thought, those on screen and in tool runs too, to its full
// text, rendered as Markdown (thinkBody), in a box (box.go) that
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
	round      int  // the model round it is from; 0 from history or an older engine
	md         thinkMD
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

// addThinking takes one reasoning delta of round (0: not named): it opens
// the round's thought if none is live, and grows it. A delta of another
// round than the live thought's ends that one first. Rendering is left to
// the tick, as for tokens.
func (m *tuiModel) addThinking(delta string, round int) {
	t, i := m.liveThought()
	if t != nil && round > 0 && t.round != round {
		m.endThinking()
		t = nil
	}
	if t == nil {
		t = &thinkBlock{start: time.Now(), live: true, round: round}
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

// endThinkingOld is the fallback for an event log from before round ids:
// the event that ends the round's thinking is guessed (a token, a tool call,
// a question, an error, a retry, the turn's end). Once an event named its
// round (m.rounds), reasoning_end says it and this does nothing.
func (m *tuiModel) endThinkingOld() {
	if !m.rounds {
		m.endThinking()
	}
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

// thinkHeader is the unfolded box's top line: what it is, how long, how
// much. A thought with a title (thinkTitle) says the title where the generic
// "thinking…" / "thought for" was.
func thinkHeader(t *thinkBlock) string {
	words := fmt.Sprintf(" · %d words", len(strings.Fields(t.text)))
	if title := thinkTitle(t.text, t.live); title != "" {
		s := "✻ " + title
		if d := thoughtTime(t); d != "–" { // "–": from history, no times
			switch {
			case t.live:
				d += "…"
			case t.cut:
				d = "⏹ " + d
			}
			s += " · " + d
		}
		return s + words
	}
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

// thinkHeadline is the line a folded thought shows, from its text: its
// title (thinkTitle), else its first non-blank line. "" for a thought with
// no text.
func thinkHeadline(text string, live bool) string {
	if title := thinkTitle(text, live); title != "" {
		return title
	}
	for _, ln := range strings.Split(text, "\n") {
		if ln = strings.TrimSpace(ln); ln != "" {
			return strings.TrimSpace(strings.TrimLeft(ln, "#"))
		}
	}
	return ""
}

// thinkTitle is what the thought says it is doing, "" when it says nothing.
// dun's prompt asks the model to open each step with a bold title line,
// `**<what>**` (USER 2026-11-18; Claude does it unasked). A title is a line
// that is only a bold span, `**…**` or `__…__`, a colon after it or inside
// it allowed; bold inside a sentence is not one. Logs from before that have
// `# Thinking: <what>` step headings and a `# Summary: <what>`, matched
// without regard to case, leading spaces or extra #s. Live, it is the latest
// title; finished, the last Summary, else the latest title. A line still
// arriving counts as it stands: "# Thin" is not a heading yet, and a bold
// span counts once it is closed. A title inside a code fence counts like
// any other.
func thinkTitle(text string, live bool) string {
	var title, summary string
	for _, ln := range strings.Split(text, "\n") {
		ln = strings.TrimSpace(ln)
		if v := boldTitle(ln); v != "" {
			title = v
			continue
		}
		h, ok := strings.CutPrefix(ln, "#")
		if !ok {
			continue
		}
		h = strings.TrimSpace(strings.TrimLeft(h, "#"))
		if len(h) >= 8 && strings.EqualFold(h[:8], "thinking") {
			if v := headingValue(h[8:]); v != "" {
				title = v
			}
		} else if len(h) >= 7 && strings.EqualFold(h[:7], "summary") {
			if v := headingValue(h[7:]); v != "" {
				summary = v
			}
		}
	}
	if !live && summary != "" {
		return summary
	}
	return title
}

// boldTitle is the text of a line that is only a bold span — `**what**`,
// `__what__`, `**what**:` or `**what:**` — else "".
func boldTitle(ln string) string {
	ln = strings.TrimSuffix(ln, ":")
	for _, m := range []string{"**", "__"} {
		in, ok := strings.CutPrefix(ln, m)
		if !ok {
			continue
		}
		if in, ok = strings.CutSuffix(in, m); !ok || strings.Contains(in, m) {
			return ""
		}
		in = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(in), ":"))
		return textfmt.OneLine(in)
	}
	return ""
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

// ── the body as Markdown ───────────────────────────────────────────

// An unfolded thought's body is Markdown, rendered like a reply (answer.go):
// a bubbleteandmd Stream, Frame while it streams and Final once done, its
// stable blocks kept in a cache so a frame costs the tail. The style is the
// reply's with the prose in the thought's colour (stSecondary) and no
// margin, so a thought still reads one step below the answer; bold titles
// stay bold, code stays code. The prose column is the reply's rule
// (proseW): the box's inner width, capped at readingCap.

// thinkCache is the thoughts' rendered-block cache: its own, so a long
// chain of thought does not push a reply's blocks out of answerCache.
var thinkCache = bubbleteandmd.NewCache(256)

// thinkRenderers are the thought renderers by prose column.
var thinkRenderers struct {
	sync.Mutex
	rs map[int]*bubbleteandmd.DirectiveRenderer
}

// thinkMD is a thought's Stream and its last render.
type thinkMD struct {
	s        *bubbleteandmd.Stream
	pw       int
	src, out string
	live     bool
}

// thinkStyle is the reply style with the prose in the thought's colour and
// no document margin (the box's edge is the margin).
func thinkStyle() glamouransi.StyleConfig {
	s := mdStyleConfig()
	fg := "246" // stSecondary (tui_view.go); 240 on a light terminal
	if mdStyle == "light" {
		fg = "240"
	}
	var zero uint
	s.Document.Color, s.Document.Margin = &fg, &zero
	return s
}

func thinkRenderer(pw int) *bubbleteandmd.DirectiveRenderer {
	initMarkdownStyle() // no-op after the first call
	thinkRenderers.Lock()
	defer thinkRenderers.Unlock()
	if r, ok := thinkRenderers.rs[pw]; ok {
		return r
	}
	r, err := bubbleteandmd.NewRenderer(thinkStyle(), pw)
	if err != nil {
		return nil
	}
	if thinkRenderers.rs == nil || len(thinkRenderers.rs) >= answerRenderersMax {
		thinkRenderers.rs = map[int]*bubbleteandmd.DirectiveRenderer{}
	}
	thinkRenderers.rs[pw] = r
	return r
}

// thinkBody is t's text rendered for a box body ww columns wide; the
// source, sanitized, when no renderer can be built. A render that did not
// change (same text, width and liveness) is the last one.
func thinkBody(t *thinkBlock, ww int) string {
	src := strings.TrimSpace(t.text)
	pw := max(20, min(ww, readingCap))
	md := &t.md
	if md.s != nil && md.pw == pw && md.src == src && md.live == t.live {
		return md.out
	}
	if md.s == nil || md.pw != pw {
		r := thinkRenderer(pw)
		if r == nil {
			return bubbleteandmd.Sanitize(src)
		}
		md.s = bubbleteandmd.NewStream(r, fmt.Sprintf("think/%s/%d", mdStyle, pw), thinkCache)
		md.pw = pw
	}
	in := titleBlocks(demoteStepHeads(textfmt.FenceToolMarkup(src))) // demote: old logs' step headings
	var out string
	var err error
	if t.live {
		out, err = md.s.Frame(in)
	} else {
		out, err = md.s.Final(in)
	}
	if err != nil {
		return bubbleteandmd.Sanitize(src)
	}
	// Glamour pads its rows to the column: a list or a code block starts
	// with a row that is only spaces.
	rows := strings.Split(out, "\n")
	for len(rows) > 0 && strings.TrimSpace(stripANSI(rows[0])) == "" {
		rows = rows[1:]
	}
	md.src, md.live, md.out = src, t.live, strings.TrimRight(strings.Join(rows, "\n"), "\n")
	return md.out
}

// titleBlocks puts a title line (boldTitle) in a paragraph of its own:
// Markdown joins it to the line under it ("**Plan**\nread x" is one line,
// "Plan read x"), and the title is what the body is skimmed by. Fenced code
// is left as written.
func titleBlocks(text string) string {
	if !strings.Contains(text, "**") && !strings.Contains(text, "__") {
		return text
	}
	lines := strings.Split(text, "\n")
	fence := ""
	for i, ln := range lines {
		t := strings.TrimSpace(ln)
		if fence != "" {
			if strings.HasPrefix(t, fence) {
				fence = ""
			}
			continue
		}
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			fence = t[:3]
			continue
		}
		if boldTitle(t) != "" {
			lines[i] = "\n" + ln + "\n"
		}
	}
	return strings.Join(lines, "\n")
}
