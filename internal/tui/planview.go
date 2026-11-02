package tui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

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
//	e, g         edit the selected node's title, goal: the composer takes
//	             the command, filled in, to change and send
//	d            drop the selected node, same way: a reason can be added
//	enter, →     the node's goal, done-when, needs, files, why it waits
//
// Marking a node done is the model's, so there is no key for it. What the
// person changes reaches the model as a note (dun/plan.go).

type planRow struct {
	id, title, goal, status, doneWhen, owner, replaces, notes, why, account string
	needs, files, waiting                                                   []string
	ready, repeat                                                           bool
	depth                                                                   int
}

type planView struct {
	rows           []planRow
	mode, approved bool
	total, done    int
	width          int
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
	nodes := make([]pageNode, 0, len(v.rows))
	for _, r := range v.rows {
		indent := strings.Repeat("  ", r.depth)
		head := indent + r.id + " " + planMark(r.status) + " " + r.title
		var tags []string
		if r.repeat {
			tags = append(tags, "again")
		}
		if r.ready {
			tags = append(tags, "ready")
		}
		if r.owner != "" {
			tags = append(tags, "agent "+r.owner)
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
		add("goal", r.goal)
		add("done when", r.doneWhen)
		add("needs", strings.Join(r.needs, ", "))
		for _, w := range r.waiting {
			add("waiting", w)
		}
		add("files", strings.Join(r.files, ", "))
		add("replaces", r.replaces)
		add("why", r.why)
		add("account", r.account)
		add("notes", r.notes)
		if r.repeat {
			body = append([]string{"(already shown under another parent)"}, body...)
		}
		nodes = append(nodes, pageNode{head: head, sub: sub, body: strings.Join(body, "\n"), pre: true, err: r.status == "dropped" || r.status == "blocked"})
	}
	if len(nodes) == 0 {
		nodes = append(nodes, pageNode{body: "The plan is empty. The model makes nodes with its plan tool when a task has several steps."})
	}
	return pageDoc{title: textfmt.Plural(v.total, "node"), nodes: nodes}
}

func (v *planView) hint() string {
	if v.width > 0 && v.width < narrowW {
		return "a approve · e g d edit/drop · enter open · esc"
	}
	return hotKey("approve", "a") + StDim.Render(" · ") + hotKey("edit title", "e") + StDim.Render(" · ") + hotKey("goal", "g") +
		StDim.Render(" · ") + hotKey("drop", "d") + StDim.Render(" · enter details · esc back")
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
		r := planRow{id: str(o["id"]), title: str(o["title"]), goal: str(o["goal"]), status: str(o["status"]), doneWhen: str(o["done_when"]),
			owner: str(o["owner"]), replaces: str(o["replaces"]), notes: str(o["notes"]), why: str(o["why"]), account: str(o["account"]),
			needs: strList(o["needs"]), files: strList(o["files"]), waiting: strList(o["waiting"]), depth: intOf(o["depth"])}
		r.ready, _ = o["ready"].(bool)
		r.repeat, _ = o["repeat"].(bool)
		v.rows = append(v.rows, r)
	}
	if cur := m.planPaging(); cur != nil {
		v.width = cur.width
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
	switch k {
	case "a":
		if !m.proc.controlCmd("plan", "approve") {
			m.flashText, m.flashUntil = "no engine right now — /reconnect first", time.Now().Add(flashFor)
			return true
		}
		m.flashText, m.flashUntil = "/plan approve", time.Now().Add(flashFor)
		return true
	case "e", "g", "d":
		if m.pg.cur < 0 || m.pg.cur >= len(v.rows) {
			return true
		}
		r := v.rows[m.pg.cur]
		line := "/plan drop " + r.id + " "
		switch k {
		case "e":
			line = "/plan edit " + r.id + " title " + r.title
		case "g":
			line = "/plan edit " + r.id + " goal " + r.goal
		}
		// The composer, filled in: the screen goes down and the person
		// changes the text and sends it.
		m.popScreen()
		m.input.SetValue(line)
		m.input.CursorEnd()
		return true
	}
	return m.pg.key(k)
}

func (planListPane) hint(m *tuiModel) string {
	if v := m.planPaging(); v != nil {
		return v.hint()
	}
	return ""
}
