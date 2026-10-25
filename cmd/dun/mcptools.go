package main

import (
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// /mcp tools <id> — one server's tools in the pager: a row per tool (name,
// cost, its switch at each scope, the first line of its description), enter
// or → opens the full description and the parameter schema, space turns the
// tool on or off for this session, and ctrl+g/ctrl+p/ctrl+s cycle its
// global, project and session switch as g/p/s do in the /mcp table
// (mcpview.go). Typing filters the list (USER 2026-10-24: some servers have
// hundreds of tools): every word must be in the tool's name or description,
// name matches first; backspace takes a letter back, esc clears the filter
// and then closes.
//
// The engine owns the list and the toggle (control "mcptools", answered with
// an mcp_tools event), as it does for every client; this is only the view.
// The page is the view's, not a convo block's: the pager reads it through
// the view's pointer, so a fresh list after a toggle lays out in place with
// the selection where it was.

type mcpToolRow struct {
	name, desc, schema, hiddenBy string
	global, project, session     string // "on", "off", "" unset
	from                         string // the scope that decided enabled
	tokens                       int
	enabled                      bool
}

type mcpToolsView struct {
	server string
	rows   []mcpToolRow
	query  string // the filter typed so far
}

// shown is the rows the filter keeps, as indexes into rows: every word of
// the query in the name or the description, case aside; name matches first,
// each group in the server's order.
func (v *mcpToolsView) shown() []int {
	words := strings.Fields(strings.ToLower(v.query))
	var byName, byDesc []int
	for i, r := range v.rows {
		name, desc := strings.ToLower(r.name), strings.ToLower(r.desc)
		inName, all := true, true
		for _, w := range words {
			n := strings.Contains(name, w)
			inName = inName && n
			all = all && (n || strings.Contains(desc, w))
		}
		switch {
		case inName:
			byName = append(byName, i)
		case all:
			byDesc = append(byDesc, i)
		}
	}
	return append(byName, byDesc...)
}

// row is the selected row under the filter, or nil.
func (v *mcpToolsView) row(cur int) *mcpToolRow {
	s := v.shown()
	if cur < 0 || cur >= len(s) {
		return nil
	}
	return &v.rows[s[cur]]
}

func (v *mcpToolsView) page() pageDoc {
	on, cost := 0, 0
	for _, r := range v.rows {
		if r.enabled {
			on++
			cost += r.tokens
		}
	}
	shown := v.shown()
	nodes := make([]pageNode, 0, len(shown))
	for _, i := range shown {
		r := v.rows[i]
		mark := "○"
		if r.enabled {
			mark = "●"
		}
		first, _, _ := strings.Cut(strings.TrimSpace(r.desc), "\n")
		head := fmt.Sprintf("%s %s  ~%s tok  g%s p%s s%s  %s", mark, r.name, kTokens(r.tokens),
			cellMark(r.global), cellMark(r.project), cellMark(r.session), first)
		body := strings.TrimSpace(r.desc)
		if r.from != "" {
			body = onOff(r.enabled) + " (" + r.from + ")\n\n" + body
		}
		if r.hiddenBy != "" {
			body = "hidden by " + r.hiddenBy + "\n\n" + body
		}
		body += "\n\nparameters:\n" + r.schema
		nodes = append(nodes, pageNode{head: head, body: body})
	}
	title := fmt.Sprintf("mcp tools · %s: %d of %d on · ~%s tokens", v.server, on, len(v.rows), kTokens(cost))
	if v.query != "" {
		title = fmt.Sprintf("mcp tools · %s · filter %q: %d shown · %d of %d on · ~%s tokens", v.server, v.query, len(shown), on, len(v.rows), kTokens(cost))
		if len(shown) == 0 {
			nodes = append(nodes, pageNode{head: "no tool matches " + strconvQuote(v.query) + " — backspace or esc"})
		}
	}
	return pageDoc{title: title, nodes: nodes}
}

// openMCPTools asks the engine for a server's tools; the pager opens when
// they arrive (noteMCPTools).
func (m *tuiModel) openMCPTools(id string) {
	m.mcpToolsWant = id
}

// toolsPaging is the tools view the pager shows, if it shows one.
func (m *tuiModel) toolsPaging() *mcpToolsView {
	if !m.paging {
		return nil
	}
	v, _ := m.pg.id.(*mcpToolsView)
	return v
}

// noteMCPTools takes an mcp_tools event: a fresh list for the open view, the
// list this TUI asked for, or (no server) the cost listing as a note.
func (m tuiModel) noteMCPTools(ev evMsg) tuiModel {
	server, msg := str(ev["server"]), strings.TrimSpace(str(ev["message"]))
	var rows []mcpToolRow
	if ts, ok := ev["tools"].([]any); ok {
		for _, t := range ts {
			o, _ := t.(map[string]any)
			en, _ := o["enabled"].(bool)
			rows = append(rows, mcpToolRow{name: str(o["name"]), desc: str(o["description"]), schema: str(o["schema"]),
				hiddenBy: str(o["hiddenBy"]), tokens: int(evNum(o["tokens"])), enabled: en,
				global: str(o["global"]), project: str(o["project"]), session: str(o["session"]), from: str(o["from"])})
		}
	}
	if msg != "" {
		m.append(replyLine(msg))
	}
	v := m.toolsPaging()
	switch {
	case v != nil && v.server == server:
		if rows != nil {
			v.rows = rows
		}
		if msg != "" {
			m.flashText, m.flashUntil = msg, time.Now().Add(flashFor)
		}
		m.syncPager()
	case server != "" && server == m.mcpToolsWant && len(rows) > 0:
		m.mcpToolsWant = ""
		m.pg = newPager(-1, &mcpToolsView{server: server, rows: rows})
		m.pg.toggles = true
		m.paging = true
		m.syncPager()
	}
	return m
}

// toggleMCPTool sends space on the selected row to the engine.
func (m *tuiModel) toggleMCPTool(v *mcpToolsView) {
	r := v.row(m.pg.cur)
	if r == nil {
		return
	}
	to := "off"
	if !r.enabled {
		to = "on"
	}
	if !m.proc.controlCmd("mcptools", "toggle "+v.server+" "+r.name+" "+to) {
		m.flashText, m.flashUntil = "no engine right now — /reconnect first", time.Now().Add(flashFor)
	}
}

// toolKey cycles the selected tool's switch: ctrl+g global on/off, ctrl+p
// and ctrl+s project and session unset → on → off. False: not one of those
// keys.
func (m *tuiModel) toolKey(v *mcpToolsView, k string) bool {
	k, ok := strings.CutPrefix(k, "ctrl+")
	if !ok || (k != "g" && k != "p" && k != "s") {
		return false
	}
	r := v.row(m.pg.cur)
	if r == nil {
		return true
	}
	var to, scope string
	switch k {
	case "g":
		to, scope = "on", "global"
		if r.global == "on" {
			to = "off"
		}
	case "p":
		to, scope = nextTri(r.project), "project"
	case "s":
		to, scope = nextTri(r.session), "session"
	}
	if !m.proc.controlCmd("mcptools", "toggle "+v.server+" "+r.name+" "+to+" --"+scope) {
		m.flashText, m.flashUntil = "no engine right now — /reconnect first", time.Now().Add(flashFor)
	}
	return true
}

// filterKey types into the tools view's filter: a printable key adds to it,
// backspace takes one back, esc clears it (and closes the view once it is
// empty: false). True: the key was the filter's.
func (m *tuiModel) filterKey(v *mcpToolsView, k, text string) bool {
	switch {
	case k == "backspace":
		if v.query != "" {
			_, size := utf8.DecodeLastRuneInString(v.query)
			v.query = v.query[:len(v.query)-size]
		}
	case k == "esc":
		if v.query == "" {
			return false
		}
		v.query = ""
	case text != "" && k != "space" && utf8.RuneCountInString(text) == 1 && unicode.IsPrint([]rune(text)[0]):
		v.query += text
	default:
		return false
	}
	m.pg.cur, m.pg.off = 0, 0
	m.syncPager()
	return true
}

// strconvQuote is s in double quotes.
func strconvQuote(s string) string { return fmt.Sprintf("%q", s) }
