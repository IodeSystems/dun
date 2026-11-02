package tui

import (
	"encoding/json"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/iodesystems/dun"
	"github.com/iodesystems/dun/internal/numfmt"
)

// ── helpers ────────────────────────────────────────────────────────

func (m *tuiModel) append(line string) {
	m.convo = append(m.convo, convoEntry{collapsed: line})
	m.refresh()
}

// selSkippable reports the entry at i as one the message selection must never
// land on — a blank turn-gap row has nothing to select.
func (m *tuiModel) selSkippable(i int) bool {
	return i >= 0 && i < len(m.convo) && m.convo[i].blank
}

// selSkipDown / selSkipUp step from i to the nearest non-skippable entry in
// the given direction, clamping at the edge. The selection never rests on a
// blank turn-gap row.
func (m *tuiModel) selSkipDown(i int) int {
	for i < len(m.convo) && m.selSkippable(i) {
		i++
	}
	if i >= len(m.convo) {
		i = len(m.convo) - 1
	}
	if i < 0 {
		i = 0
	}
	return i
}
func (m *tuiModel) selSkipUp(i int) int {
	for i >= 0 && m.selSkippable(i) {
		i--
	}
	if i < 0 {
		i = len(m.convo) - 1
	}
	if i >= len(m.convo) {
		i = len(m.convo) - 1
	}
	return i
}

// turnGap appends the blank row that separates one turn's output from the
// next, so the conversation reads in blocks. Called when the agent goes idle.
func (m *tuiModel) turnGap() {
	// An ask answered without a done in between would otherwise stack two
	// blank rows; the second is only the first one over.
	if n := len(m.convo); n > 0 && m.convo[n-1].blank {
		return
	}
	m.convo = append(m.convo, convoEntry{collapsed: "", blank: true})
}

// showSavedConversation puts the session being resumed on screen from its
// transcript on disk, read-only, while the session service cannot be reached.
// When dund answers, its replay of the same conversation is skipped.
func (m *tuiModel) showSavedConversation() {
	m.offlineShown = true
	id := m.opts.Resume
	if id == "" && m.opts.Cont {
		id = dun.LatestSession(m.opts.Workspace)
	}
	if id == "" {
		return
	}
	hist := dun.SessionHistory(dun.SessionFile(m.opts.Workspace, id))
	if len(hist) == 0 {
		return
	}
	b, err := json.Marshal(hist)
	if err != nil {
		return
	}
	var items []any
	if json.Unmarshal(b, &items) != nil {
		return
	}
	m.replay(items)
	m.append(StDim.Render("── " + id + ", read-only from disk: the session service (dund) cannot be reached — retrying ──"))
	m.skipHistory = true
	m.refresh()
}

// exit is /exit: tell dund this client is leaving — it stops the engine
// unless the session persists or another client is attached — then quit.
// ctrl+c only detaches.
func (m *tuiModel) exit() tea.Cmd {
	m.proc.leave()
	m.quitting = true
	return tea.Quit
}

// appendUser adds a user message and stores the raw text for full-width re-rendering.
func (m *tuiModel) appendUser(text string) {
	m.convo = append(m.convo, convoEntry{collapsed: userRow(text), userText: text})
	m.refresh()
}
func (m *tuiModel) flushCur() {
	// A reply of only whitespace is no reply. Reasoning models open with
	// "\n\n" after their thinking, and a round that then calls a tool leaves
	// nothing else — finalizing it would leave a blank row in the log.
	if replyShown(m.cur) {
		src := strings.TrimRight(m.cur, "\n")
		// Finalize the streamed reply as rendered markdown (headers, lists, code).
		// The final render can come out shorter than a partial one did (an
		// unclosed fence, a list mid-item): it keeps the streamed height.
		m.convo = append(m.convo, convoEntry{collapsed: renderMarkdown(m.md, src), mdSource: src,
			maxH: m.curMaxH, maxKey: m.curMaxKey})
	}
	m.cur, m.curMaxH, m.curMaxKey = "", 0, 0
}

// heightKey is what a block's held height is valid for: one wrap width, one
// view state.
func heightKey(w int, st viewState) int { return w*4 + int(st) + 1 }

// holdHeight pads rendered block w to the most rows it has had under key,
// and records a new most.
func holdHeight(w string, maxH, maxKey *int, key int) string {
	if *maxKey != key {
		*maxH, *maxKey = 0, key
	}
	n := strings.Count(w, "\n") + 1
	if n < *maxH {
		return w + strings.Repeat("\n", *maxH-n)
	}
	*maxH = n
	return w
}

// replyShown says whether the streaming reply has anything to draw. Until it
// does it takes no row: a block that renders to nothing is still one row,
// and it sat between the message just sent and whatever came next.
func replyShown(cur string) bool { return strings.TrimSpace(cur) != "" }

// replay rebuilds scrollback from a `history` event (a resumed session), reusing
// the same rendering as live events: user echo, assistant markdown, folded tool
// call/result blocks, notifications. No turn runs — this is pure display. A
// trailing dim marker delimits the resumed history from new activity.
func (m *tuiModel) replay(items []any) {
	n := m.replayItems(items, -1)
	m.endRun()
	// A model pick restarts the engine on the same conversation: nothing was
	// resumed as far as the person is concerned, so there is no marker.
	if n > 0 && !m.quietReplay {
		m.convo = append(m.convo, convoEntry{collapsed: StDim.Render(numfmt.Commaf("── resumed %d entries ──", n))})
	}
	m.quietReplay = false
	// History was never on screen: its thoughts come back folded
	// (scrollfold.go). Not in replayLive: a watched child's new entries are.
	for i := range m.convo {
		if r := m.convo[i].run; r != nil {
			for _, it := range r.items {
				if it.think != nil && !it.think.live {
					it.folded = true
				}
			}
			r.ver++
		}
	}
	m.refresh()
}

