package main

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/iodesystems/dun"
)

// /ext in the TUI: a screen (panes.go) with a status pane (how many there
// are, how many are on, where they load from, what did not load), the list
// (each extension's server state, source, command and roles; enter opens
// what it adds: its commands, hooks and where it is from), and the selected
// one's server log. An extension's on/off is its server's, so g/p/s send
// /mcp's switch commands (mcpview.go). The engine sends the list (control
// "ext" "view", an extensions event); the servers' state comes in the
// server events, by id. Other clients get /ext's text listing.

type extItem struct {
	id, source, path, desc, slash string
	commands                      [][2]string // name, help
	roles, hooks, hostHooks       []string
}

type extView struct {
	items   []extItem
	errors  []string
	addPath string
	servers map[string]dun.ServerState // by id, from the last server event
	spin    string
	width   int
}

func (v *extView) setWidth(w int) { v.width = w }

func (v *extView) server(id string) (dun.ServerState, bool) {
	st, ok := v.servers[id]
	return st, ok
}

func (v *extView) selectedServer(cur int) (dun.ServerState, bool) {
	if cur < 0 || cur >= len(v.items) {
		return dun.ServerState{}, false
	}
	st, ok := v.server(v.items[cur].id)
	st.ID = v.items[cur].id
	return st, ok
}

func (v *extView) lines(_ *tuiModel, w int) []string {
	on := 0
	for _, it := range v.items {
		if st, ok := v.server(it.id); ok && st.Auto {
			on++
		}
	}
	out := wrapToRows(stHeader.Render("/ext")+stDim.Render(" · ")+fmt.Sprintf("%d of %d on", on, len(v.items)), w)
	add := "add one: " + tildePath(v.addPath)
	if w >= narrowW {
		add = "bundled < user < project; the same id replaces · " + add
	}
	out = append(out, wrapToRows(stDim.Render(add), w)...)
	if n := len(v.errors); n > 0 {
		out = append(out, wrapToRows(stNote.Render(fmt.Sprintf("⚠ %s did not load: the last rows say why", plural(n, "manifest"))), w)...)
	}
	return out
}

// state is a row's state in words: starting, running and its tools, off.
func (v *extView) state(st dun.ServerState, ok bool) string {
	switch {
	case !ok:
		return "no server"
	case st.Starting:
		return "starting"
	case st.Running:
		return "running · " + plural(st.Tools, "tool")
	case st.Auto && st.Err != "":
		return "on, not running"
	}
	return "off"
}

func (v *extView) page() pageDoc {
	narrow := v.width > 0 && v.width < narrowW
	nodes := make([]pageNode, 0, len(v.items)+len(v.errors))
	for _, it := range v.items {
		st, ok := v.server(it.id)
		mark := (&mcpServersView{spin: v.spin}).connMark(st)
		what := it.source
		if it.slash != "" {
			what += " · /" + it.slash
		}
		if len(it.roles) > 0 {
			what += " · " + strings.Join(it.roles, ", ")
		}
		head := mark + " " + it.id + " · " + v.state(st, ok) + " · " + what
		sub := ""
		if narrow {
			head, sub = mark+" "+it.id+" · "+v.state(st, ok), what
		}
		var body []string
		if it.desc != "" {
			body = append(body, it.desc)
		}
		if ok {
			body = append(body, switchSentence(st))
		}
		for _, c := range it.commands {
			body = append(body, "/"+it.slash+" "+c[0]+" — "+c[1])
		}
		if len(it.hooks) > 0 {
			body = append(body, "hooks: "+strings.Join(it.hooks, ", "))
		}
		if len(it.hostHooks) > 0 {
			body = append(body, "host hooks: "+strings.Join(it.hostHooks, ", "))
		}
		from := it.source
		if it.path != "" {
			from += " · " + it.path
		}
		body = append(body, "from: "+from)
		nodes = append(nodes, pageNode{head: head, sub: sub, body: strings.Join(body, "\n"), err: ok && st.Auto && !st.Running && st.Err != ""})
	}
	for _, e := range v.errors {
		nodes = append(nodes, pageNode{head: "✗ " + e, body: e, err: true})
	}
	return pageDoc{title: plural(len(v.items), "extension"), nodes: nodes}
}

func (v *extView) hint() string {
	if v.width > 0 && v.width < narrowW {
		return "g p s on/off · enter open · tab log · esc"
	}
	return hotKey("global", "g") + stDim.Render(" on/off · ") + hotKey("project", "p") + stDim.Render("/") +
		hotKey("session", "s") + stDim.Render(" unset→on→off · enter details · tab its log · esc back")
}

