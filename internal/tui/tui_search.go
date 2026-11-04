package tui

import (
	"regexp"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// updateSearch drives the "/" query box: keys type into it (matches recompute
// live and the selection jumps to the first hit), enter commits to match-scroll
// mode (↑/↓ step between hits), esc cancels.
func (m tuiModel) updateSearch(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.searching, m.searchActive, m.matches = false, false, nil
		m.search.Blur()
		m.refresh()
		return m, nil
	case "enter":
		m.searching = false
		m.search.Blur()
		m.matches = m.computeMatches()
		if len(m.matches) > 0 {
			m.searchActive = true
			m.matchPos = 0
			m.sel = m.matches[0]
		} else {
			m.searchActive = false
		}
		m.refresh()
		return m, nil
	default:
		return m.typeSearch(msg)
	}
}

// typeSearch is a key or a paste in the "/" query box.
func (m tuiModel) typeSearch(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	cmd = editField(&m.search, "search", msg)
	m.matches = m.computeMatches() // live: preview the first hit as you type
	if len(m.matches) > 0 {
		m.matchPos = 0
		m.sel = m.matches[0]
	}
	m.refresh()
	return m, cmd
}

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func stripANSI(s string) string { return ansiRe.ReplaceAllString(s, "") }

// computeMatches returns the convo indices whose text (collapsed AND full, so
// hidden tool output is searchable) contains the query, case-insensitively.
func (m tuiModel) computeMatches() []int {
	q := strings.ToLower(strings.TrimSpace(m.search.Value()))
	if q == "" {
		return nil
	}
	var out []int
	for i, e := range m.convo {
		hay := strings.ToLower(stripANSI(e.collapsed + "\n" + e.full))
		if strings.Contains(hay, q) {
			out = append(out, i)
		}
	}
	return out
}
