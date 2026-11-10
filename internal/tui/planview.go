package tui

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/iodesystems/dun/internal/textfmt"
)

// /plan in the TUI: a screen (panes.go) with a status pane (how much is done,
// plan mode and whether the plan is approved) over the graph as a tree. A
// node with several parents shows under each, the later rows marked "again".
// The engine sends the graph (control "plan" "view", then a plan event
// whenever it changes, whoever changed it); the screen never works out what
// can start, the engine says (Ready, Waiting).
//
//	a            approve the plan (plan mode: the model may change things)
//	e, m         edit the selected node's title, details: the composer takes
//	             the command, filled in, to change and send
//	d            drop the selected node, same way: a reason can be added
//	enter, →     the node's details, done-when, needs, why it waits
//
// Marking a node done is the model's, so there is no key for it. What the
// person changes reaches the model as a note (dun/plan.go).

type planRow struct {
	id, title, details, status, doneWhen, owner, replaces, why, account string
	needs, waiting                                                      []string
	ready, repeat                                                       bool
	depth                                                               int
}

type planView struct {
	rows           []planRow
	mode, approved bool
	total, done    int
	width          int
	// live is who is working, read from the model at every layout
	// (syncPager); phase steps the motion.
	live planLive
	// order maps a page node to its row; -1 is the finished-trees row.
	// keys names each page node (its id, and which time that id is shown),
	// so the selection follows a node when the rows move (keepPlace).
	order []int
	keys  []string
	// animating: the last layout has a row in motion, so the pulse runs.
	animating bool
}

// planLive is what /plan needs to know of the agents to colour and move it.
type planLive struct {
	agents       map[int]string // child id → state
	busy, asking bool           // the main agent: a turn runs; it asks you
	phase        int
}

// planTellTo is a plan node whose owner the composer is telling something.
type planTellTo struct {
	id, title, who, placeholder string
}

func (v *planView) setWidth(w int) { v.width = w }

// planMark is a status in one cell.
func planMark(status string) string {
	switch status {
	case "doing":
		return "◐"
	case "done":
		return "✓"
	case "blocked":
		return "⚠"
	case "dropped":
		return "✗"
	}
	return "○"
}

// planModeWord says plan mode and the approval in words.
func (v *planView) modeWord() string {
	switch {
	case !v.mode:
		return "off"
	case v.approved:
		return "on · approved"
	}
	return "on · not approved: the model cannot change files or run commands"
}

func (v *planView) lines(_ *tuiModel, w int) []string {
	out := wrapToRows(stHeader.Render("/plan")+StDim.Render(" · ")+fmt.Sprintf("%d of %d done", v.done, v.total), w)
	out = append(out, wrapToRows(StDim.Render("plan mode: ")+v.modeWord(), w)...)
	if v.mode && !v.approved && v.total > 0 {
		out = append(out, wrapToRows(stNote.Render("a approves it"), w)...)
	}
	return out
}

