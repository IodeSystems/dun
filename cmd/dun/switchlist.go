package main

import (
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// A kind of scoped switches in the pager (dun/switches.go: skills now,
// the rules files): a row per item with its cost and its switch at each
// scope, the kind's mode in the title. It is the /mcp tools view
// (mcptools.go) for any kind:
//
//	type        filter: every word in the name, the description or the source,
//	            name matches first; backspace takes a letter back
//	↑/↓ enter   select, open (the description in full and what decided;
//	            a skill opens in the reader below, skillpane.go)
//	space       on/off for this session
//	ctrl+g/p/s  cycle the global, project or session switch
//	tab         the kind's mode for this session (listed → surfaced → unset)
//	esc         clear the filter, then close
//
// Every key sends the command a person could type (/skill off pdf-* --global)
// through the engine (control "switches"), and the answer refreshes the rows
// in place. A bare /<kind> opens it; a complete /<kind> on|off|mode … just
// does it (docs/ui-decisions.md).

type switchRow struct {
	name, desc, source, detail string
	global, project, session   string // "on", "off", "" unset
	from                       string // the scope that decided on
	tokens                     int
	on                         bool
}

type switchListView struct {
	kind     string // "skills"
	slash    string // its command: "skill", "rules"
	rows     []switchRow
	mode     string
	modeFrom string
	modes    map[string]string // scope → its mode
	about    map[string]string // mode → what it means
	order    []string          // the modes, the default first
	note     string            // a warning, in the status pane
	status   []string          // the kind's facts (raglit on or off), in the status pane
	query    string
	width    int // the pane's; under narrowW a row is two lines
}

// narrowW is the width under which a list's rows take two lines: the name
// (and its mark) above, its switches and cost below (USER 2026-10-25: a
// phone's terminal).
const narrowW = 80

func (v *switchListView) setWidth(w int) { v.width = w }

// shown is the rows the filter keeps, as indexes into rows.
func (v *switchListView) shown() []int {
	words := strings.Fields(strings.ToLower(v.query))
	var byName, other []int
	for i, r := range v.rows {
		name, rest := strings.ToLower(r.name), strings.ToLower(r.desc+" "+r.source)
		inName, all := true, true
		for _, w := range words {
			n := strings.Contains(name, w)
			inName = inName && n
			all = all && (n || strings.Contains(rest, w))
		}
		switch {
		case inName:
			byName = append(byName, i)
		case all:
			other = append(other, i)
		}
	}
	return append(byName, other...)
}

func (v *switchListView) row(cur int) *switchRow {
	s := v.shown()
	if cur < 0 || cur >= len(s) {
		return nil
	}
	return &v.rows[s[cur]]
}

// lines is the status pane above the list: what it is, the mode, what the
// items that are on cost, the kind's facts, and a warning when there is one
// (USER 2026-10-25: a warning is not an item).
func (v *switchListView) lines(_ *tuiModel, w int) []string {
	on, cost := 0, 0
	for _, r := range v.rows {
		if r.on {
			on++
			cost += r.tokens
		}
	}
	mode := v.mode
	if v.modeFrom != "" {
		mode += " (" + v.modeFrom + ")"
	}
	if len(v.order) == 0 { // a kind with no modes (the indexes): what is on, and its facts
		out := wrapToRows(stHeader.Render("/"+v.slash)+stDim.Render(" · ")+fmt.Sprintf("%d of %d on", on, len(v.rows)), w)
		for _, s := range v.status {
			out = append(out, wrapToRows(stDim.Render(s), w)...)
		}
		if v.note != "" {
			out = append(out, wrapToRows(stNote.Render("⚠ "+v.note), w)...)
		}
		return out
	}
	// What the items that are on cost, in the kind's default mode (the
	// first): another mode sends less.
	cost0 := fmt.Sprintf("~%s tokens in every request", kTokens(cost))
	if len(v.order) > 0 && v.mode != v.order[0] {
		cost0 = fmt.Sprintf("~%s tokens in mode %s", kTokens(cost), v.order[0])
	}
	var out []string
	if w < narrowW { // two rows on a phone: what it is and what it costs, then its facts
		out = append(out, wrapToRows(stHeader.Render("/"+v.slash)+stDim.Render(" · ")+
			fmt.Sprintf("%d/%d on · %s · ~%s tok", on, len(v.rows), mode, kTokens(cost)), w)...)
	} else {
		out = append(out, wrapToRows(stHeader.Render("/"+v.slash)+stDim.Render(" · ")+fmt.Sprintf("%d of %d on · mode %s", on, len(v.rows), mode), w)...)
		out = append(out, wrapToRows(cost0, w)...)
		if about := v.about[v.mode]; about != "" {
			out = append(out, wrapToRows(stDim.Render(v.mode+": "+about), w)...)
		}
	}
	for _, s := range v.status {
		out = append(out, wrapToRows(stDim.Render(s), w)...)
	}
	if v.note != "" {
		out = append(out, wrapToRows(stNote.Render("⚠ "+v.note), w)...)
	}
	return out
}

func (v *switchListView) page() pageDoc {
	shown := v.shown()
	noun := strings.TrimSuffix(v.kind, "s") + "s"
	switch v.kind {
	case "rules":
		noun = "files"
	case "indexes":
		noun = "indexes"
	case "paths":
		noun = "paths"
	}
	title := fmt.Sprintf("%d %s", len(v.rows), noun)
	if v.query != "" {
		title = fmt.Sprintf("filter %q: %d of %d shown", v.query, len(shown), len(v.rows))
	}
	narrow := v.width > 0 && v.width < narrowW
	nodes := make([]pageNode, 0, len(shown)+1)
	for _, i := range shown {
		r := v.rows[i]
		mark := "○"
		if r.on {
			mark = "●"
		}
		first, _, _ := strings.Cut(strings.TrimSpace(r.desc), "\n")
		cells := fmt.Sprintf("g%s p%s s%s", cellMark(r.global), cellMark(r.project), cellMark(r.session))
		head := fmt.Sprintf("%s %s  ~%s tok  %s  %s  %s", mark, r.name, kTokens(r.tokens), cells, r.source, first)
		if len(v.order) == 0 { // no cost to show: the indexes
			head = fmt.Sprintf("%s %s  %s  %s  %s", mark, r.name, cells, r.source, first)
		}
		sub := ""
		if narrow {
			// The name above; below, what it costs, where it is from and
			// what it is. The switch is said only when one is set: an
			// unset row is on by default, and three dashes on every row
			// were noise (USER 2026-10-25, a phone).
			head = mark + " " + r.name
			sub = fmt.Sprintf("~%s tok · %s", kTokens(r.tokens), r.source)
			if len(v.order) == 0 {
				sub = r.source
			}
			if r.from != "" {
				sub = onOff(r.on) + " " + scopeIn(r.from) + " · " + sub
			}
			if first != "" {
				sub += " · " + first
			}
		}
		body := onOff(r.on)
		if r.from != "" {
			body += " (switched " + scopeIn(r.from) + ")"
		}
		if narrow { // each scope's switch, in words: the row's second line has only the one that decided
			body += "\nglobal " + cellWord(r.global) + " · project " + cellWord(r.project) + " · session " + cellWord(r.session)
		}
		if r.detail != "" {
			body += "\n" + r.detail
		}
		body += "\n\n" + strings.TrimSpace(r.desc)
		nodes = append(nodes, pageNode{head: head, sub: sub, body: body})
	}
	if v.query != "" && len(shown) == 0 {
		nodes = append(nodes, pageNode{head: fmt.Sprintf("nothing matches %q — backspace or esc", v.query)})
	}
	return pageDoc{title: title, nodes: nodes}
}

// hint is the status line while the view is open; narrow, the keys alone,
// short enough to fit a phone (fitHints would keep only the first and last).
func (v *switchListView) hint() string {
	if len(v.order) == 0 {
		if v.width > 0 && v.width < narrowW {
			return "␣ session · ^g ^p ^s scopes · esc"
		}
		return "type to search · space attach/detach (session) · ctrl+g/p/s global/project/session · enter details · esc clear, back"
	}
	if v.width > 0 && v.width < narrowW {
		return "␣ on/off · ^g ^p ^s scopes · tab mode · esc"
	}
	about := v.about[v.mode]
	if about != "" {
		about = " · " + v.mode + ": " + about
	}
	return "type to filter · space on/off (session) · ctrl+g/p/s global/project/session · tab mode · esc clear, back" + about
}

// scopeIn is "for this project".
func scopeIn(scope string) string {
	switch scope {
	case "global":
		return "for every project"
	case "project":
		return "for this project"
	case "session":
		return "for this session"
	}
	return scope
}

// openSwitches asks the engine for a kind's view; it opens when it arrives.
func (m *tuiModel) openSwitches(kind string) {
	m.switchesWant = kind
	if !m.proc.controlCmd("switches", kind) {
		m.switchesWant = ""
		m.append(stErr.Render("no engine right now — /reconnect first"))
	}
}

// switchesPaging is the switch list the pager shows, if it shows one.
func (m *tuiModel) switchesPaging() *switchListView {
	if !m.paging {
		return nil
	}
	v, _ := m.pg.id.(*switchListView)
	return v
}

// noteSwitches takes a switches event: a fresh view for the open list, the
// list this TUI asked for, or (nothing open) the answer as a line.
func (m tuiModel) noteSwitches(ev evMsg) tuiModel {
	kind, msg := str(ev["kind"]), strings.TrimSpace(str(ev["message"]))
	fresh := &switchListView{kind: kind, slash: str(ev["slash"]), mode: str(ev["mode"]), modeFrom: str(ev["modeFrom"]), note: str(ev["note"]),
		modes: strMap(ev["modes"]), about: strMap(ev["about"])}
	if fresh.slash == "" {
		fresh.slash = strings.TrimSuffix(kind, "s")
	}
	if st, ok := ev["status"].([]any); ok {
		for _, x := range st {
			fresh.status = append(fresh.status, str(x))
		}
	}
	if o, ok := ev["order"].([]any); ok {
		for _, x := range o {
			fresh.order = append(fresh.order, str(x))
		}
	}
	if items, ok := ev["items"].([]any); ok {
		for _, it := range items {
			o, _ := it.(map[string]any)
			on, _ := o["on"].(bool)
			fresh.rows = append(fresh.rows, switchRow{name: str(o["name"]), desc: str(o["description"]), source: str(o["source"]),
				detail: str(o["detail"]), tokens: int(evNum(o["tokens"])), on: on, from: str(o["from"]),
				global: str(o["global"]), project: str(o["project"]), session: str(o["session"])})
		}
	}
	v := m.switchesPaging()
	switch {
	case v != nil && v.kind == kind:
		q, w := v.query, v.width
		*v = *fresh
		v.query, v.width = q, w
		if msg != "" {
			m.flashText, m.flashUntil = msg, time.Now().Add(flashFor)
		}
		m.syncPager()
	case kind != "" && kind == m.switchesWant:
		m.switchesWant = ""
		if msg != "" {
			m.append(replyLine(msg))
		}
		m.pg = newPager(-1, fresh)
		m.paging = true
		m.scr = switchScreen() // the status pane over the list (panes.go)
		m.syncPager()
	case msg != "":
		m.append(replyLine(msg))
	}
	return m
}

func strMap(v any) map[string]string {
	o, _ := v.(map[string]any)
	out := map[string]string{}
	for k, x := range o {
		out[k] = str(x)
	}
	return out
}

// switchKey handles a key in the switch list; false: not its key.
func (m *tuiModel) switchKey(v *switchListView, k, text string) bool {
	send := func(op string) {
		if !m.proc.controlCmd("switches", v.kind+" "+op) {
			m.flashText, m.flashUntil = "no engine right now — /reconnect first", time.Now().Add(flashFor)
		}
	}
	switch k {
	case "space":
		if r := v.row(m.pg.cur); r != nil {
			send(map[bool]string{true: "off", false: "on"}[r.on] + " " + r.name + " --session")
		}
		return true
	case "ctrl+g", "ctrl+p", "ctrl+s":
		r := v.row(m.pg.cur)
		if r == nil {
			return true
		}
		switch k {
		case "ctrl+g":
			send(nextTri(r.global) + " " + r.name + " --global")
		case "ctrl+p":
			send(nextTri(r.project) + " " + r.name + " --project")
		case "ctrl+s":
			send(nextTri(r.session) + " " + r.name + " --session")
		}
		return true
	case "tab": // the session's mode: each in turn, then unset
		if len(v.order) == 0 {
			return true
		}
		next := v.order[0]
		if i := slices.Index(v.order, v.modes["session"]); i >= 0 {
			next = "unset"
			if i+1 < len(v.order) {
				next = v.order[i+1]
			}
		}
		send("mode " + next + " --session")
		return true
	case "enter": // a skill: read it in a pane below (skillpane.go)
		if v.kind != "skills" {
			return false
		}
		if r := v.row(m.pg.cur); r != nil {
			m.openSkill(r.name)
		}
		return true
	case "backspace":
		if v.query != "" {
			_, size := utf8.DecodeLastRuneInString(v.query)
			v.query = v.query[:len(v.query)-size]
		}
	case "esc":
		if v.query == "" {
			return false
		}
		v.query = ""
	default:
		if text == "" || utf8.RuneCountInString(text) != 1 || !unicode.IsPrint([]rune(text)[0]) {
			return false
		}
		v.query += text
	}
	m.pg.cur, m.pg.off = 0, 0
	m.syncPager()
	return true
}
