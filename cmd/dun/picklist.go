package main

import (
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
)

// pickList is the one type-to-filter list (DES-10): the model picker and the
// login's service picker. Typing goes to the search box and narrows the
// list — every word of the query, in any case, somewhere in a row's text;
// ↑/↓ (ctrl+p/ctrl+n) and pgup/pgdown move; the owner says what enter and
// esc do. Its rows keep their height while the query narrows them
// (pickWindow, padPick): the search line and the divider do not move as a
// person types.
type pickList struct {
	query textinput.Model
	sel   int // the highlighted row of the filtered list
}

func newPickList(query, hint string) pickList {
	q := textinput.New()
	q.Placeholder = placeholder(hint)
	q.SetWidth(40) // without a width the placeholder shows as its first letter
	q.Prompt = "/ "
	q.SetValue(query)
	q.CursorEnd()
	q.Focus()
	return pickList{query: q}
}

// words is the query, lower-cased, split at spaces.
func (p pickList) words() []string { return strings.Fields(strings.ToLower(p.query.Value())) }

// matchWords reports whether every word is in hay (any case).
func matchWords(words []string, hay string) bool {
	hay = strings.ToLower(hay)
	for _, w := range words {
		if !strings.Contains(hay, w) {
			return false
		}
	}
	return true
}

// move handles a moving key over a filtered list of n rows; false for a
// key that does not move.
func (p *pickList) move(key string, n, window int) bool {
	switch key {
	case "up", "ctrl+p":
		p.sel = max(0, p.sel-1)
	case "down", "ctrl+n":
		p.sel = max(0, min(n-1, p.sel+1))
	case "pgup":
		p.sel = max(0, p.sel-window)
	case "pgdown":
		p.sel = max(0, min(n-1, p.sel+window))
	default:
		return false
	}
	return true
}

// typeKey is a key or a paste in the search box; changed reports a new
// query (the owner puts the cursor back on the first match).
func (p *pickList) typeKey(msg tea.Msg) (cmd tea.Cmd, changed bool) {
	before := p.query.Value()
	p.query, cmd = p.query.Update(msg)
	return cmd, p.query.Value() != before
}

// span is the part of a filtered list of n rows that shows in window rows,
// with the cursor in it.
func (p pickList) span(n, window int) (lo, hi int) {
	lo = max(0, p.sel-window/2)
	hi = min(n, lo+window)
	lo = max(0, hi-window)
	return lo, hi
}

// pickWindow is how many rows a list shows in a terminal h rows high: ten,
// fewer on a short screen, so the conversation keeps some rows.
func pickWindow(h int) int {
	if h <= 0 {
		return 10
	}
	return max(4, min(10, h/2-3))
}

// padPick fills a list's rows with blank ones to its height: the height of
// the whole list (all rows, unfiltered) in its window, so narrowing it
// leaves blank rows instead of pulling the rows below up.
func padPick(rows []string, total, window int) []string {
	for len(rows) < min(total, window) {
		rows = append(rows, "")
	}
	return rows
}
