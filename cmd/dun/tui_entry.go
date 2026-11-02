package main

import (
	"fmt"
	"strings"

	"github.com/iodesystems/dun/internal/numfmt"
)

// convoEntry is one conversation block. A block with a full form opens and
// closes (enter, →/←); a relevant-docs summary carries a docsBlock (nested
// navigation); a thinking block or a run of tool calls is a box (box.go),
// and enter opens it in the pager.
type convoEntry struct {
	collapsed string
	full      string
	state     viewState   // minimized | expanded
	docs      *docsBlock  // proactive-RAG summary (nil for normal blocks)
	think     *thinkBlock // a round's reasoning (thinking.go) → enter opens the pager
	run       *toolRun    // a run of tool calls and thinking (toolrun.go) → enter opens the pager
	msg       *agentMsg   // a message between agents (agentmsg.go) → enter opens the pager
	// box draws a thinking block or a run bounded, scrolling inside itself
	// (box.go). nil = not boxed.
	box *boxState
	// maxH is the most rows this block has drawn under maxKey (its wrap width
	// and view state): it is never drawn shorter, because the conversation's
	// height never goes down while a session runs (noshrink_test.go). A width
	// or a state change — a resize, the person opening or closing it — starts
	// it over.
	maxH   int
	maxKey int
	// askText is the question of an ask_user row; askOpen while it waits for
	// its answer (askpark.go).
	askText string
	askOpen bool
	// provisional marks a message that was typed mid-turn and is queued for
	// delivery. It appears in the conversation with a dimmed style so the user
	// knows it landed but hasn't been delivered to the model yet. When the turn
	// ends and the messages are delivered, they lose the provisional marker and
	// render as normal user messages.
	provisional     bool
	provisionalText string // original text, used to restore normal style on delivery
	userText        string // raw user message text (non-empty for user messages only)
	// stats is the cost of the turn this message started (turnstats.go):
	// shown under it when it is opened.
	stats      string
	needLogin  bool   // the "not logged in" line (onboarding.go): said once, where it was last needed
	adding     string // the server an "adding …" line is for: the result replaces it (noteServerMsg)
	splash     bool   // the splash or a /status block (splash.go): client-only, never in the session log
	liveSplash bool   // the splash at the top: drawn again from live state at every refresh
	startKept  bool   // a message typed before ready, shown until it is sent (startqueue.go)
	// hang: a line that wraps continues at column 2, under its text, not
	// under its mark (⚠ …, the kept-question line).
	hang bool

	// blank marks the empty row turnGap() appends when a turn ends, so
	// consecutive turns read as blocks. It renders as nothing.
	blank bool

	// mdSource is the RAW markdown an assistant block was rendered from. The
	// rendered string bakes in the word-wrap width that was current when it was
	// produced, and a pre-wrapped line has no break opportunity left for
	// cellbuf to find on a narrower pane — so a resize down leaves over-width
	// lines that the viewport silently truncates. Keeping the source lets
	// refresh() re-render through the (rebuilt) markdown renderer when the
	// width changes, the way userText already does for user messages. Empty
	// for blocks that are not rendered markdown.
	mdSource string
	// wide is the answer's sideways scroll, when a code block or table in it
	// is wider than the pane (wide.go). nil until one is.
	wide *wideState

	// Wrapped render, cached. A finalized block's text never changes, but
	// refresh() runs once per STREAMED TOKEN, so re-wrapping the whole
	// scrollback every frame made the cost of a reply quadratic in the
	// conversation: measured at 7.8ms per frame with 200 blocks, ~1s of CPU for
	// one 100-token reply — on the goroutine that also reads the keyboard,
	// which is why keystrokes went missing. Invalidated by width (a resize) and
	// by state (expand/collapse/raw); docs blocks opt out, their inner per-doc
	// state is not captured here.
	wrapped   string
	wrapW     int
	wrapState viewState

	// wrapMax is the widest line of the UNWRAPPED block, measured once per
	// source rather than per width. Any terminal at least this wide needs no
	// wrapping at all, so a resize down to it costs nothing. That matters
	// because wrapping is what a width change actually pays for: on a
	// resume-sized conversation, measuring every block is 14ms and wrapping
	// every block is 73ms, and markdown that was rendered to fit the last
	// width usually still fits the next one.
	wrapMax   int
	wrapMaxOK bool

	// rowOffset is the viewport row (line index) where this entry starts.
	// Set by refresh() as a cumulative sum of block heights. Used by
	// scrollOverlay to map vp.YOffset back to conversation entries without
	// re-rendering.
	rowOffset int
	// lastH is the rows this entry drew at the last refresh (0 before the
	// first): with rowOffset, where it was in the view (foldAbove).
	lastH int
}