func (v *planView) page() pageDoc {
	narrow := v.width > 0 && v.width < narrowW
	v.order, v.keys, v.animating = v.order[:0], v.keys[:0], false
	seen := map[string]int{}
	live, done := v.segments()
	nodes := make([]pageNode, 0, len(v.rows))
	for _, i := range live {
		r := v.rows[i]
		mark, style, ownerTag := v.motion(r)
		indent := strings.Repeat("  ", r.depth)
		head := indent + r.id + " " + mark + " " + r.title
		var tags []string
		if r.repeat {
			tags = append(tags, "again")
		}
		if r.ready {
			tags = append(tags, "ready")
		}
		if ownerTag != "" {
			tags = append(tags, ownerTag)
		}
		if len(r.waiting) > 0 {
			tags = append(tags, textfmt.Plural(len(r.waiting), "wait"))
		}
		sub := ""
		if len(tags) > 0 {
			if narrow {
				sub = strings.Join(tags, " · ")
			} else {
				head += "  · " + strings.Join(tags, " · ")
			}
		}
		var body []string
		add := func(k, s string) {
			if s != "" {
				body = append(body, k+": "+s)
			}
		}
		add("details", r.details)
		add("done when", r.doneWhen)
		add("needs", strings.Join(r.needs, ", "))
		for _, w := range r.waiting {
			add("waiting", w)
		}
		add("replaces", r.replaces)
		add("why", r.why)
		add("account", r.account)
		if r.repeat {
			body = append([]string{"(already shown under another parent)"}, body...)
		}
		nodes = append(nodes, pageNode{head: head, sub: sub, body: strings.Join(body, "\n"), pre: true,
			err: r.status == "dropped" || r.status == "blocked", style: style})
		v.order = append(v.order, i)
		seen[r.id]++
		v.keys = append(v.keys, fmt.Sprintf("%s#%d", r.id, seen[r.id]))
	}
	// Finished trees, out of the way: one row at the end that opens to them
	// (USER 2026-11-05: "clear done trees and move them out"). The graph
	// keeps them; this is the view.
	if len(done) > 0 {
		trees := 0
		var body []string
		for _, i := range done {
			r := v.rows[i]
			if r.depth == 0 {
				trees++
			}
			line := strings.Repeat("  ", r.depth) + r.id + " " + planMark(r.status) + " " + r.title
			if n, ok := planAgentOf(r.owner); ok {
				line += fmt.Sprintf("  · agent %d", n)
			}
			body = append(body, line)
		}
		nodes = append(nodes, pageNode{head: fmt.Sprintf("≡ %s finished · %s", textfmt.Plural(trees, "tree"),
			textfmt.Plural(len(done), "node")), body: strings.Join(body, "\n"), pre: true, style: &StDim})
		v.order = append(v.order, -1)
		v.keys = append(v.keys, "≡")
	}
	if len(nodes) == 0 {
		nodes = append(nodes, pageNode{body: "The plan is empty. The model makes nodes with its plan tool when a task has several steps."})
	}
	return pageDoc{title: textfmt.Plural(v.total, "node"), nodes: nodes}
}

// segments splits the rows into trees (a root and the rows under it, up to
// the next root) and sorts them: the trees with work left, and the rows of the
// finished ones, where every node is done or dropped.
func (v *planView) segments() (live, done []int) {
	for i := 0; i < len(v.rows); {
		j := i + 1
		for j < len(v.rows) && v.rows[j].depth > 0 {
			j++
		}
		finished := true
		for k := i; k < j; k++ {
			if st := v.rows[k].status; st != "done" && st != "dropped" {
				finished = false
				break
			}
		}
		for k := i; k < j; k++ {
			if finished {
				done = append(done, k)
			} else {
				live = append(live, k)
			}
		}
		i = j
	}
	return live, done
}

// planSpin is a working owner's mark, one frame per pulse step.
var planSpin = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// planOwnerColors tell agents apart on /plan. None is green (the main agent's,
// and "it worked"), red (failed), orange (attention) or pink (asking, and
// where you are): those mean something already.
var planOwnerColors = []lipgloss.Style{
	lipgloss.NewStyle().Foreground(lipgloss.Color("33")),  // blue
	lipgloss.NewStyle().Foreground(lipgloss.Color("37")),  // teal
	lipgloss.NewStyle().Foreground(lipgloss.Color("135")), // purple
	lipgloss.NewStyle().Foreground(lipgloss.Color("130")), // brown
	lipgloss.NewStyle().Foreground(lipgloss.Color("63")),  // indigo
	lipgloss.NewStyle().Foreground(lipgloss.Color("66")),  // slate
}

// planAgentOf is the child a node's owner names ("agent:N").
func planAgentOf(owner string) (int, bool) {
	n, ok := strings.CutPrefix(owner, "agent:")
	if !ok {
		return 0, false
	}
	k, err := strconv.Atoi(n)
	return k, err == nil
}

// motion is a row's mark, colour and owner tag from who owns it and what they
// are doing: a working owner's open nodes spin; the main agent's node in
// progress shows ? while it asks you; an idle owner's are still. Colour says
// which agent; motion says what it is doing (USER 2026-11-05).
func (v *planView) motion(r planRow) (mark string, style *lipgloss.Style, tag string) {
	mark = planMark(r.status)
	open := r.status == "todo" || r.status == "doing"
	if n, ok := planAgentOf(r.owner); ok {
		st := planOwnerColors[(n-1+len(planOwnerColors))%len(planOwnerColors)]
		state := v.live.agents[n]
		if state == "" {
			state = "gone"
		}
		tag = fmt.Sprintf("agent %d %s", n, state)
		if state == "running" && open {
			mark, v.animating = planSpin[v.live.phase%len(planSpin)], true
			tag = fmt.Sprintf("agent %d working", n)
		}
		return mark, &st, tag
	}
	if r.status != "doing" {
		return mark, nil, ""
	}
	switch {
	case v.live.asking:
		v.animating = true
		st := stAsk
		if v.live.phase/4%2 == 1 {
			st = stNote
		}
		return "?", &st, "asking you"
	case v.live.busy:
		v.animating = true
		return planSpin[v.live.phase%len(planSpin)], nil, "working"
	}
	return mark, nil, ""
}

