package tui

import (
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/iodesystems/dun/internal/textfmt"
)

// /journal in the TUI: a screen (panes.go) with a status pane over the
// entries, newest first: time, kind (fold, now, worklog), title, tokens.
// enter opens one. The engine sends them (control "journal" "screen", a
// journal event) and again after a /journal now; other clients get
// /journal's text listing and /journal view <n> (dun/journal.go).
//
//	n      /journal now: the model writes an entry for what happened since
//	       the last one
//	enter  the entry's text

type journalItem struct {
	n                int
	kind, line, text string
	path             string
}

type journalView struct {
	items []journalItem // oldest first, as the engine sends them
}

func (v *journalView) lines(_ *tuiModel, w int) []string {
	return wrapToRows(stHeader.Render("/journal")+StDim.Render(" · ")+textfmt.Plural(len(v.items), "entry"), w)
}

func (v *journalView) page() pageDoc {
	nodes := make([]pageNode, 0, len(v.items))
	for i := len(v.items) - 1; i >= 0; i-- {
		it := v.items[i]
		body := it.text
		if it.path != "" {
			body += "\n\n" + it.path
		}
		nodes = append(nodes, pageNode{head: it.line, body: body, pre: true})
	}
	if len(nodes) == 0 {
		nodes = append(nodes, pageNode{body: "The journal is empty. Each fold adds an entry, /worklog write adds its account, and n (/journal now) writes one."})
	}
	return pageDoc{title: textfmt.Plural(len(v.items), "entry"), nodes: nodes}
}

func (v *journalView) hint() string {
	return hotKey("now", "n") + StDim.Render(" an entry for what happened since the last · enter open · esc back")
}

// openJournal asks the engine for the entries; the screen opens when they
// arrive.
func (m *tuiModel) openJournal() {
	m.journalWant = true
	if !m.proc.controlCmd("journal", "screen") {
		m.journalWant = false
		m.append(StErr.Render("no engine right now — /reconnect first"))
	}
}

// journalPaging is the /journal view the pager shows, if it shows one.
func (m *tuiModel) journalPaging() *journalView {
	if !m.paging {
		return nil
	}
	v, _ := m.pg.id.(*journalView)
	return v
}

// noteJournal takes a journal event: the one this TUI asked for opens the
// screen; an open screen refreshes.
func (m tuiModel) noteJournal(ev evMsg) tuiModel {
	v := &journalView{}
	es, _ := ev["entries"].([]any)
	for _, x := range es {
		o, _ := x.(map[string]any)
		v.items = append(v.items, journalItem{n: intOf(o["n"]), kind: str(o["kind"]), line: str(o["line"]),
			text: str(o["text"]), path: str(o["path"])})
	}
	if cur := m.journalPaging(); cur != nil {
		grew := len(v.items) - len(cur.items)
		*cur = *v
		if grew > 0 { // newest first: the selection stays on its entry
			m.pg.cur += grew
			open := map[int]bool{}
			for i, o := range m.pg.open {
				open[i+grew] = o
			}
			m.pg.open = open
		}
		m.syncPager()
		return m
	}
	if !m.journalWant {
		return m
	}
	m.journalWant = false
	m.pg = newPager(-1, v)
	m.paging = true
	m.scr = journalScreen()
	m.syncPager()
	return m
}

// journalScreen is /journal: its status over the entries.
func journalScreen() *paneScreen {
	return &paneScreen{
		panes: []screenPane{{body: viewStatusPane{}, fixed: true}, {name: "journal", body: journalListPane{}, weight: 3, min: 4}},
		focus: 1,
		valid: func(m *tuiModel) bool { return m.journalPaging() != nil },
	}
}

// journalListPane is the pager showing the entries: n is the journal's, the
// rest are the pager's.
type journalListPane struct{}

func (journalListPane) view(m *tuiModel, w, h int) string { return m.pg.view(w, h) }

func (journalListPane) key(m *tuiModel, msg tea.KeyPressMsg) bool {
	if k := msg.String(); k == "n" && m.journalPaging() != nil {
		if !m.proc.controlCmd("journal", "now") {
			m.flashText, m.flashUntil = "no engine right now — /reconnect first", time.Now().Add(flashFor)
			return true
		}
		m.flashText, m.flashUntil = "/journal now: writing an entry…", time.Now().Add(flashFor)
		return true
	}
	return m.pg.key(msg.String())
}

func (journalListPane) hint(m *tuiModel) string {
	if v := m.journalPaging(); v != nil {
		return v.hint()
	}
	return ""
}
