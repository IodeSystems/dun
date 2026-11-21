package tui

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/iodesystems/dun/internal/dunconfig"
	"github.com/iodesystems/dun/internal/dundcli"
	"github.com/iodesystems/dun/internal/procps"
)

// /ps in the TUI: a screen (panes.go) with what `dun ps` shows: every
// process dun runs, by session, with CPU, memory and IO. Read-only; kill,
// stop and open are in `dun ps --watch` (internal/psview, which this package
// cannot import: psview imports tui for its styles).
//
// The reading is dundcli.PSSampler's, taken in a command, never in Update.
// While the screen is open a reading is followed by a tick, then the next
// reading; a reading or tick that arrives after the screen closed (or for
// an earlier opening: gen) ends the chain.
//
// The order is frozen once the selection moves (procps.Order): values update
// in place, a new process goes to the end of its session, an exited one
// drops, and the selection stays on its process. t sorts again.

// psEvery is the refresh interval, as `dun ps --watch`'s default.
const psEvery = 2 * time.Second

type (
	// psMsg is a reading for opening gen.
	psMsg struct {
		gen int
		v   dundcli.PSView
		err error
	}
	// psTickMsg says the interval for opening gen is up.
	psTickMsg struct{ gen int }
)

type psView struct {
	gen    int
	sample func() (dundcli.PSView, error)
	v      dundcli.PSView
	have   bool
	err    error
	order  *procps.Order // non-nil: frozen
	width  int
	doc    pageDoc
	pids   []int // the node's process (0: none), parallel to doc.nodes
}

func (v *psView) page() pageDoc { return v.doc }

func (v *psView) setWidth(w int) {
	if w != v.width {
		v.width = w
		v.build()
	}
}

// build lays the reading out as nodes: a process is a head with its roots
// and notes as its body; a total is a head.
func (v *psView) build() {
	v.pids = v.pids[:0]
	if !v.have {
		text := "Reading the process table…"
		if v.err != nil {
			text = "Cannot read the process table: " + v.err.Error()
		}
		v.doc = pageDoc{title: "/ps", nodes: []pageNode{{body: text}}}
		return
	}
	gs := v.v.Groups
	if v.order != nil {
		gs = v.order.Apply(gs)
	}
	head, blocks := procps.TableBlocks(gs, max(20, v.width-3))
	doc := pageDoc{title: "/ps", header: head}
	doc.header = strings.TrimRight(head, "\n")
	for _, b := range blocks {
		if b.PID == 0 && b.Group == "" { // the blank line between sessions
			continue
		}
		lines := strings.Split(strings.TrimRight(b.Text, "\n"), "\n")
		n := pageNode{head: lines[0], pre: true}
		if len(lines) > 1 { // continuation lines sit under the head, one column left
			for i := 1; i < len(lines); i++ {
				lines[i] = strings.TrimPrefix(lines[i], " ")
			}
			rest := lines[1:]
			n.body = strings.Join(rest, "\n")
			n.rows = func(int) []string { return rest }
		}
		doc.nodes = append(doc.nodes, n)
		v.pids = append(v.pids, b.PID)
	}
	if len(doc.nodes) == 0 {
		doc.nodes = []pageNode{{body: "No processes."}}
		v.pids = append(v.pids, 0)
	}
	v.doc = doc
}

func (v *psView) lines(_ *tuiModel, w int) []string {
	s := stHeader.Render("/ps")
	if v.have {
		s += StDim.Render(" · ") + strings.ReplaceAll(v.v.Footer(), "\n", StDim.Render(" · "))
	}
	if v.err != nil && v.have {
		s += StDim.Render(" · ") + StErr.Render("reading failed: "+v.err.Error())
	}
	if v.order != nil {
		s += StDim.Render(" · ") + "sort paused"
	}
	return wrapToRows(s, w)
}

func (v *psView) hint() string {
	h := "↑↓ move"
	if v.order != nil {
		h += StDim.Render(" · ") + hotKey("re-sort", "t")
	}
	return h + StDim.Render(" · esc back")
}

