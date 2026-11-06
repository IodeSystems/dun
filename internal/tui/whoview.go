package tui

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"

	"github.com/iodesystems/dun/internal/dunconfig"
	"github.com/iodesystems/dun/internal/dundcli"
	"github.com/iodesystems/dun/proto"
)

// /who: this session, its sub-agents under it as a tree, and every other
// live session of this user, with the `dun msg` listeners (USER 2026-11-06).
// One screen (panes.go): a status pane (the counts, a dund failure) over the
// list in the pager. It is a type-to-filter list as /skill is
// (switchlist.go): every word in a row's name, state, task, prompt,
// workspace or branch; a sub-agent that matches keeps this session above it,
// so the tree still reads. Enter or → opens a row's details; backspace takes
// a letter back; esc clears the filter, then closes; ctrl+r asks dund again.
//
// The names are dund's (dun_list's: the workspace folder, then the session's
// time where two share one), the same names dun_msg and `dun msg` take. This
// session's comes from the same answer: until it arrives, or with no dund,
// it is the session id. The sub-agents are the agents events the activity
// strip shows (m.agents), so they update live; a sub-agent has no name of its
// own, only its number (#N), and never a child (subagent.go: a child does
// not get the agent tool), so the tree is one level deep. dund is asked once
// on open (whoFetch), off the update loop; the sessions do not update live.

type whoView struct {
	self      string // this session's id
	selfRow   *proto.PeerSession
	agents    []agentRow
	peers     []proto.PeerSession // the other sessions, in dund's order
	listeners []string
	loading   bool   // dund not answered yet
	err       string // dund's failure: no other session is listed
	query     string
	width     int
	now       time.Time // for a running agent's time
}

func (v *whoView) setWidth(w int) { v.width = w }

// name is what this session is called: dund's name, else its id.
func (v *whoView) name() string {
	switch {
	case v.selfRow != nil && v.selfRow.Name != "":
		return v.selfRow.Name
	case v.self != "":
		return v.self
	}
	return "this session"
}

// whoRow is one row of the list before the filter.
type whoRow struct {
	kind  string // "self", "agent", "peer", "listener"
	head  string // plain, the tree's glyphs in it
	sub   string // a narrow screen's second line
	body  string
	match string // what the filter searches, lower case
}

func (v *whoView) rows() []whoRow {
	narrow := v.width > 0 && v.width < narrowW
	var out []whoRow
	self := whoRow{kind: "self"}
	self.head, self.sub, self.body = sessionRow(v.name(), v.selfRow, true, narrow)
	self.match = strings.ToLower(v.name() + " " + self.body)
	out = append(out, self)
	for i, a := range v.agents {
		glyph := "├─ "
		if i == len(v.agents)-1 {
			glyph = "└─ "
		}
		r := whoRow{kind: "agent"}
		label := fmt.Sprintf("#%d %s", a.id, a.state)
		if s := a.elapsed(v.now); s > 0 {
			label += " " + compactDur(s)
		}
		if a.tokens > 0 {
			label += " " + compactTokens(a.tokens)
		}
		detail := a.status // what it is doing beats what it was asked (agentActRow)
		if detail == "" {
			detail = a.prompt
		}
		detail = oneLineOf(detail)
		r.head = "  " + glyph + label
		if narrow {
			r.sub = detail
		} else if detail != "" {
			r.head += "  " + detail
		}
		var b strings.Builder
		fmt.Fprintf(&b, "sub-agent #%d of %s\nstate: %s", a.id, v.name(), a.state)
		if a.model != "" {
			fmt.Fprintf(&b, "\nmodel: %s", a.model)
		}
		if a.status != "" {
			fmt.Fprintf(&b, "\nstatus: %s", a.status)
		}
		fmt.Fprintf(&b, "\ntokens: %d · time: %s\n\nasked:\n%s", a.tokens, compactDur(a.elapsed(v.now)), strings.TrimSpace(a.prompt))
		r.body = b.String()
		r.match = strings.ToLower(fmt.Sprintf("#%d %s %s %s %s", a.id, a.state, a.status, a.prompt, a.model))
		out = append(out, r)
	}
	for i := range v.peers {
		p := &v.peers[i]
		r := whoRow{kind: "peer"}
		r.head, r.sub, r.body = sessionRow(p.Name, p, false, narrow)
		r.match = strings.ToLower(p.Name + " " + r.body)
		out = append(out, r)
	}
	for _, l := range v.listeners {
		out = append(out, whoRow{kind: "listener", head: l + "  listener (dun msg)",
			body:  "a listener: a script, a person or an agent that runs `dun msg --name " + l + "` in a shell; dun_msg and `dun msg " + l + " …` reach it",
			match: strings.ToLower(l + " listener")})
	}
	return out
}

