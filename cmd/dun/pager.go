package main

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// The pager is the full-pane reader: one convo block, given the conversation
// pane's whole height. Enter on a block that has more to it than fits inline
// (a round's thinking, a folded run of tool calls) opens it here; esc comes
// back to the conversation with the selection where it was.
//
// It takes the conversation pane's place and nothing else: the header, the
// divider, the input and the status line stay, so a turn running underneath
// is still visible and the composer still works after esc.
//
// The pager does not copy the block. It keeps the block's identity and asks
// it for its page on every update (syncPager), so a block that is still
// growing — thinking that is still streaming, a run that gets another call —
// grows in here too. A reader at the bottom stays at the bottom as it grows; one who
// scrolled up stays where they are.
//
// A page is a list of nodes. One headless node is plain text: ↑/↓ scroll it.
// Several nodes, or nodes with heads, are a tree: ↑/↓ move between the heads,
// enter or → opens a node's body under it, ← closes it.

// pageNode is one node of a page: a one-line head and the body it opens to.
// A headless node is always open — it is the text itself.
type pageNode struct {
	head string
	body string
	err  bool        // marks the head as a failure
	pre  bool        // the head's spacing is kept (a table row), not collapsed
	insp *inspSource // a call: "i" opens it in the inspector
}

// inspSource is what the inspector shows for a call.
type inspSource struct{ name, input, output string }

// curNode is the selected node, or nil.
func (p pager) curNode() *pageNode {
	if p.cur < 0 || p.cur >= len(p.doc.nodes) {
		return nil
	}
	return &p.doc.nodes[p.cur]
}

type pageDoc struct {
	title string
	// header is a row above the nodes, not selectable and drawn as given
	// (styled): a table's column heads.
	header string
	nodes  []pageNode
}

// pageID is the identity of a block with a page: the pointer its page is
// read from. Index alone is not identity — the conversation is replaced
// wholesale (/clear, a resume, an agent's view) under an open pager.
func (e convoEntry) pageID() any {
	switch {
	case e.think != nil:
		return e.think
	case e.run != nil:
		return e.run
	}
	return nil
}

// page is what a block shows in the pager, if it has a page at all.
func (e convoEntry) page() (pageDoc, bool) {
	switch {
	case e.think != nil:
		title := "✻ thinking…"
		if !e.think.live {
			title = "✻ thought"
			if !e.think.end.IsZero() {
				title += " for " + thinkDur(e.think.end.Sub(e.think.start))
			}
		}
		return pageDoc{title: title, nodes: []pageNode{{body: strings.TrimSpace(e.think.text)}}}, true
	case e.run != nil:
		return e.run.page(), true
	}
	return pageDoc{}, false
}

type pager struct {
	src     int          // the convo index of the block being read
	id      any          // its pageID, to tell it is still the same block
	doc     pageDoc      // its page, as of the last sync
	open    map[int]bool // tree nodes the reader opened
	cur     int          // the selected node (tree)
	off     int          // first body row shown
	follow  bool         // at the bottom: stay there as the page grows
	rows    []string     // the page laid out at w
	nodeAt  []int        // row where node i's head is
	w, h    int          // the pane: h includes the title row
	phase   int          // the selection band's pulse step (band.go)
	toggles bool         // space turns the selected node on or off (mcptools.go)
}

func newPager(src int, id any) pager {
	return pager{src: src, id: id, open: map[int]bool{}}
}

func (p pager) tree() bool {
	return len(p.doc.nodes) > 1 || (len(p.doc.nodes) == 1 && p.doc.nodes[0].head != "")
}

// bodyH is the rows under the title.
func (p pager) bodyH() int { return max(1, p.h-1) }

func (p pager) maxOff() int { return max(0, len(p.rows)-p.bodyH()) }

// layout lays the page out at w×h and re-clamps the offset.
func (p *pager) layout(doc pageDoc, w, h int) {
	p.doc, p.w, p.h = doc, max(1, w), max(2, h)
	p.cur = min(max(p.cur, 0), max(0, len(doc.nodes)-1))
	p.rows, p.nodeAt = p.rows[:0], p.nodeAt[:0]
	tree := p.tree()
	if doc.header != "" {
		p.rows = append(p.rows, "   "+doc.header) // under the heads, past the gutter and mark
	}
	for i, n := range doc.nodes {
		p.nodeAt = append(p.nodeAt, len(p.rows))
		if !tree {
			p.rows = append(p.rows, wrapRows(n.body, p.w)...)
			continue
		}
		gutter, mark := " ", "▸ "
		if i == p.cur {
			gutter = stSel.Render("▎")
		}
		if p.open[i] {
			mark = "▾ "
		}
		h := oneLine(n.head)
		if n.pre {
			h = strings.ReplaceAll(n.head, "\n", " ")
		}
		head, _ := fitPlain(h, p.w-3)
		st := stTool
		if n.err {
			st = stErr
		}
		r := gutter + stDim.Render(mark) + st.Render(head)
		if i == p.cur {
			r = band.row(r, p.w, p.phase)
		}
		p.rows = append(p.rows, r)
		if p.open[i] {
			for _, r := range wrapRows(n.body, p.w-4) {
				p.rows = append(p.rows, "    "+r)
			}
		}
	}
	if p.follow {
		p.off = p.maxOff()
	}
	p.off = min(max(p.off, 0), p.maxOff())
}

// wrapRows wraps text to w columns, as display rows.
func wrapRows(s string, w int) []string {
	if strings.TrimSpace(s) == "" {
		return []string{stDim.Render("(empty)")}
	}
	return strings.Split(lipgloss.NewStyle().Width(max(1, w)).Render(s), "\n")
}