// replayLive appends a watched child's new entries (agent_entry) as they
// come: a call and its result arrive apart, so the open run is carried
// across events and left open for the next call.
func (m *tuiModel) replayLive(items []any) {
	m.replayItems(items, m.openRun())
	m.refresh()
}

// replayItems renders history items onto the conversation, the next
// tool_result going to run; it returns how many entries it added.
func (m *tuiModel) replayItems(items []any, run int) int {
	n := 0
	for _, it := range items {
		im, _ := it.(map[string]any)
		switch str(im["kind"]) {
		case "user":
			m.endRun()
			m.task = str(im["content"]) // the task line survives a resume
			m.convo = append(m.convo, convoEntry{collapsed: userRow(str(im["content"])), userText: str(im["content"])})
			n++
		case "assistant":
			m.endRun()
			src := strings.TrimRight(str(im["content"]), "\n")
			m.convo = append(m.convo, convoEntry{collapsed: renderMarkdown(m.md, src), mdSource: src})
			n++
		case "tool_call":
			// The same path as a live call: into the run's box (toolrun.go),
			// or a message's own (agentmsg.go).
			args, _ := im["args"].(map[string]any)
			if a := carriedMsg(str(im["tool"]), args); a != nil {
				run = m.addMsg(a)
				break
			}
			run = m.runAddCall(str(im["tool"]), args)
		case "tool_result":
			if run >= 0 && run < len(m.convo) && m.convo[run].msg != nil && !m.convo[run].msg.done {
				m.convo[run].msg.setResult(str(im["content"]))
				m.touchMsg(run)
				n++
				break
			}
			if run < 0 || run >= len(m.convo) || m.convo[run].run == nil {
				run = m.runAddCall(str(im["tool"]), nil) // a result with no call seen
			}
			m.runSetResult(run, str(im["tool"]), str(im["content"]))
			n++
		case "notification":
			m.endRun()
			if a := noticeMsg("", str(im["content"])); a != nil {
				m.addMsg(a)
				n++
				break
			}
			m.convo = append(m.convo, convoEntry{collapsed: noticeLine(str(im["content"]), m.noticeW())})
			n++
		case "reasoning":
			m.addThought(&thinkBlock{text: str(im["content"])})
		case "command":
			// A command the person ran and dun's answer (dun/usercommand.go):
			// the line dim, as typed, and the reply under it.
			m.endRun()
			line, reply, _ := strings.Cut(str(im["content"]), "\n")
			text := StDim.Render("› " + line)
			if reply != "" {
				text += "\n" + replyLine(reply)
			}
			m.convo = append(m.convo, convoEntry{collapsed: text})
			n++
		}
	}
	return n
}

// selDocs returns the docsBlock of the selected entry, or nil.
func (m *tuiModel) selDocs() *docsBlock {
	if m.sel >= 0 && m.sel < len(m.convo) {
		return m.convo[m.sel].docs
	}
	return nil
}

// descendSel opens the selected block one level further and reports whether →
// was consumed by the conversation. False means "this block has nothing left to
// open", which is the only case where → hands focus back to the input.
//
// The order is innermost-first, so → always acts on the deepest thing already
// open rather than on the block as a whole.
func (m *tuiModel) descendSel() bool {
	if m.sel < 0 || m.sel >= len(m.convo) {
		return false
	}
	e := &m.convo[m.sel]
	// A box: → descends into it, to scroll it (box.go). Inside, → stays.
	if e.boxed() {
		e.box.inside = true
		return true
	}
	// An answer with a block cut at the edge: → descends into it, to
	// scroll it sideways (wide.go).
	if e.wide != nil && e.wide.max > 0 {
		e.wide.inside = true
		return true
	}
	if d := e.docs; d != nil {
		if d.descended {
			// Inside the list: → opens the highlighted document. Already open,
			// it stays put — a nested focus does not leak out to the input, or ←
			// would no longer be the way back to where you came from.
			if d.cur >= 0 && d.cur < len(d.docs) && !d.docs[d.cur].open {
				d.docs[d.cur].open = true
			}
			return true
		}
		if e.state > viewMinimized && len(d.docs) > 0 {
			d.descended = true
			if d.cur < 0 || d.cur >= len(d.docs) {
				d.cur = 0
			}
			return true
		}
	}
	if next, ok := e.deeper(); ok {
		e.state = next
		return true
	}
	return false
}

// ascendSel closes one level and reports whether ← was consumed. False means
// the block is already shut, and ← falls through to moving between zones.
func (m *tuiModel) ascendSel() bool {
	if m.sel < 0 || m.sel >= len(m.convo) {
		return false
	}
	e := &m.convo[m.sel]
	if d := e.docs; d != nil && d.descended {
		if d.cur >= 0 && d.cur < len(d.docs) && d.docs[d.cur].open {
			d.docs[d.cur].open = false // close the document before leaving the list
			return true
		}
		d.descended = false
		return true
	}
	if prev, ok := e.shallower(); ok {
		e.state = prev
		if prev == viewMinimized && e.docs != nil {
			e.docs.descended = false
		}
		return true
	}
	return false
}

// selGeom returns the top line offset and rendered height of the selected block
// (from the last refresh), for tall-message intra-scroll decisions.
func (m tuiModel) selGeom() (top, h int) {
	bh := m.frame.blockH
	for i := 0; i < m.sel && i < len(bh); i++ {
		top += bh[i]
	}
	if m.sel >= 0 && m.sel < len(bh) {
		h = bh[m.sel]
	}
	return top, h
}