// psSlash opens /ps. The first reading takes a second (CPU and IO are
// rates), so the screen opens saying so.
func psSlash(m *tuiModel, _ []string) tea.Cmd {
	sample := m.psSample
	if sample == nil {
		sample = dundcli.NewPSSampler(dunconfig.DunHome()).Sample
	}
	m.psGen++
	v := &psView{gen: m.psGen, sample: sample, width: m.vp.Width}
	v.build()
	m.pg = newPager(-1, v)
	m.paging = true
	m.scr = psScreen()
	m.syncPager()
	return psRead(v)
}

// psPaging is the /ps view the pager shows, if it shows one.
func (m *tuiModel) psPaging() *psView {
	if !m.paging {
		return nil
	}
	v, _ := m.pg.id.(*psView)
	return v
}

// psRead takes a reading off the UI goroutine.
func psRead(v *psView) tea.Cmd {
	gen, sample := v.gen, v.sample
	return func() tea.Msg {
		pv, err := sample()
		return psMsg{gen: gen, v: pv, err: err}
	}
}

// notePS takes a reading: the open screen shows it and schedules the next;
// a reading for a screen no longer open ends the chain.
func (m tuiModel) notePS(msg psMsg) (tuiModel, tea.Cmd) {
	v := m.psPaging()
	if v == nil || v.gen != msg.gen {
		return m, nil
	}
	if msg.err != nil {
		v.err = msg.err // the last good reading stays
	} else {
		v.v, v.have, v.err = msg.v, true, nil
		if v.order != nil {
			v.order.Extend(v.v.Groups)
		}
	}
	m.psRebuild(v)
	m.syncPager()
	gen := v.gen
	return m, tea.Tick(psEvery, func(time.Time) tea.Msg { return psTickMsg{gen: gen} })
}

// psRebuild lays v out again, the bodies open and the selection on the
// process it was on, not the row.
func (m *tuiModel) psRebuild(v *psView) {
	var sel int
	if m.pg.cur >= 0 && m.pg.cur < len(v.pids) {
		sel = v.pids[m.pg.cur]
	}
	v.build()
	if v.have {
		m.pg.open = map[int]bool{}
		for i, n := range v.doc.nodes {
			if n.body != "" {
				m.pg.open[i] = true
			}
		}
		if sel != 0 {
			for i, p := range v.pids {
				if p == sel {
					m.pg.cur = i
				}
			}
		}
	}
}

// notePSTick starts the next reading, if the screen it was for is open.
func (m tuiModel) notePSTick(msg psTickMsg) (tuiModel, tea.Cmd) {
	if v := m.psPaging(); v != nil && v.gen == msg.gen {
		return m, psRead(v)
	}
	return m, nil
}

func psScreen() *paneScreen {
	return &paneScreen{
		panes: []screenPane{{body: viewStatusPane{}, fixed: true}, {name: "processes", body: psListPane{}, weight: 3, min: 4}},
		focus: 1,
		valid: func(m *tuiModel) bool { return m.psPaging() != nil },
	}
}

type psListPane struct{}

func (psListPane) view(m *tuiModel, w, h int) string { return m.pg.view(w, h) }

func (psListPane) key(m *tuiModel, msg tea.KeyPressMsg) bool {
	v := m.psPaging()
	if v == nil {
		return m.pg.key(msg.String())
	}
	switch k := msg.String(); k {
	case "up", "down", "k", "j", "pgup", "pgdown", "home", "end", "g", "G":
		if v.order == nil && v.have {
			v.order = procps.FreezeOrder(v.v.Groups)
		}
	case "t":
		if v.order != nil {
			v.order = nil
			m.psRebuild(v)
			m.syncPager()
		}
		return true
	case "enter", "space", "right", "l", "left", "h":
		return true // the rows are open already
	}
	return m.pg.key(msg.String())
}

func (psListPane) hint(m *tuiModel) string {
	if v := m.psPaging(); v != nil {
		return v.hint()
	}
	return ""
}
