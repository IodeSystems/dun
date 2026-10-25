package main

import (
	"strings"
	"time"

	"charm.land/lipgloss/v2"

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
	spin     string // the spinner's frame, for a row still starting
}

// stDisconnected is a server that is not connected: yellow, not the
// attention orange (USER 2026-10-25: a spinner while it starts, a green dot
// once connected, a yellow hollow circle when not).
var stDisconnected = lipgloss.NewStyle().Foreground(lipgloss.Color("220"))

// connMark is a row's first cell: is it connected, or on its way.
func (v *mcpServersView) connMark(st dun.ServerState) string {
	switch {
	case st.Starting: // the status spinner's frame, its trailing space cut
		return strings.TrimRight(v.spin, " ")
	case st.Running || st.Hosted:
		return stOK.Render("●")
	}
	return stDisconnected.Render("○")
}

// starting reports whether a row is still starting: the spinner turns.
func (v *mcpServersView) starting() bool {
	for _, st := range v.rows {
		if st.Starting {
			return true
		}
	}
	return false
}

func (v *mcpServersView) page() pageDoc {
	idW, viaW := tableWidths(v.rows)
	nodes := make([]pageNode, 0, len(v.rows))
	for i, st := range v.rows {
		body := serverBody(st, v.details[i])
		nodes = append(nodes, pageNode{head: v.connMark(st) + " " + serverRow(st, idW, viaW, v.details[i]), body: body, err: st.Err != "", pre: true})
	}
	return pageDoc{title: "mcp servers · " + switchLegend, header: "  " + serverHeader(idW, viaW, hotKey), nodes: nodes}
}

