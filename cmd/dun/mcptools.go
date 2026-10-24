package main

import (
	"fmt"
	"strings"
	"time"
)

// /mcp tools <id> — one server's tools in the pager: a row per tool (name,
// cost, its switch at each scope, the first line of its description), enter
// or → opens the full description and the parameter schema, space turns the
// tool on or off for this session, and g/p/s cycle its global, project and
// session switch as in the /mcp table (mcpview.go).
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
}

func (v *mcpToolsView) page() pageDoc {
	on, cost := 0, 0
	nodes := make([]pageNode, 0, len(v.rows))
	for _, r := range v.rows {
		mark := "○"
		if r.enabled {
			mark = "●"
			on++
			cost += r.tokens
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
	if m.pg.cur < 0 || m.pg.cur >= len(v.rows) {
		return
	}
	r := v.rows[m.pg.cur]
	to := "off"
	if !r.enabled {
		to = "on"
	}
	if !m.proc.controlCmd("mcptools", "toggle "+v.server+" "+r.name+" "+to) {
		m.flashText, m.flashUntil = "no engine right now — /reconnect first", time.Now().Add(flashFor)
	}
}

// toolKey cycles the selected tool's switch: g global on/off, p and s
// project and session unset → on → off. False: not one of those keys.
func (m *tuiModel) toolKey(v *mcpToolsView, k string) bool {
	if k != "g" && k != "p" && k != "s" {
		return false
	}
	if m.pg.cur < 0 || m.pg.cur >= len(v.rows) {
		return true
	}
	r := v.rows[m.pg.cur]
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
