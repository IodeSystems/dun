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
// Outline is the default: the engine's rows are in the graph's order (a node
// under each node it needs), and the screen reverses the edges for display
// from each row's needs. The finished-trees row is by connected component in
// outline and order, per node in flat. The engine sends the graph (control "plan" "view", then a plan event
// whenever it changes, whoever changed it); the screen never works out what
// can start, the engine says (Ready, Waiting).
//
//	a            approve the plan (plan mode: the model may change things)
//	e, m         edit the selected node's title, details: the composer takes
//	             the command, filled in, to change and send
//	d            drop the selected node, same way: a reason can be added
//	], [         next, previous row of the same node (it sits under several
//	             parents; every row of the selected node is marked ◆)
//	v            the view: outline (goals over what they need), order (the
//	             graph's own tree), flat (one row per node); not saved
//	c            on the finished row: /plan clear, archive the finished trees
//	s            /plan summary: the model says Done / Doing / Next (one side
//	             call, beside the turn); again, a fresh one. It shows as the
//	             first row, open, with when it was made and its cost
//	enter, →     the node's details, done-when, needs, why it waits
//
// Marking a node done is the model's, so there is no key for it. What the
// person changes reaches the model as a note (dun/plan.go).

type planRow struct {
	id, title, details, status, doneWhen, owner, replaces, why, account string
	// by: who wrote it when not the agent ("suggest": the idle suggestion
	// call recorded it from the last turn; shown "auto" beside the mark).
	by             string
	needs, waiting []string
	ready, repeat  bool
	depth          int
	// ask: the number of the ask the node serves (askfold.go), 0 for none.
	ask int
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
	// view is how the rows are laid out (planOutline, planOrder, planFlat),
	// kept across refreshes of one open screen.
	view int
	// sum is the last /plan summary, or the one being made; nil before one.
	sum *planSum
	// asks and intake: the asks ledger, listed above the plan (askfold.go).
	asks   []planAsk
	intake *planIntake
	now    func() time.Time // tests
}

// planSum is the plan event's summary (dun/plansummary.go).
type planSum struct {
	text, made, err string
	running         bool
}

// planSumKey is the summary row's key (keys), and planSumRow its order.
const (
	planSumKey = "summary"
	planSumRow = -2
)

const (
	planOutline = iota // goals at the root, what each needs beneath it
	planOrder          // the graph's own tree: a node under each node it needs
	planFlat           // one row per node
	planViews
)

var planViewNames = [planViews]string{"outline", "order", "flat"}

// planDisp is a row as shown: which of the engine's rows, how deep, and whether
// it only points at a node shown earlier.
type planDisp struct {
	i, depth int
	repeat   bool
}