// hint is the status line: the keys, with the scope m writes to.
func (v *mcpServersView) hint() string {
	if v.pickVia {
		return stDim.Render("mode scope: ") + hotKey("global", "g") + stDim.Render(" · ") + hotKey("project", "p") +
			stDim.Render(" · ") + hotKey("session", "s") + stDim.Render(" · esc cancel")
	}
	return hotKey("global", "g") + stDim.Render(" on/off · ") + hotKey("project", "p") + stDim.Render("/") +
		hotKey("session", "s") + stDim.Render(" unset→on→off · ") + hotKey("mode", "m") +
		stDim.Render(" at "+v.viaScope+" (M+g/p/s: scope) · ") + hotKey("tools", "t") + stDim.Render(" · enter details · esc back")
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
		hosted, _ := o["hosted"].(bool)
		running, _ := o["running"].(bool)
		starting, _ := o["starting"].(bool)
		missing, _ := o["missing"].(bool)
		var log []dun.ServerLogLine
		if ls, ok := o["log"].([]any); ok {
			for _, l := range ls {
				lo, _ := l.(map[string]any)
				se, _ := lo["stderr"].(bool)
				log = append(log, dun.ServerLogLine{At: time.UnixMilli(int64(evNum(lo["at"]))), Text: str(lo["text"]), Stderr: se})
			}
		}
		out = append(out, dun.ServerState{ID: str(o["id"]), Auto: auto, Running: running, Hosted: hosted, Log: log, Err: str(o["err"]), Missing: missing,
			Starting: starting, StartingSeconds: evNum(o["startingSeconds"]),
			Via: str(o["via"]), Proto: str(o["proto"]), Global: str(o["global"]), Project: str(o["project"]), Session: str(o["session"]),
			From: str(o["from"]), ViaGlobal: str(o["viaGlobal"]), ViaProject: str(o["viaProject"]),
			ViaSession: str(o["viaSession"]), ViaFrom: str(o["viaFrom"]),
			Tools: int(evNum(o["tools"])), Slash: str(o["slash"]),
			Index: str(o["index"]), IndexSeconds: evNum(o["indexSeconds"]), IndexErr: str(o["indexErr"]),
			Cost: int(evNum(o["cost"])), Standalone: int(evNum(o["standalone"])),
			Description: str(o["description"]), Title: str(o["title"]), Version: str(o["version"]), Instructions: str(o["instructions"])})
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
	m.pg = newPager(-1, &mcpServersView{rows: rows, details: details, viaScope: dun.ScopeSession, spin: m.spin.View()})
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
	if k == "t" { // its tools, in their own filterable view (mcptools.go)
		if m.pg.cur >= 0 && m.pg.cur < len(v.rows) {
			id := v.rows[m.pg.cur].ID
			m.openMCPTools(id)
			if !m.proc.controlCmd("mcptools", id) {
				m.flashText, m.flashUntil = "no engine right now — /reconnect first", time.Now().Add(flashFor)
			}
		}
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

// serverBody is a server's detail under its row, in words: what it is (dun's
// line for it, and what it says about itself while it runs), whether it is
// on and which switch decided, its mode, its tools and what they cost, and
// the detail line the engine sent.
func serverBody(st dun.ServerState, detail string) string {
	var rows []string
	what := st.Description
	if st.Title != "" && !strings.EqualFold(st.Title, st.ID) {
		what = strings.TrimSpace(st.Title + " — " + what)
	}
	if what != "" {
		rows = append(rows, what)
	}
	if st.Version != "" {
		rows = append(rows, "version "+st.Version)
	}
	if ins := st.Instructions; ins != "" {
		lines := strings.Split(ins, "\n")
		if len(lines) > 4 {
			lines = append(lines[:4], "…")
		}
		rows = append(rows, "it says: "+strings.Join(lines, "\n  "))
	}
	rows = append(rows, switchSentence(st))
	if st.Via != "" {
		rows = append(rows, "mode: "+modeSentence(st))
	}
	if st.Tools > 0 {
		t := plural(st.Tools, "tool")
		switch {
		case st.Via == dun.ViaShell:
			t += " inside the shell's eval, 0 tokens in every request"
			if st.Standalone > 0 {
				t += " (~" + tokN(st.Standalone) + " standalone)"
			}
		case st.Cost > 0:
			t += ", ~" + tokN(st.Cost) + " tokens in every request"
		}
		rows = append(rows, t+" — t lists them, type to filter")
	}
	if detail != "" {
		rows = append(rows, detail)
	}
	if lines := logLines(st.Log, serverBodyLog); lines != "" {
		rows = append(rows, "activity:\n"+lines)
	}
	return strings.Join(rows, "\n")
}

// serverBodyLog is how many of a server's newest log lines its detail shows.
const serverBodyLog = 8

// logLines is a server's newest n log lines, one a row: the time, then the
// line; what it wrote on stderr is marked so.
func logLines(log []dun.ServerLogLine, n int) string {
	if len(log) > n {
		log = log[len(log)-n:]
	}
	rows := make([]string, 0, len(log))
	for _, l := range log {
		text := l.Text
		if l.Stderr {
			text = "stderr: " + text
		}
		rows = append(rows, "  "+stDim.Render(l.At.Format("15:04:05"))+" "+text)
	}
	return strings.Join(rows, "\n")
}

// switchSentence says whether a server is on and which switch decided, in
// words, and the switches that did not.
func switchSentence(st dun.ServerState) string {
	where := map[string]string{dun.ScopeGlobal: "for every project", dun.ScopeProject: "for this project",
		dun.ScopeSession: "for this session", "flag": "by this run's flags"}
	s := onOff(st.Auto)
	if w, ok := where[st.From]; ok {
		s += ": switched " + onOff(st.Auto) + " " + w
	}
	var also []string
	if st.Project != "" && st.From != dun.ScopeProject {
		also = append(also, "this project says "+st.Project)
	}
	if st.Session != "" && st.From != dun.ScopeSession {
		also = append(also, "this session says "+st.Session)
	}
	if st.From == dun.ScopeGlobal && len(also) == 0 {
		s += ", nothing set for this project or session"
	}
	if len(also) > 0 {
		s += " (" + strings.Join(also, ", ") + ")"
	}
	return s + " — g every project · p this project · s this session"
}

// modeSentence is a placed server's mode, in words.
func modeSentence(st dun.ServerState) string {
	s := dun.ModeWord(st.Via)
	if st.Via == dun.ViaShell {
		s += ": its tools are called inside the shell's eval as " + dun.ShellNamespace(st.ID) + ".<tool>(…), and their schemas stay out of every request"
	} else {
		s += ": its tools are sent with every request"
	}
	if st.ViaFrom != "" {
		s += " (set " + map[string]string{dun.ScopeGlobal: "for every project", dun.ScopeProject: "for this project", dun.ScopeSession: "for this session"}[st.ViaFrom] + ")"
	}
	return s + " — m changes it"
}