func (p *pager) scroll(n int) {
	p.off = min(max(p.off+n, 0), p.maxOff())
	p.follow = p.off >= p.maxOff()
}

// showCur scrolls so the selected node is in view: its head always, and as
// much of an open body as fits under it.
func (p *pager) showCur() {
	if p.cur >= len(p.nodeAt) {
		return
	}
	top := p.nodeAt[p.cur]
	end := len(p.rows)
	if p.cur+1 < len(p.nodeAt) {
		end = p.nodeAt[p.cur+1]
	}
	switch {
	case top < p.off:
		p.off = top
	case end > p.off+p.bodyH():
		p.off = min(top, end-p.bodyH())
	}
	p.off = min(max(p.off, 0), p.maxOff())
	p.follow = p.off >= p.maxOff()
}

// key handles one key and reports whether the pager stays open.
func (p *pager) key(k string) bool {
	tree := p.tree()
	switch k {
	case "esc", "q":
		return false
	case "up", "k":
		if tree {
			p.cur = max(0, p.cur-1)
			p.layout(p.doc, p.w, p.h)
			p.showCur()
		} else {
			p.scroll(-1)
		}
	case "down", "j":
		if tree {
			p.cur = min(len(p.doc.nodes)-1, p.cur+1)
			p.layout(p.doc, p.w, p.h)
			p.showCur()
		} else {
			p.scroll(1)
		}
	case "enter", "space", "right", "l":
		if tree {
			open := !p.open[p.cur]
			if k == "right" || k == "l" {
				open = true
			}
			p.open[p.cur] = open
			p.layout(p.doc, p.w, p.h)
			p.showCur()
		}
	case "left", "h":
		if !tree {
			return false // one level out, and there is only the one
		}
		if p.open[p.cur] {
			p.open[p.cur] = false
			p.layout(p.doc, p.w, p.h)
			p.showCur()
		} else {
			return false
		}
	case "pgup":
		p.scroll(-p.bodyH())
	case "pgdown":
		p.scroll(p.bodyH())
	case "home", "g":
		p.scroll(-len(p.rows))
	case "end", "G":
		p.scroll(len(p.rows))
	}
	return true
}

// mouse scrolls on the wheel.
func (p *pager) mouse(msg tea.MouseMsg) {
	if _, ok := msg.(tea.MouseWheelMsg); !ok {
		return
	}
	switch msg.Mouse().Button {
	case tea.MouseWheelUp:
		p.scroll(-wheelDelta)
	case tea.MouseWheelDown:
		p.scroll(wheelDelta)
	}
}

// view draws the title row and the window of the page, exactly h rows of w.
func (p pager) view(w, h int) string {
	bodyH := max(1, h-1)
	off := min(max(p.off, 0), max(0, len(p.rows)-bodyH))
	pos := "all"
	if len(p.rows) > bodyH {
		pos = fmt.Sprintf("%d–%d of %d", off+1, min(off+bodyH, len(p.rows)), len(p.rows))
	}
	title, _ := fitPlain(p.doc.title, max(1, w-len(pos)-2))
	head := stHeader.Render(title) + stDim.Render("  "+pos)
	rows := append([]string{head}, p.rows[off:min(off+bodyH, len(p.rows))]...)
	return lipgloss.NewStyle().Width(w).MaxWidth(w).Height(h).MaxHeight(h).Render(strings.Join(rows, "\n"))
}

// hint is the status line while the pager is open.
func (p pager) hint() string {
	if p.toggles {
		return "type to filter · ↑/↓ select · enter/→ open · ← close · space on/off (session) · ctrl+g global · ctrl+p project · ctrl+s session · esc clear, back"
	}
	if p.tree() {
		return "↑/↓ select · enter/→ open · ← close · i inspect a call · pgup/pgdn scroll · esc back"
	}
	return "↑/↓ pgup/pgdn scroll · g/G ends · esc back"
}

// ── the model's side ───────────────────────────────────────────────

// openPager opens the selected block in the pager, if it has a page.
func (m *tuiModel) openPager(i int) bool {
	if i < 0 || i >= len(m.convo) {
		return false
	}
	if _, ok := m.convo[i].page(); !ok {
		return false
	}
	e := m.convo[i]
	m.pg = newPager(i, e.pageID())
	// Thinking that is still streaming opens at its newest line, following.
	m.pg.follow = e.think != nil && e.think.live
	m.paging = true
	m.syncPager()
	return true
}

// syncPager re-reads the block being paged and lays it out at the pane's
// size. A block that is gone (the conversation was cleared or replaced)
// closes the pager rather than show something else at that index.
func (m *tuiModel) syncPager() {
	if !m.paging {
		return
	}
	if v, ok := m.pg.id.(interface{ page() pageDoc }); ok { // not a convo block: its own page
		m.pg.phase = m.pulse
		m.pg.layout(v.page(), m.vp.Width, m.convoHeight())
		return
	}
	if m.pg.src < 0 || m.pg.src >= len(m.convo) || m.convo[m.pg.src].pageID() != m.pg.id {
		m.pg.src = -1
		for i := range m.convo {
			if m.convo[i].pageID() == m.pg.id {
				m.pg.src = i
				break
			}
		}
		if m.pg.src < 0 {
			m.paging = false
			return
		}
	}
	doc, ok := m.convo[m.pg.src].page()
	if !ok {
		m.paging = false
		return
	}
	m.pg.phase = m.pulse
	m.pg.layout(doc, m.vp.Width, m.convoHeight())
}