// invalidateWrap drops everything refresh() cached about how this entry
// renders. Call it wherever the entry's CONTENT changes — the width and state
// keys cannot see that, and a stale wrapMax would let a block skip the wrap it
// needs and overflow the pane.
func (e *convoEntry) invalidateWrap() {
	e.wrapped, e.wrapW = "", 0
	e.wrapMax, e.wrapMaxOK = 0, false
}
func (e convoEntry) expandable() bool { return e.full != "" || e.docs != nil || e.stats != "" }

// deeper is the next view state that shows something NEW, for →: expanded,
// when the block has a full form. Next() wraps, which is right for enter (a
// cycle) and wrong for → (a direction): → must not close what it opened.
func (e convoEntry) deeper() (viewState, bool) {
	if e.state == viewMinimized && (e.full != "" || e.docs != nil || e.stats != "") {
		return viewExpanded, true
	}
	return e.state, false
}

// shallower is the inverse, for ←: one level out, stopping at minimized.
func (e convoEntry) shallower() (viewState, bool) {
	if e.state == viewExpanded {
		return viewMinimized, true
	}
	return e.state, false
}
func (e convoEntry) view() string {
	if e.docs != nil {
		return e.docs.render(e.state > viewMinimized)
	}
	if e.state == viewExpanded && e.full != "" {
		return e.full
	}
	if e.state == viewExpanded && e.stats != "" {
		return e.collapsed + "\n" + e.stats
	}
	return e.collapsed
}

// docNode is one surfaced document inside a docsBlock; open shows its snippet.
type docNode struct {
	title, line string
	score       float64
	open        bool
	// source and why come from the engine ("" from an older one): where the
	// document came from, and why it won.
	source, why string
	kind        string
}

// docsBlock is a collapsed proactive-RAG summary ("N relevant · M surfaced").
// Expanding (enter) reveals the surfaced docs; → descends into the list, where
// ↑/↓ move between docs, enter expands a doc's snippet, ← / esc ascends.
type docsBlock struct {
	found, surfaced int
	tokens          int // estimated size of the appended entry; 0 from an older engine
	docs            []docNode
	descended       bool // focus is inside the doc list
	cur             int  // selected doc when descended
}

// docsBriefMax is the most the collapsed row adds for the documents; a
// longer list is left to the expanded block.
const docsBriefMax = 60

// brief names the surfaced documents on the collapsed row, by kind, each
// lone candidate marked: " · skill docs (only candidate), memory stash";
// by kind alone when the titles do not fit (" · skill, memory · 1 only
// candidate"). "" when neither fits, or from an older engine that sends no
// kind.
func (d *docsBlock) brief() string {
	var long, kinds []string
	lone := 0
	for _, doc := range d.docs {
		if doc.kind == "" {
			return ""
		}
		p := doc.title
		if !strings.HasPrefix(p, doc.kind+" ") {
			p = doc.kind + " " + p
		}
		if strings.HasPrefix(doc.why, "only candidate") {
			p += " (only candidate)"
			lone++
		}
		long, kinds = append(long, p), append(kinds, doc.kind)
	}
	short := strings.Join(kinds, ", ")
	if lone > 0 {
		short += fmt.Sprintf(" · %d only candidate", lone)
	}
	for _, s := range []string{strings.Join(long, ", "), short} {
		if s != "" && len([]rune(s)) <= docsBriefMax {
			return " · " + s
		}
	}
	return ""
}
func (d *docsBlock) render(open bool) string {
	glyph := "▸ "
	if open {
		glyph = "▾ "
	}
	label := fmt.Sprintf("%s🔎 %d relevant doc(s) · %d surfaced", glyph, d.found, d.surfaced)
	if d.tokens > 0 {
		label += numfmt.Commaf(" · ~%d tok", d.tokens)
	}
	if !open {
		label += d.brief()
	}
	head := stNote.Render(label)
	if !open || len(d.docs) == 0 {
		return head
	}
	lines := []string{head}
	for i, doc := range d.docs {
		cursor := "  "
		title := doc.title
		if d.descended && i == d.cur {
			cursor = stSel.Render("➤ ")
			title = stSel.Render(title)
		}
		dg := "▸"
		if doc.open {
			dg = "▾"
		}
		why := doc.why
		if why == "" {
			why = fmt.Sprintf("score %.2g", doc.score)
		}
		lines = append(lines, fmt.Sprintf("   %s%s %s  %s", cursor, dg, title, stDim.Render("("+why+")")))
		if doc.source != "" {
			lines = append(lines, stDim.Render("        "+doc.source))
		}
		if doc.open && doc.line != "" {
			lines = append(lines, stDim.Render("        "+doc.line))
		}
	}
	return strings.Join(lines, "\n")
}
