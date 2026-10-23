package main

import (
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/iodesystems/dun"
)

// /mcp in the TUI — the switch table in the pager: a row per server, enter
// opens its detail, and a key cycles a column of the selected row:
//
//	g  global   on ↔ off
//	p  project  unset → on → off → unset
//	s  session  unset → on → off → unset
//	m  mode     scripted → standalone → unset, at the mode scope (session
//	            unless changed); global has no unset, so it is the two
//	M  then g/p/s chooses the mode scope
//
// Each key sends the typed command (/mcp on <id> --global …) through the
// engine like any other client, and the server event that answers refreshes
// the rows in place.

// stHotKey marks a hotkey letter inside a word. Bold and underline, no color:
// under NO_COLOR (lipgloss's Ascii profile) it renders as the plain word.
var stHotKey = lipgloss.NewStyle().Bold(true).Underline(true)

// hotKey renders word with its key letter marked; word unchanged when the
// letter is not in it.
func hotKey(word, key string) string {
	i := strings.Index(word, key)
	if key == "" || i < 0 {
		return word
	}
	return word[:i] + stHotKey.Render(word[i:i+len(key)]) + word[i+len(key):]
}

// mcpServersView is the table's rows, as the last server event gave them.
type mcpServersView struct {
	rows     []dun.ServerState
	details  []string
	viaScope string // the scope m writes
	pickVia  bool   // M was pressed: the next g/p/s picks viaScope
}

func (v *mcpServersView) page() pageDoc {
	idW, viaW := tableWidths(v.rows)
	nodes := make([]pageNode, 0, len(v.rows))
	for i, st := range v.rows {
		body := "switches: global " + cellWord(st.Global) + " · project " + cellWord(st.Project) +
			" · session " + cellWord(st.Session) + " → " + stateCell(st)
		if st.Via != "" {
			body += "\nmode: global " + modeWord(st.ViaGlobal) + " · project " + modeWord(st.ViaProject) +
				" · session " + modeWord(st.ViaSession) + " → " + viaCell(st)
		}
		body += "\n" + v.details[i]
		nodes = append(nodes, pageNode{head: serverRow(st, idW, viaW, v.details[i]), body: body, err: st.Err != ""})
	}
	return pageDoc{title: "mcp servers · " + switchLegend, header: serverHeader(idW, viaW, hotKey), nodes: nodes}
}

// hint is the status line: the keys, with the scope m writes to.
func (v *mcpServersView) hint() string {
	if v.pickVia {
		return stDim.Render("mode scope: ") + hotKey("global", "g") + stDim.Render(" · ") + hotKey("project", "p") +
			stDim.Render(" · ") + hotKey("session", "s") + stDim.Render(" · esc cancel")
	}
	return hotKey("global", "g") + stDim.Render(" on/off · ") + hotKey("project", "p") + stDim.Render("/") +
		hotKey("session", "s") + stDim.Render(" unset→on→off · ") + hotKey("mode", "m") +
		stDim.Render(" at "+v.viaScope+" (M+g/p/s: scope) · enter details · esc back")
}

// serverStates reads a server event's list.
func serverStates(list []any) ([]dun.ServerState, []string) {
	var out []dun.ServerState
	var details []string
	for _, s := range list {
		o, ok := s.(map[string]any)
		if !ok || str(o["id"]) == "" {
			continue
		}
		auto, _ := o["auto"].(bool)
		running, _ := o["running"].(bool)
		out = append(out, dun.ServerState{ID: str(o["id"]), Auto: auto, Running: running, Err: str(o["err"]),
			Via: str(o["via"]), Proto: str(o["proto"]), Global: str(o["global"]), Project: str(o["project"]), Session: str(o["session"]),
			From: str(o["from"]), ViaGlobal: str(o["viaGlobal"]), ViaProject: str(o["viaProject"]),
			ViaSession: str(o["viaSession"]), ViaFrom: str(o["viaFrom"])})
		details = append(details, str(o["detail"]))
	}
	return out, details
}

// openMCPServers asks for the table; it opens when the listing arrives.
func (m *tuiModel) openMCPServers() { m.mcpServersWant = true }

// serversPaging is the table the pager shows, if it shows one.
func (m *tuiModel) serversPaging() *mcpServersView {
	if !m.paging {
		return nil
	}
	v, _ := m.pg.id.(*mcpServersView)
	return v
}

// noteServerView takes a server event's list: the table this TUI asked
// for opens, an open one refreshes.
func (m *tuiModel) noteServerView(list []any) {
	rows, details := serverStates(list)
	if v := m.serversPaging(); v != nil {
		v.rows, v.details = rows, details
		m.syncPager()
	}
}

// openServerTable opens the table on rows; it reports whether it did.
func (m *tuiModel) openServerTable(list []any) bool {
	if !m.mcpServersWant {
		return false
	}
	m.mcpServersWant = false
	rows, details := serverStates(list)
	if len(rows) == 0 {
		return false
	}
	m.pg = newPager(-1, &mcpServersView{rows: rows, details: details, viaScope: dun.ScopeSession})
	m.paging = true
	m.syncPager()
	return true
}

// nextTri is unset → on → off → unset.
func nextTri(v string) string {
	switch v {
	case "":
		return "on"
	case "on":
		return "off"
	}
	return "unset"
}

// nextVia is scripted → standalone → unset → scripted; global has no unset.
func nextVia(v, scope string) string {
	switch v {
	case dun.ViaShell:
		return dun.ViaDirect
	case dun.ViaDirect:
		if scope == dun.ScopeGlobal {
			return dun.ViaShell
		}
		return "unset"
	}
	return dun.ViaShell
}

// serverKey handles a key on the table; false leaves it to the pager.
func (m *tuiModel) serverKey(v *mcpServersView, k string) bool {
	if v.pickVia {
		v.pickVia = false
		if s, ok := dun.ParseScope(k); ok && len(k) == 1 {
			v.viaScope = s
		}
		return true
	}
	if k == "M" {
		v.pickVia = true
		return true
	}
	if k != "g" && k != "p" && k != "s" && k != "m" {
		return false
	}
	if m.pg.cur < 0 || m.pg.cur >= len(v.rows) {
		return true
	}
	st := v.rows[m.pg.cur]
	var line string
	switch k {
	case "g":
		line = "mcp " + map[bool]string{true: "off", false: "on"}[st.Global == "on"] + " " + st.ID + " --global"
	case "p", "s":
		scope, cur := dun.ScopeProject, st.Project
		if k == "s" {
			scope, cur = dun.ScopeSession, st.Session
		}
		line = "mcp " + nextTri(cur) + " " + st.ID + " --" + scope
	case "m":
		if st.Via == "" {
			m.flashText, m.flashUntil = st.ID+" is built in: it is always standalone", time.Now().Add(flashFor)
			return true
		}
		cur := map[string]string{dun.ScopeGlobal: st.ViaGlobal, dun.ScopeProject: st.ViaProject, dun.ScopeSession: st.ViaSession}[v.viaScope]
		if v.viaScope == dun.ScopeGlobal && cur == "" {
			cur = dun.ViaDirect
		}
		next := nextVia(cur, v.viaScope)
		if next != "unset" {
			next = dun.ModeWord(next)
		}
		line = "mcp mode " + st.ID + " " + next + " --" + v.viaScope
	}
	if !m.proc.controlCmd("slash", line) {
		m.flashText, m.flashUntil = "no engine right now — /reconnect first", time.Now().Add(flashFor)
		return true
	}
	m.flashText, m.flashUntil = "/"+line, time.Now().Add(flashFor)
	return true
}