func (v *planView) hint() string {
	if v.width > 0 && v.width < narrowW {
		return "t tell · a approve · e m d edit/drop · enter dive/open · esc"
	}
	return hotKey("tell", "t") + StDim.Render(" · ") + hotKey("approve", "a") + StDim.Render(" · ") + hotKey("edit title", "e") +
		StDim.Render(" · ") + hotKey("details", "m") + StDim.Render(" · ") + hotKey("drop", "d") +
		StDim.Render(" · enter dive into its agent, else details · → details · esc back")
}

// openPlan asks the engine for the plan; the screen opens when it arrives.
func (m *tuiModel) openPlan() {
	m.planWant = true
	if !m.proc.controlCmd("plan", "view") {
		m.planWant = false
		m.append(StErr.Render("no engine right now — /reconnect first"))
	}
}

// planPaging is the /plan view the pager shows, if it shows one.
func (m *tuiModel) planPaging() *planView {
	if !m.paging {
		return nil
	}
	v, _ := m.pg.id.(*planView)
	return v
}

// notePlan takes a plan event: the one this TUI asked for opens the screen;
// an open screen refreshes.
func (m tuiModel) notePlan(ev evMsg) tuiModel {
	v := &planView{total: intOf(ev["total"]), done: intOf(ev["done"])}
	v.mode, _ = ev["mode"].(bool)
	v.approved, _ = ev["approved"].(bool)
	rows, _ := ev["rows"].([]any)
	for _, x := range rows {
		o, _ := x.(map[string]any)
		r := planRow{id: str(o["id"]), title: str(o["title"]), details: str(o["details"]), status: str(o["status"]), doneWhen: str(o["done_when"]),
			owner: str(o["owner"]), replaces: str(o["replaces"]), why: str(o["why"]), account: str(o["account"]),
			needs: strList(o["needs"]), waiting: strList(o["waiting"]), depth: intOf(o["depth"])}
		r.ready, _ = o["ready"].(bool)
		r.repeat, _ = o["repeat"].(bool)
		v.rows = append(v.rows, r)
	}
	if cur := m.planPaging(); cur != nil {
		v.width, v.keys = cur.width, cur.keys // keys: where the selection was (keepPlace)
		*cur = *v
		m.syncPager()
		return m
	}
	if !m.planWant {
		return m
	}
	m.planWant = false
	m.pg = newPager(-1, v)
	m.paging = true
	m.scr = planScreen()
	m.syncPager()
	return m
}

// planScreen is /plan: its status over the tree.
func planScreen() *paneScreen {
	return &paneScreen{
		panes: []screenPane{{body: viewStatusPane{}, fixed: true}, {body: planListPane{}, weight: 3, min: 4}},
		focus: 1,
		valid: func(m *tuiModel) bool { return m.planPaging() != nil },
	}
}

// planListPane is the pager showing the tree: a, e, g and d are the plan's,
// the rest are the pager's.
type planListPane struct{}

func (planListPane) view(m *tuiModel, w, h int) string { return m.pg.view(w, h) }

