package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
)

// /context and /usage: one screen (panes.go) with three tabs, in place of
// the conversation, so the numbers take no room in it (USER 2026-11-04:
// "a full screen modal that doesn't take up space in the conversation").
// /context opens on Context, /usage on Usage; tab and shift+tab (or 1 2 3,
// ← →) move between them, esc closes. It redraws as usage events arrive,
// so it can stay open through a turn.
//
//	Context   the window, the system prompt's parts, the conversation,
//	          compaction and recaps, tool results, out-of-band messages
//	Usage     this session's tokens, side calls, surfacing, cache, and cost
//	          & limits (the key's budget, rate limits)
//	Spend     this machine's spend, every session, from the ledger

const (
	statsContext = iota
	statsUsage
	statsSpend
)

var statsTabs = []string{"Context", "Usage", "Spend"}

// statsView is the screen's text for each tab, as of the last fill.
type statsView struct {
	tab  int
	text [3]string
}

// fill takes the tabs' text from m's stats.
func (v *statsView) fill(m *tuiModel) {
	v.text[statsContext] = m.contextText()
	v.text[statsUsage] = m.usageText()
	v.text[statsSpend] = ledgerBlock(&m.ctxStats)
}

func (v *statsView) page() pageDoc {
	return pageDoc{title: "/" + strings.ToLower(statsTabs[v.tab]), nodes: []pageNode{{body: v.text[v.tab]}}}
}

// lines is the status pane: the tabs, the open one marked.
func (v *statsView) lines(_ *tuiModel, w int) []string {
	parts := make([]string, len(statsTabs))
	for i, t := range statsTabs {
		label := hotKey(t, t[:1])
		if i == v.tab {
			label = stSel.Render("▸ " + t)
		} else {
			label = "  " + label
		}
		parts[i] = label
	}
	return wrapToRows(strings.Join(parts, "   "), w)
}

// openStats opens the screen on tab, or moves an open one there.
func (m *tuiModel) openStats(tab int) {
	if v := m.statsPaging(); v != nil {
		v.tab = tab
		m.pg.off = 0
		m.syncPager()
		return
	}
	v := &statsView{tab: tab}
	v.fill(m)
	m.pg = newPager(-1, v)
	m.paging = true
	m.scr = statsScreen()
	m.syncPager()
}

// statsPaging is the screen's view, if it is up.
func (m *tuiModel) statsPaging() *statsView {
	if !m.paging {
		return nil
	}
	v, _ := m.pg.id.(*statsView)
	return v
}

// syncStats redraws an open screen from the stats: an event changed them.
func (m *tuiModel) syncStats() {
	if v := m.statsPaging(); v != nil {
		v.fill(m)
		m.syncPager()
	}
}

func statsScreen() *paneScreen {
	return &paneScreen{
		panes: []screenPane{{body: viewStatusPane{}, fixed: true}, {body: statsPane{}, weight: 1, min: 3}},
		focus: 1,
		valid: func(m *tuiModel) bool { return m.statsPaging() != nil },
	}
}

// statsPane is the pager showing the open tab; it takes tab to change tabs.
type statsPane struct{}

func (statsPane) takesTab() bool { return true }

func (statsPane) view(m *tuiModel, w, h int) string { return m.pg.view(w, h) }

func (statsPane) key(m *tuiModel, msg tea.KeyPressMsg) bool {
	v := m.statsPaging()
	if v == nil {
		return m.pg.key(msg.String())
	}
	n := len(statsTabs)
	to := -1
	switch k := msg.String(); k {
	case "tab", "right", "l":
		to = (v.tab + 1) % n
	case "shift+tab", "left", "h":
		to = (v.tab + n - 1) % n
	case "1", "2", "3":
		to = int(k[0] - '1')
	case "c", "u", "s":
		to = strings.Index("cus", k)
	default:
		return m.pg.key(k)
	}
	m.openStats(to)
	return true
}

func (statsPane) hint(*tuiModel) string {
	return "tab/←→ tab · ↑↓ scroll · esc close"
}