// layout lays the rows out in the current view and splits them into the work
// that is left and the finished rows (folded away): a connected component with
// every node done or dropped, the same in every view and as /plan clear has it.
// units counts the finished trees.
func (v *planView) layout() (live, done []planDisp, units int) {
	first := map[string]int{} // id → its first row
	var uniq []int
	for i, r := range v.rows {
		if _, ok := first[r.id]; !ok {
			first[r.id] = i
			uniq = append(uniq, i)
		}
	}
	// Components over needs, either way.
	parent := map[string]string{}
	var find func(string) string
	find = func(x string) string {
		if parent[x] == "" || parent[x] == x {
			parent[x] = x
			return x
		}
		parent[x] = find(parent[x])
		return parent[x]
	}
	for _, i := range uniq {
		for _, n := range v.rows[i].needs {
			if _, ok := first[n]; ok {
				parent[find(v.rows[i].id)] = find(n)
			}
		}
	}
	finished := func(status string) bool { return status == "done" || status == "dropped" }
	open := map[string]bool{} // component root → has an open node
	for _, i := range uniq {
		if !finished(v.rows[i].status) {
			open[find(v.rows[i].id)] = true
		}
	}
	var all []planDisp
	switch v.view {
	case planOrder:
		for i, r := range v.rows {
			all = append(all, planDisp{i, r.depth, r.repeat})
		}
	case planFlat:
		for _, i := range uniq {
			all = append(all, planDisp{i: i})
		}
	default:
		needed := map[string]bool{}
		for _, i := range uniq {
			for _, n := range v.rows[i].needs {
				needed[n] = true
			}
		}
		shown := map[string]bool{}
		var walk func(i, depth int)
		walk = func(i, depth int) {
			id := v.rows[i].id
			if shown[id] {
				all = append(all, planDisp{i, depth, true})
				return
			}
			shown[id] = true
			all = append(all, planDisp{i, depth, false})
			for _, n := range v.rows[i].needs {
				if j, ok := first[n]; ok {
					walk(j, depth+1)
				}
			}
		}
		for _, i := range uniq {
			if !needed[v.rows[i].id] {
				walk(i, 0)
			}
		}
	}
	trees := map[string]bool{}
	for _, d := range all {
		r := v.rows[d.i]
		if !open[find(r.id)] {
			done = append(done, d)
			trees[find(r.id)] = true
		} else {
			live = append(live, d)
		}
	}
	return live, done, len(trees)
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

// planBy is the tag beside a node's mark for who wrote it: " auto" for one
// the idle suggestion call recorded, else "".
func planBy(r planRow) string {
	if r.by == "suggest" {
		return " auto"
	}
	return ""
}

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
	live, done, units := v.layout()
	nodes := make([]pageNode, 0, len(v.rows)+1)
	if s := v.sum; s != nil {
		head := "summary"
		switch {
		case s.running:
			head += " · summarizing…"
		case s.made != "":
			head += " · " + s.made
		}
		body := s.text
		if body == "" && s.running {
			body = "summarizing…"
		}
		if s.err != "" {
			body = strings.TrimSpace(body + "\n\nlast try failed: " + s.err)
		}
		nodes = append(nodes, pageNode{head: head, body: body, style: &stNote})
		v.order = append(v.order, planSumRow)
		v.keys = append(v.keys, planSumKey)
	}
	nodes = v.askNodes(nodes) // the asks, above the plan (askfold.go)
	info := v.occurrences(live)
	for _, d := range live {
		i, r := d.i, v.rows[d.i]
		in := info[r.id]
		mark, style, ownerTag := v.motion(r)
		indent := strings.Repeat("  ", d.depth)
		head := indent + r.id + " " + mark + planBy(r) + " " + r.title
		var tags []string
		if d.repeat {
			tags = append(tags, "again")
		}
		if in.ready {
			tags = append(tags, "ready")
		}
		if ownerTag != "" {
			tags = append(tags, ownerTag)
		}
		// The same on every occurrence of a node: all it waits on, and
		// every parent it sits under (USER 2026-11-18).
		if len(in.waiting) > 0 {
			tags = append(tags, fmt.Sprintf("waits %d", len(in.waiting)))
		}
		if r.ask > 0 {
			tags = append(tags, "ask "+strconv.Itoa(r.ask))
		}
		if in.n > 1 && len(in.parents) > 0 {
			tags = append(tags, "under "+strings.Join(in.parents, ", "))
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
		if len(in.parents) > 0 {
			add("under", strings.Join(in.parents, " · "))
		}
		for _, w := range in.waiting {
			add("waiting", v.blocker(w))
		}
		add("replaces", r.replaces)
		add("why", r.why)
		add("account", r.account)
		if d.repeat {
			body = append([]string{"(already shown under another parent)"}, body...)
		}
		nodes = append(nodes, pageNode{head: head, sub: sub, body: strings.Join(body, "\n"), pre: true, group: r.id,
			err: r.status == "dropped" || r.status == "blocked", style: style})
		v.order = append(v.order, i)
		seen[r.id]++
		v.keys = append(v.keys, fmt.Sprintf("%s#%d", r.id, seen[r.id]))
	}
	// Finished trees, out of the way: one row at the end that opens to them
	// (USER 2026-11-05: "clear done trees and move them out"). The graph
	// keeps them; this is the view.
	if len(done) > 0 {
		var body []string
		for _, d := range done {
			r := v.rows[d.i]
			line := strings.Repeat("  ", d.depth) + r.id + " " + planMark(r.status) + planBy(r) + " " + r.title
			if n, ok := planAgentOf(r.owner); ok {
				line += fmt.Sprintf("  · agent %d", n)
			}
			body = append(body, line)
		}
		ids := map[string]bool{} // a repeat is not another node
		for _, d := range done {
			ids[v.rows[d.i].id] = true
		}
		head := fmt.Sprintf("≡ %s finished · %s", textfmt.Plural(units, "tree"), textfmt.Plural(len(ids), "node"))
		nodes = append(nodes, pageNode{head: head, body: strings.Join(body, "\n"), pre: true, style: &StDim})
		v.order = append(v.order, -1)
		v.keys = append(v.keys, "≡")
	}
	if len(nodes) == 0 || len(nodes) == 1 && v.sum != nil {
		nodes = append(nodes, pageNode{body: "The plan is empty. The model makes nodes with its plan tools when a task has several steps."})
	}
	return pageDoc{title: textfmt.Plural(v.total, "node"), nodes: nodes}
}

// planInfo is what every occurrence of a node shows alike.
type planInfo struct {
	waiting, parents []string
	ready            bool
	n                int // rows it has on the page
}

// occurrences gathers, per node id, the engine's waiting lists (all rows of
// the node, joined), the nodes that need it (its parents, from the reversed
// needs) and how many rows show it. No readiness is worked out here.
func (v *planView) occurrences(live []planDisp) map[string]*planInfo {
	info := map[string]*planInfo{}
	get := func(id string) *planInfo {
		if info[id] == nil {
			info[id] = &planInfo{}
		}
		return info[id]
	}
	for _, r := range v.rows {
		in := get(r.id)
		for _, w := range r.waiting {
			if !slices.Contains(in.waiting, w) {
				in.waiting = append(in.waiting, w)
			}
		}
		in.ready = in.ready || r.ready
		for _, n := range r.needs {
			if pi := get(n); !slices.Contains(pi.parents, r.id) {
				pi.parents = append(pi.parents, r.id)
			}
		}
	}
	for _, d := range live {
		get(v.rows[d.i].id).n++
	}
	return info
}

// blocker is an engine's waiting line ("needs 2 (doing)") with the title of
// the node it names, when that node is on the plan.
func (v *planView) blocker(w string) string {
	f := strings.Fields(w)
	if len(f) >= 2 && f[0] == "needs" {
		for _, r := range v.rows {
			if r.id == f[1] {
				return w + " · " + r.title
			}
		}
	}
	return w
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
	name := "view: " + planViewNames[v.view]
	if v.width > 0 && v.width < narrowW {
		return "s summary · t tell · a approve · e m d edit/drop · [ ] same · v " + planViewNames[v.view] + " · enter · esc"
	}
	return StDim.Render(name+" ") + hotKey("summary", "s") + StDim.Render(" · ") + hotKey("change view", "v") + StDim.Render(" · ") + hotKey("tell", "t") + StDim.Render(" · ") + hotKey("approve", "a") + StDim.Render(" · ") + hotKey("edit title", "e") +
		StDim.Render(" · ") + hotKey("details", "m") + StDim.Render(" · ") + hotKey("drop", "d") + StDim.Render(" · ") + hotKey("same node", "[ ]") +
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
	if msg := str(ev["note"]); msg != "" { // auto-archive: said once, in the status line
		m.flashText, m.flashUntil = msg, time.Now().Add(flashFor)
	}
	if o, ok := ev["summary"].(map[string]any); ok {
		v.sum = &planSum{text: str(o["text"]), made: str(o["made"]), err: str(o["error"])}
		v.sum.running, _ = o["running"].(bool)
	}
	v.asks, v.intake = parseAsks(ev)
	rows, _ := ev["rows"].([]any)
	for _, x := range rows {
		o, _ := x.(map[string]any)
		r := planRow{id: str(o["id"]), title: str(o["title"]), details: str(o["details"]), status: str(o["status"]), doneWhen: str(o["done_when"]),
			owner: str(o["owner"]), replaces: str(o["replaces"]), why: str(o["why"]), account: str(o["account"]), by: str(o["by"]),
			needs: strList(o["needs"]), waiting: strList(o["waiting"]), depth: intOf(o["depth"])}
		r.ready, _ = o["ready"].(bool)
		r.repeat, _ = o["repeat"].(bool)
		r.ask = intOf(o["ask"])
		v.rows = append(v.rows, r)
	}
	if cur := m.planPaging(); cur != nil {
		v.width, v.keys, v.view = cur.width, cur.keys, cur.view // keys: where the selection was (keepPlace)
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
	m.placePlan(v)
	return m
}

// planScreen is /plan: its status over the tree.
func planScreen() *paneScreen {
	return &paneScreen{
		panes: []screenPane{{body: viewStatusPane{}, fixed: true}, {name: "plan", body: planListPane{}, weight: 3, min: 4}},
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
	case "s":
		if v.sum != nil && v.sum.running {
			m.flashText, m.flashUntil = "summarizing…", time.Now().Add(flashFor)
			return true
		}
		if !m.proc.controlCmd("plan", "summary") {
			m.flashText, m.flashUntil = "no engine right now — /reconnect first", time.Now().Add(flashFor)
			return true
		}
		m.flashText, m.flashUntil = "/plan summary", time.Now().Add(flashFor)
		return true
	case "]", "[":
		if r != nil {
			m.pg.jumpSame(r.id, k == "]")
		}
		return true
	case "v":
		v.view = (v.view + 1) % planViews
		m.syncPager()
		return true
	case "c":
		if m.pg.cur >= 0 && m.pg.cur < len(v.order) && v.order[m.pg.cur] == -1 {
			if !m.proc.controlCmd("plan", "clear") {
				m.flashText, m.flashUntil = "no engine right now — /reconnect first", time.Now().Add(flashFor)
				return true
			}
			m.flashText, m.flashUntil = "/plan clear", time.Now().Add(flashFor)
			return true
		}
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
		// changes the text and sends it. Sending it, or esc, brings /plan
		// back on the same place (planBack), as a tell does: dropping five
		// nodes is d, enter five times, not five reopens and walks back.
		m.planBack = &planBackTo{key: v.keys[m.pg.cur], cur: m.pg.cur, view: v.view}
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

// planBackTo is where /plan was when e, m or d lent it the composer: the
// selected node's key, its row and the view.
type planBackTo struct {
	key  string
	cur  int
	view int
}

// endPlanBack opens /plan again where planBack says, once the composer's
// line is sent (sent: what enter submitted) or put aside with esc. A line
// changed into something other than a /plan command is the person's own: it
// goes as typed and /plan stays down.
func (m *tuiModel) endPlanBack(sent string) {
	b := m.planBack
	if b == nil {
		return
	}
	m.planBack = nil
	if sent != "" && !strings.HasPrefix(sent, "/plan ") {
		return
	}
	m.planAt = b
	m.openPlan()
}

// placePlan puts a /plan just opened where planAt says: the same view, the
// same node, or the row it was on when that node moved (a drop that
// finished its tree folds it into the finished row).
func (m *tuiModel) placePlan(v *planView) {
	b := m.planAt
	m.planAt = nil
	if b == nil {
		return
	}
	if b.view != v.view {
		v.view = b.view
		m.syncPager()
	}
	if i := slices.Index(v.keys, b.key); i >= 0 {
		m.pg.cur = i
	} else if len(v.keys) > 0 {
		m.pg.cur = min(b.cur, len(v.keys)-1)
	}
	m.syncPager()
	m.pg.showCur()
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