func (planListPane) key(m *tuiModel, msg tea.KeyPressMsg) bool {
	v := m.planPaging()
	if v == nil {
		return m.pg.key(msg.String())
	}
	k := msg.String()
	var r *planRow
	if m.pg.cur >= 0 && m.pg.cur < len(v.order) && v.order[m.pg.cur] >= 0 {
		r = &v.rows[v.order[m.pg.cur]]
	}
	switch k {
	case "a":
		if !m.proc.controlCmd("plan", "approve") {
			m.flashText, m.flashUntil = "no engine right now — /reconnect first", time.Now().Add(flashFor)
			return true
		}
		m.flashText, m.flashUntil = "/plan approve", time.Now().Add(flashFor)
		return true
	case "e", "m", "d":
		if r == nil {
			return true
		}
		line := "/plan drop " + r.id + " "
		switch k {
		case "e":
			line = "/plan edit " + r.id + " title " + r.title
		case "m":
			line = "/plan edit " + r.id + " details " + r.details
		}
		// The composer, filled in: the screen goes down and the person
		// changes the text and sends it.
		m.popScreen()
		m.input.SetValue(line)
		m.input.CursorEnd()
		return true
	case "t":
		if r != nil {
			m.startPlanTell(*r, v)
		}
		return true
	case "enter":
		if r != nil && m.planDive(*r) {
			return true
		}
	}
	return m.pg.key(k)
}

// planDive goes from a node into the session working it: a child's
// conversation (as the activity strip's descent does), or the main one for a
// node the main agent is working now. False when nobody is: enter then opens
// the node's details.
func (m *tuiModel) planDive(r planRow) bool {
	if n, ok := planAgentOf(r.owner); ok {
		if m.agentByID(n) == nil {
			return false
		}
		m.popScreen()
		m.enterAgentScope(n)
		return true
	}
	if r.status == "doing" && (m.busy || m.asking || m.askParked) {
		m.popScreen()
		if m.scopeAgent != 0 {
			m.leaveAgentScope()
		}
		return true
	}
	return false
}

// startPlanTell lends the composer to telling a node's owner something: the
// screen goes down so the composer shows, and sending (or esc) brings it back
// (USER 2026-11-05: talk to the plan; "this does mean coopting the text entry
// bar"). The owner changes the plan to match; it is not a direct edit.
func (m *tuiModel) startPlanTell(r planRow, v *planView) {
	who := "the main agent"
	if n, ok := planAgentOf(r.owner); ok {
		who = fmt.Sprintf("agent #%d", n)
	}
	m.planTelling = &planTellTo{id: r.id, title: r.title, who: who, placeholder: m.input.placeholder}
	m.popScreen()
	m.input.Reset()
	m.input.placeholder = fmt.Sprintf("tell %s about node %s · %s…", who, r.id, r.title)
	m.setFocus(focusInput)
	m.refresh()
}

// endPlanTell gives the composer back and opens /plan again; text, when not
// empty, is sent to the node's owner first.
func (m *tuiModel) endPlanTell(text string) {
	t := m.planTelling
	if t == nil {
		return
	}
	m.planTelling = nil
	m.input.placeholder = t.placeholder
	m.input.Reset()
	if text != "" {
		if !m.proc.controlCmd("plan", "tell "+t.id+" "+text) {
			m.append(StErr.Render("no engine right now — not sent"))
			m.refresh()
			return
		}
		m.flashText, m.flashUntil = "told "+t.who+" about node "+t.id, time.Now().Add(flashFor)
	}
	m.openPlan()
}

// keepPlace moves the pager's selection and open nodes from the page laid
// out with keys before to the same nodes in the page now: a tree finishing
// above the selection moves its rows to the end, and the next key must act
// on the node the person was on.
func (v *planView) keepPlace(p *pager, before []string) {
	if len(before) == 0 || slices.Equal(before, v.keys) {
		return
	}
	at := make(map[string]int, len(v.keys))
	for i, k := range v.keys {
		at[k] = i
	}
	if p.cur >= 0 && p.cur < len(before) {
		if i, ok := at[before[p.cur]]; ok {
			p.cur = i
		}
	}
	open := map[int]bool{}
	for i, o := range p.open {
		if o && i >= 0 && i < len(before) {
			if j, ok := at[before[i]]; ok {
				open[j] = true
			}
		}
	}
	p.open = open
}

// planLiveNow is the agents' state for /plan.
func (m tuiModel) planLiveNow() planLive {
	l := planLive{agents: map[int]string{}, busy: m.busy, asking: m.asking || m.askParked, phase: m.pulse}
	for _, a := range m.agents {
		l.agents[a.id] = a.state
	}
	return l
}

// planAnimates is whether /plan has a row in motion: the pulse runs for it.
func (m tuiModel) planAnimates() bool {
	v := m.planPaging()
	return v != nil && v.animating
}

func (planListPane) hint(m *tuiModel) string {
	if v := m.planPaging(); v != nil {
		return v.hint()
	}
	return ""
}