// sessionRow is a session's head, its narrow second line and its details.
func sessionRow(name string, p *proto.PeerSession, you, narrow bool) (head, sub, body string) {
	head = name
	if you {
		head += "  (you)"
	}
	if p == nil {
		return head, "", name + "\n\nthis session"
	}
	facts := p.State
	if p.Attached > 0 {
		facts += fmt.Sprintf(" · %d attached", p.Attached)
	}
	if p.Worktree != "" {
		facts += " · wt " + p.Branch
	} else if p.Branch != "" {
		facts += " · " + p.Branch
	}
	task := oneLineOf(p.Task)
	if narrow {
		sub = facts
		if task != "" {
			sub += " · " + task
		}
	} else {
		head += "  " + facts
		if task != "" {
			head += "  " + task
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s\nid: %s\nstate: %s · %d attached\nworkspace: %s", name, p.ID, p.State, p.Attached, p.Workspace)
	if p.Branch != "" {
		fmt.Fprintf(&b, "\nbranch: %s", p.Branch)
	}
	if p.Worktree != "" {
		fmt.Fprintf(&b, "\nworktree: %s", p.Worktree)
	}
	if p.Task != "" {
		fmt.Fprintf(&b, "\n\ntask:\n%s", p.Task)
	}
	return head, sub, b.String()
}

func oneLineOf(s string) string { return strings.Join(strings.Fields(s), " ") }

// shown is the rows the filter keeps: every word of the query in the row.
// A sub-agent that is kept keeps this session (row 0) above it.
func (v *whoView) shown() []whoRow {
	all := v.rows()
	words := strings.Fields(strings.ToLower(v.query))
	if len(words) == 0 {
		return all
	}
	keep := make([]bool, len(all))
	for i, r := range all {
		keep[i] = true
		for _, w := range words {
			if !strings.Contains(r.match, w) {
				keep[i] = false
				break
			}
		}
		if keep[i] && r.kind == "agent" {
			keep[0] = true
		}
	}
	var out []whoRow
	for i, r := range all {
		if keep[i] {
			out = append(out, r)
		}
	}
	return out
}

func (v *whoView) page() pageDoc {
	shown := v.shown()
	nodes := make([]pageNode, 0, len(shown)+1)
	for _, r := range shown {
		nodes = append(nodes, pageNode{head: r.head, sub: r.sub, body: r.body, pre: true})
	}
	title := "who"
	if v.query != "" {
		title = fmt.Sprintf("filter %q: %d of %d shown", v.query, len(shown), len(v.rows()))
		if len(shown) == 0 {
			nodes = append(nodes, pageNode{head: fmt.Sprintf("nothing matches %q — backspace or esc", v.query)})
		}
	}
	return pageDoc{title: title, nodes: nodes}
}

// lines is the status pane: who this is, then the counts, then why the
// other sessions are missing, when they are.
func (v *whoView) lines(_ *tuiModel, w int) []string {
	live := 0
	for _, a := range v.agents {
		if a.state == "running" || a.state == "idle" {
			live++
		}
	}
	out := wrapToRows(stHeader.Render("/who")+StDim.Render(" · ")+"you are "+v.name(), w)
	counts := fmt.Sprintf("%d sub-agents (%d live)", len(v.agents), live)
	switch {
	case v.loading:
		counts += " · asking dund for the other sessions…"
	case v.err == "":
		counts += fmt.Sprintf(" · %d other sessions · %d listeners", len(v.peers), len(v.listeners))
	}
	out = append(out, wrapToRows(StDim.Render(counts), w)...)
	if v.err != "" {
		out = append(out, wrapToRows(stNote.Render("dund: "+v.err+" — other sessions are not listed"), w)...)
	}
	return out
}

func (v *whoView) hint() string {
	if v.width > 0 && v.width < narrowW {
		return "type · ⏎ open · ^r again · esc"
	}
	return "type to filter · ↑/↓ select · enter/→ details · ctrl+r ask dund again · esc clear, back"
}

// whoMsg is dund's answer to /who's question.
type whoMsg struct {
	list *proto.MsgWhoList
	err  error
}

// whoFetch asks dund for every live session and listener, as `dun msg
// --who` does; a variable for the tests.
var whoFetch = func() (*proto.MsgWhoList, error) {
	c, err := net.DialTimeout("unix", dundcli.Socket(dunconfig.DunHome()), time.Second)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	ac := &attachConn{c: c, wait: 3 * time.Second}
	ev, err := ac.call(sc, "", &proto.MsgWho{})
	if err != nil {
		return nil, err
	}
	l, ok := ev.Payload.(*proto.MsgWhoList)
	if !ok {
		return nil, fmt.Errorf("answered %s: is it older than this dun? (dun dund … restart it)", ev.Type)
	}
	return l, nil
}

func whoCmd() tea.Cmd {
	return func() tea.Msg {
		l, err := whoFetch()
		return whoMsg{list: l, err: err}
	}
}

// whoSlash is /who: the screen opens at once with what this TUI knows (its
// id, its sub-agents) and asks dund for the rest.
func whoSlash(m *tuiModel, _ []string) tea.Cmd {
	v := &whoView{self: m.sessionID, agents: m.agents, loading: true, now: time.Now()}
	m.pg = newPager(-1, v)
	m.paging = true
	m.scr = whoScreen()
	m.syncPager()
	return whoCmd()
}

// whoPaging is the /who view, if it is up.
func (m *tuiModel) whoPaging() *whoView {
	if !m.paging {
		return nil
	}
	v, _ := m.pg.id.(*whoView)
	return v
}

// whoDone takes dund's answer into an open view: this session's row out of
// the list, the rest as the others.
func (m *tuiModel) whoDone(msg whoMsg) {
	v := m.whoPaging()
	if v == nil {
		return
	}
	v.loading, v.err, v.selfRow, v.peers, v.listeners = false, "", nil, nil, nil
	if msg.err != nil {
		v.err = msg.err.Error()
		m.syncPager()
		return
	}
	for _, p := range msg.list.Sessions {
		if p.ID == v.self && v.self != "" {
			p := p
			v.selfRow = &p
			continue
		}
		v.peers = append(v.peers, p)
	}
	v.listeners = msg.list.Listeners
	m.syncPager()
}

// syncWho redraws an open view from the sub-agents: an agents event came.
func (m *tuiModel) syncWho() {
	if v := m.whoPaging(); v != nil {
		v.agents, v.now = m.agents, time.Now()
		m.syncPager()
	}
}

// whoKey handles a key in the view; false: not its key (the pager's).
func (m *tuiModel) whoKey(v *whoView, k, text string) (bool, tea.Cmd) {
	switch k {
	case "ctrl+r":
		v.loading = true
		m.syncPager()
		return true, whoCmd()
	case "backspace":
		if v.query != "" {
			_, size := utf8.DecodeLastRuneInString(v.query)
			v.query = v.query[:len(v.query)-size]
		}
	case "esc":
		if v.query == "" {
			return false, nil
		}
		v.query = ""
	case "space": // a second word; before the first, the pager's page down
		if v.query == "" {
			return false, nil
		}
		v.query += " "
	default:
		if text == "" || utf8.RuneCountInString(text) != 1 || !unicode.IsPrint([]rune(text)[0]) {
			return false, nil
		}
		v.query += text
	}
	m.pg.cur, m.pg.off = 0, 0
	m.syncPager()
	return true, nil
}

func whoScreen() *paneScreen {
	return &paneScreen{
		panes: []screenPane{{body: viewStatusPane{}, fixed: true}, {body: whoPane{}, weight: 1, min: 3}},
		focus: 1,
		valid: func(m *tuiModel) bool { return m.whoPaging() != nil },
	}
}

// whoPane is the pager showing the list; its keys are the filter's and the
// pager's.
type whoPane struct{}

func (whoPane) view(m *tuiModel, w, h int) string { return m.pg.view(w, h) }

func (whoPane) key(m *tuiModel, msg tea.KeyPressMsg) bool {
	if v := m.whoPaging(); v != nil {
		if ok, cmd := m.whoKey(v, msg.String(), msg.Text); ok {
			m.scrCmd = cmd
			return true
		}
	}
	return m.pg.key(msg.String())
}

func (whoPane) hint(m *tuiModel) string {
	if v := m.whoPaging(); v != nil {
		return StDim.Render(v.hint())
	}
	return ""
}