// starting reports whether a row's server is starting: the spinner turns.
func (v *extView) starting() bool {
	for _, it := range v.items {
		if st, ok := v.server(it.id); ok && st.Starting {
			return true
		}
	}
	return false
}

// openExtensions asks the engine for the list; it opens when it arrives.
func (m *tuiModel) openExtensions() {
	m.extWant = true
	if !m.proc.controlCmd("ext", "view") {
		m.extWant = false
		m.append(stErr.Render("no engine right now — /reconnect first"))
	}
}

// extPaging is the /ext list the pager shows, if it shows one.
func (m *tuiModel) extPaging() *extView {
	if !m.paging {
		return nil
	}
	v, _ := m.pg.id.(*extView)
	return v
}

func (m *tuiModel) serverMap() map[string]dun.ServerState {
	out := make(map[string]dun.ServerState, len(m.servers))
	for _, st := range m.servers {
		out[st.ID] = st
	}
	return out
}

// noteExtensions takes an extensions event: the list this TUI asked for
// opens; an open one refreshes.
func (m tuiModel) noteExtensions(ev evMsg) tuiModel {
	v := &extView{addPath: str(ev["addPath"]), servers: m.serverMap(), spin: m.spin.View()}
	if errs, ok := ev["errors"].([]any); ok {
		for _, e := range errs {
			v.errors = append(v.errors, str(e))
		}
	}
	items, _ := ev["items"].([]any)
	for _, x := range items {
		o, _ := x.(map[string]any)
		it := extItem{id: str(o["id"]), source: str(o["source"]), path: str(o["path"]), desc: str(o["description"]), slash: str(o["slash"]),
			roles: strList(o["roles"]), hooks: strList(o["hooks"]), hostHooks: strList(o["hostHooks"])}
		if cs, ok := o["commands"].([]any); ok {
			for _, c := range cs {
				co, _ := c.(map[string]any)
				it.commands = append(it.commands, [2]string{str(co["name"]), str(co["help"])})
			}
		}
		v.items = append(v.items, it)
	}
	if cur := m.extPaging(); cur != nil {
		*cur = *v
		m.syncPager()
		return m
	}
	if !m.extWant {
		return m
	}
	m.extWant = false
	m.pg = newPager(-1, v)
	m.paging = true
	m.scr = extScreen()
	m.syncPager()
	return m
}

func strList(x any) []string {
	l, _ := x.([]any)
	out := make([]string, 0, len(l))
	for _, s := range l {
		out = append(out, str(s))
	}
	return out
}

// noteExtServers gives an open /ext list the servers' new state.
func (m *tuiModel) noteExtServers() {
	if v := m.extPaging(); v != nil {
		v.servers = m.serverMap()
		m.syncPager()
	}
}

// extScreen is /ext: its status over the list over the selected one's log.
func extScreen() *paneScreen {
	return &paneScreen{
		panes: []screenPane{{body: viewStatusPane{}, fixed: true}, {body: extListPane{}, weight: 3, min: 4},
			{body: &serverLogPane{follow: true}, weight: 2, min: 3}},
		focus: 1,
		valid: func(m *tuiModel) bool { return m.extPaging() != nil },
	}
}

// extListPane is the pager showing the list: g/p/s switch the selected
// extension's server, the rest are the pager's.
type extListPane struct{}

func (extListPane) view(m *tuiModel, w, h int) string { return m.pg.view(w, h) }

func (extListPane) key(m *tuiModel, msg tea.KeyPressMsg) bool {
	v := m.extPaging()
	if k := msg.String(); v != nil && (k == "g" || k == "p" || k == "s") {
		st, ok := v.selectedServer(m.pg.cur)
		if !ok {
			m.flashText, m.flashUntil = "no server for this extension", time.Now().Add(flashFor)
			return true
		}
		line := switchLine(st, k)
		if !m.proc.controlCmd("slash", line) {
			m.flashText, m.flashUntil = "no engine right now — /reconnect first", time.Now().Add(flashFor)
			return true
		}
		m.flashText, m.flashUntil = "/"+line, time.Now().Add(flashFor)
		return true
	}
	return m.pg.key(msg.String())
}

func (extListPane) hint(m *tuiModel) string {
	if v := m.extPaging(); v != nil {
		return v.hint()
	}
	return ""
}
