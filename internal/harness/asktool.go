package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/agentkit/taskgraph"
)

// asktool.go — the person's asks are acknowledged and marked (USER
// 2026-11-18: "there should be a single unacknowledged ask that grows as
// the user adds to it").
//
// A message the person types does not open an ask: it goes on the intake
// (asks.go intakeAdd), the one ask not yet acknowledged. "continue",
// "retry", a pasted list of four requests: each grows it. The model
// acknowledges it with the asks tool, at any point in the turn: it splits
// the intake into asks with short titles, merges it into the ask it
// continues, or closes it as done or filed at once. Each ask keeps the
// person's words (Source, Continued). The tool also rewords an ask and
// marks it done, filed with the plan nodes it became, or open; several
// asks in one call.
//
// The turn-end check (idlecheck.go) of a turn the person started names
// what is left: the intake, when it has messages, and each ask the turn
// touched (merged into, or a plan change under it) that the model did not
// mark in this turn. Nothing left, nothing said.
//
// Automatic rounds never grow the intake (they are not the person's). They
// may use the tool: acknowledging and closing only ever lower the open
// asks, which tightens the automatic-turn budget (autoturn.go); they are
// never asked to.

const asksToolName = "asks"

func asksToolDef() llm.ToolDef {
	piece := map[string]any{
		"title": map[string]any{"type": "string", "description": "a short title"},
		"text":  map[string]any{"type": "string", "description": "the part of the intake it is; default the whole intake"},
		"state": map[string]any{"type": "string", "enum": []string{"done", "filed", "open"}},
		"nodes": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "filed: the plan node ids"},
		"note":  map[string]any{"type": "string", "description": "done: what was done"},
	}
	item := map[string]any{
		"id":         map[string]any{"type": "string", "description": `"intake", or an ask's plan node id`},
		"split":      map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": piece}, "description": "intake: the asks it holds"},
		"merge_into": map[string]any{"type": "string", "description": "intake: the ask it continues"},
	}
	for k, v := range piece {
		item[k] = v
	}
	var td llm.ToolDef
	td.Type = "function"
	td.Function.Name = asksToolName
	td.Function.Description = "The person's messages collect in the intake, one ask not yet acknowledged. " +
		"Acknowledge it, and mark asks, in one call of several items, at any point in the turn. " +
		"Intake (id \"intake\"): split makes asks of it, each with a short title; merge_into adds it to the ask " +
		"it continues (continue, retry, a correction), reopening a closed one; state alone makes one ask of it in that state. " +
		"An ask (its plan node id): title rewords it; state done (note), filed (nodes: the plan nodes it became) or open. " +
		"The person's words are kept. Before a turn the person started ends, acknowledge the intake and mark each ask the turn worked."
	td.Function.Parameters = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"asks": map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": item, "required": []string{"id"}}},
		},
		"required": []string{"asks"},
	}
	return td
}

// askItem is one item of an asks call, and one piece of a split.
type askItem struct {
	ID        string    `json:"id"`
	Split     []askItem `json:"split"`
	MergeInto string    `json:"merge_into"`
	Title     string    `json:"title"`
	Text      string    `json:"text"`
	State     string    `json:"state"`
	Nodes     []string  `json:"nodes"`
	Note      string    `json:"note"`
}

func withAsks(inner agent.ToolDispatcher, h *Harness, onCall func(string, map[string]any, string)) agent.ToolDispatcher {
	return func(ctx context.Context, tc llm.ToolCall) (string, error) {
		if tc.Function.Name != asksToolName {
			return inner(ctx, tc)
		}
		out := h.asksCall(tc.Function.Arguments)
		if onCall != nil {
			var a map[string]any
			_ = json.Unmarshal([]byte(tc.Function.Arguments), &a)
			onCall(asksToolName, a, out)
		}
		return out, nil
	}
}

const asksUsage = `asks {asks: [{id, split?, merge_into?, text?, title?, state?, nodes?, note?}]}: id "intake" or an ask's node id`

// asksCall runs one asks call: each item in order, each one's result on its
// own line. The intake is acknowledged when an item on it succeeded; the
// messages that arrived during the call stay on it.
func (h *Harness) asksCall(raw string) string {
	var args struct {
		Asks []askItem `json:"asks"`
	}
	if err := json.Unmarshal([]byte(raw), &args); err != nil || len(args.Asks) == 0 {
		return "ERROR: " + asksUsage
	}
	turn := h.asksTurnNow()
	h.asks.mu.Lock()
	intake := slices.Clone(h.asksLocked().Intake)
	h.asks.mu.Unlock()
	whole := intakeText(intake)
	var lines []string
	acked, cur := false, ""
	note := func(r *askRec) {
		if cur == "" && r != nil && !askClosedState(h.askState(r)) {
			cur = r.Node
		}
	}
	for _, it := range args.Asks {
		id := strings.TrimSpace(it.ID)
		if id != askIntakeID {
			if len(it.Split) > 0 || it.MergeInto != "" {
				lines = append(lines, "ERROR: "+id+": split and merge_into are for the intake")
				continue
			}
			if it.Title == "" && it.State == "" {
				lines = append(lines, "ERROR: "+id+": say title or state")
				continue
			}
			r, line, err := h.askEdit(id, it, turn)
			if err != nil {
				lines = append(lines, "ERROR: "+id+": "+err.Error())
				continue
			}
			note(r)
			lines = append(lines, line)
			continue
		}
		if len(intake) == 0 {
			lines = append(lines, "intake: empty, nothing to acknowledge")
			continue
		}
		switch {
		case it.MergeInto != "":
			msgs := intake
			if t := strings.TrimSpace(it.Text); t != "" {
				msgs = []askMsg{{At: intake[len(intake)-1].At, Text: t}}
			}
			r, line, err := h.askMerge(strings.TrimSpace(it.MergeInto), msgs)
			if err != nil {
				lines = append(lines, "ERROR: intake: "+err.Error())
				continue
			}
			acked = true
			note(r)
			lines = append(lines, line)
		case len(it.Split) > 0 || it.State != "":
			pieces := it.Split
			if len(pieces) == 0 {
				pieces = []askItem{{Title: it.Title, Text: it.Text, State: it.State, Nodes: it.Nodes, Note: it.Note}}
			}
			for _, p := range pieces {
				if err := askStateOK(p); err != nil {
					lines = append(lines, "ERROR: intake: "+err.Error())
					continue
				}
				text := strings.TrimSpace(p.Text)
				if text == "" {
					text = whole
				}
				r, err := h.askAdd(p.Title, text, intake)
				if err != nil {
					lines = append(lines, "ERROR: intake: "+err.Error())
					continue
				}
				acked = true
				st := p.State
				if st == "" {
					st = "open"
				}
				p.Title = ""
				p.State = st
				if _, _, err := h.askEdit(r.Node, p, turn); err != nil {
					lines = append(lines, fmt.Sprintf("intake → ask %s %q, open: not marked %s: %v", r.Node, r.Title, st, err))
					note(r)
					continue
				}
				note(r)
				lines = append(lines, fmt.Sprintf("intake → ask %s %q · %s", r.Node, r.Title, st))
			}
		default:
			lines = append(lines, "ERROR: intake: say split, merge_into or state")
		}
	}
	if acked {
		h.asks.mu.Lock()
		d := h.asksLocked()
		if len(d.Intake) >= len(intake) {
			d.Intake = slices.Clone(d.Intake[len(intake):])
		}
		if len(d.Intake) == 0 {
			if h.asks.cur == askIntakeID || h.asks.cur == "" {
				h.asks.cur = cur
			}
		}
		h.asksSaveLocked()
		h.asks.mu.Unlock()
		h.planChanged()
	} else if cur != "" {
		h.asks.mu.Lock()
		if h.asks.cur != askIntakeID {
			h.asks.cur = cur
		}
		h.asks.mu.Unlock()
	}
	return strings.Join(lines, "\n")
}

// intakeText is the intake's messages as one text.
func intakeText(ms []askMsg) string {
	var parts []string
	for _, m := range ms {
		parts = append(parts, m.Text)
	}
	return strings.Join(parts, "\n\n")
}

func askStateOK(it askItem) error {
	switch it.State {
	case "", "open", "done":
	case "filed":
		if len(it.Nodes) == 0 {
			return errors.New("filed needs nodes: the plan node ids it became")
		}
	default:
		return fmt.Errorf("state %q: done, filed or open", it.State)
	}
	return nil
}

// askByNode is the record of the ask at node id. Under asks.mu.
func (h *Harness) askByNodeLocked(id string) *askRec {
	for _, r := range h.asksLocked().Asks {
		if r.Node == id {
			return r
		}
	}
	return nil
}

// askState is an ask's state as the screens show it: open, done, filed,
// dropped, from its node (the turn's end may not have read it yet).
func (h *Harness) askState(r *askRec) string {
	g := h.planNow().store.full()
	return askStateIn(g, r)
}

func askStateIn(g taskgraph.Graph, r *askRec) string {
	n := g.Get(r.Node)
	switch {
	case n == nil:
		return "gone"
	case n.Status == taskgraph.Done && len(r.Filed) > 0:
		return "filed"
	case n.Status == taskgraph.Done:
		return "done"
	case n.Status == taskgraph.Dropped:
		return "dropped"
	case n.Status == taskgraph.Blocked:
		return "blocked"
	}
	return "open"
}

func askClosedState(s string) bool { return s != "open" && s != "blocked" }

// askNodeTo moves an ask's node to doing (open) or done, from wherever it
// is: a closed one is reopened.
func askNodeTo(g *taskgraph.Graph, id string, to taskgraph.Status, why string) error {
	n := g.Get(id)
	if n == nil {
		return fmt.Errorf("no node %s", id)
	}
	if n.Status == to {
		if to == taskgraph.Done {
			n.Account = why
		}
		return nil
	}
	switch n.Status {
	case taskgraph.Done, taskgraph.Dropped, taskgraph.Blocked:
		if err := g.SetStatus(id, taskgraph.Todo, ""); err != nil {
			return err
		}
	}
	if n.Status == taskgraph.Todo {
		if err := g.SetStatus(id, taskgraph.Doing, ""); err != nil {
			return err
		}
	}
	if to == taskgraph.Done {
		return g.SetStatus(id, taskgraph.Done, why)
	}
	return nil
}

// askTouch notes an ask's node as the turn's (the ledger's attribution).
func (h *Harness) askTouch(node string) {
	h.ledger.mu.Lock()
	if h.ledger.open && !slices.Contains(h.ledger.touched, node) {
		h.ledger.touched = append(h.ledger.touched, node)
	}
	h.ledger.mu.Unlock()
}

// askReopenLocked makes a closed ask's record open again. Under asks.mu.
func askReopenLocked(r *askRec) {
	r.Outcome, r.Note, r.Closed, r.Ended, r.EndSnap = "", "", time.Time{}, false, false
}

// askEdit rewords ask id (it.Title) and marks it (it.State), in turn.
func (h *Harness) askEdit(id string, it askItem, turn int) (*askRec, string, error) {
	h.asks.mu.Lock()
	r := h.askByNodeLocked(id)
	h.asks.mu.Unlock()
	if r == nil {
		return nil, "", errors.New(`no such ask: asks are the plan's "ask:" nodes`)
	}
	if err := askStateOK(it); err != nil {
		return nil, "", err
	}
	title := headLine(strings.TrimSpace(it.Title), askTitleMax)
	why := strings.TrimSpace(it.Note)
	var nodes []string
	switch it.State {
	case "done":
		if why == "" {
			return nil, "", errors.New("done needs note: what was done")
		}
	case "filed":
		g := h.planNow().store.full()
		for _, n := range it.Nodes {
			n = strings.TrimSpace(n)
			if g.Get(n) == nil || n == id {
				return nil, "", fmt.Errorf("filed: no plan node %q", n)
			}
			nodes = append(nodes, n)
		}
		why = "filed → " + strings.Join(nodes, ", ")
	}
	err := planStore{h}.Update(context.Background(), func(g *taskgraph.Graph) error {
		if title != "" {
			t := "ask: " + title
			if err := g.Update(id, taskgraph.Patch{Title: &t}); err != nil {
				return err
			}
		}
		switch it.State {
		case "open":
			return askNodeTo(g, id, taskgraph.Doing, "")
		case "done", "filed":
			return askNodeTo(g, id, taskgraph.Done, why)
		}
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	h.asks.mu.Lock()
	if title != "" {
		r.Title = title
	}
	if it.State != "" {
		r.Marked = turn
		if r.Outcome != "" {
			askReopenLocked(r) // the turn's end writes its entry again, as it is now
		}
		r.Filed = nodes
	}
	h.asksSaveLocked()
	h.asks.mu.Unlock()
	h.askTouch(id)
	var parts []string
	if title != "" {
		parts = append(parts, fmt.Sprintf("titled %q", title))
	}
	if it.State != "" {
		w := it.State
		if it.State == "filed" {
			w = why
		}
		parts = append(parts, w)
	}
	return r, "ask " + id + ": " + strings.Join(parts, " · "), nil
}

// askMerge adds msgs to ask id, reopening it when it was closed: the
// person carried on with it.
func (h *Harness) askMerge(id string, msgs []askMsg) (*askRec, string, error) {
	h.asks.mu.Lock()
	r := h.askByNodeLocked(id)
	h.asks.mu.Unlock()
	if r == nil {
		return nil, "", fmt.Errorf("merge_into %s: no such ask", id)
	}
	was := h.askState(r)
	if was == "gone" {
		return nil, "", fmt.Errorf("merge_into %s: the ask's plan node is gone", id)
	}
	var add strings.Builder
	for _, m := range msgs {
		fmt.Fprintf(&add, "\n\ncontinued %s:\n%s", m.At.Local().Format("Jan 2 15:04"), m.Text)
	}
	err := planStore{h}.Update(context.Background(), func(g *taskgraph.Graph) error {
		n := g.Get(id)
		d := askCut(n.Details + add.String())
		if err := g.Update(id, taskgraph.Patch{Details: &d}); err != nil {
			return err
		}
		return askNodeTo(g, id, taskgraph.Doing, "")
	})
	if err != nil {
		return nil, "", err
	}
	h.asks.mu.Lock()
	r.Continued = append(r.Continued, msgs...)
	r.Marked, r.Filed = 0, nil // carried on: the turn's end asks for its state again
	if r.Outcome != "" {
		askReopenLocked(r)
	}
	h.asksSaveLocked()
	h.asks.mu.Unlock()
	h.askTouch(id)
	line := fmt.Sprintf("intake merged into ask %s %q", id, r.Title)
	if askClosedState(was) {
		line += ", reopened (it was " + was + ")"
	}
	return r, line, nil
}

// ── the turn-end check ──────────────────────────────────────────────────

// idleAskMsgQuote is how much of the intake's first message the check quotes.
const idleAskMsgQuote = 80

// idleAsks is the turn-end check's part on asks, in a turn the person
// started: the intake if it has messages, and the asks the turn touched
// that are open and not marked in it. "" when neither.
func (h *Harness) idleAsks() string {
	if !h.asksOn() {
		return ""
	}
	if o, _ := h.turnOriginNow(); o != originPerson {
		return ""
	}
	h.ledger.mu.Lock()
	touched := slices.Clone(h.ledger.touched)
	h.ledger.mu.Unlock()
	g := h.planNow().store.full()
	h.asks.mu.Lock()
	defer h.asks.mu.Unlock()
	d := h.asksLocked()
	var lines []string
	if n := len(d.Intake); n > 0 {
		line := fmt.Sprintf("- intake: %s, not acknowledged: %s", plurals(n, "message"), strconv.Quote(workClip(d.Intake[0].Text, idleAskMsgQuote)))
		if n > 1 {
			line += fmt.Sprintf(" +%d", n-1)
		}
		lines = append(lines, line)
	}
	byNode := map[string]*askRec{}
	for _, r := range d.Asks {
		byNode[r.Node] = r
	}
	isAsk := func(id string) bool { return byNode[id] != nil }
	var seen []string
	for _, id := range touched {
		for _, a := range askAncestors(g, id, isAsk) {
			r := byNode[a]
			if slices.Contains(seen, a) || r.Marked == d.Turn || askClosedState(askStateIn(g, r)) {
				continue
			}
			seen = append(seen, a)
			lines = append(lines, fmt.Sprintf("- %s ask: %s", a, r.Title))
		}
	}
	if len(lines) == 0 {
		return ""
	}
	var do []string
	if len(d.Intake) > 0 {
		do = append(do, "Acknowledge the intake: split, merge_into or state.")
	}
	if len(seen) > 0 {
		do = append(do, "Mark each ask done, filed with its nodes, or open.")
	}
	return "Asks (the asks tool):\n" + strings.Join(lines, "\n") + "\n" + strings.Join(do, " ")
}

// ── what the screens show ───────────────────────────────────────────────

// PlanAsk is an ask node's ask, for the plan screens.
type PlanAsk struct {
	N       int
	State   string // open, blocked, done, filed, dropped
	At      time.Time
	Working bool // the ask the turn works (only one, or the intake)
	Filed   []string
}

// PlanIntake is the intake for the plan screens.
type PlanIntake struct {
	Msgs    []PlanMsg
	Working bool
}

// PlanMsg is one message on the intake.
type PlanMsg struct {
	At   time.Time
	Text string
}

// asksView is the asks by node, and the intake (nil when empty).
func (h *Harness) asksView(g taskgraph.Graph) (map[string]*PlanAsk, *PlanIntake) {
	if h.isChild() {
		return nil, nil
	}
	h.asks.mu.Lock()
	defer h.asks.mu.Unlock()
	d := h.asksLocked()
	out := map[string]*PlanAsk{}
	for _, r := range d.Asks {
		st := askStateIn(g, r)
		out[r.Node] = &PlanAsk{N: r.N, State: st, At: r.At, Filed: slices.Clone(r.Filed),
			Working: h.asks.cur == r.Node && !askClosedState(st)}
	}
	var in *PlanIntake
	if len(d.Intake) > 0 {
		in = &PlanIntake{Working: h.asks.cur == askIntakeID}
		for _, m := range d.Intake {
			in.Msgs = append(in.Msgs, PlanMsg(m))
		}
	}
	return out, in
}

// intakeOpen reports whether the intake has messages: the person waits on
// an ask, for the automatic-turn budget (autoturn.go).
func (h *Harness) intakeOpen() bool {
	h.asks.mu.Lock()
	defer h.asks.mu.Unlock()
	return len(h.asksLocked().Intake) > 0
}

// asksFoldAsk is the ask a fold must keep (fold.go noteAsk) where asks are
// kept: the one the turn works with what was merged into it and the intake
// after it, or the intake. ok false where asks are not kept: the fold finds
// the ask in the conversation, as before.
func (h *Harness) asksFoldAsk() (foldAsk, bool) {
	if !h.asksOn() {
		return foldAsk{}, false
	}
	h.asks.mu.Lock()
	defer h.asks.mu.Unlock()
	d := h.asksLocked()
	var a foldAsk
	later := func(id string, ms []askMsg) {
		for i, m := range ms {
			a.later = append(a.later, askLater{id: id + "-" + strconv.Itoa(i), text: m.Text, at: m.At.UnixNano()})
		}
	}
	var r *askRec
	if h.asks.cur != askIntakeID {
		r = h.askByNodeLocked(h.asks.cur)
	}
	if r == nil && h.asks.cur != askIntakeID {
		for _, x := range d.Asks {
			if x.Outcome == "" {
				r = x // the newest open
			}
		}
	}
	switch {
	case r != nil:
		a = foldAsk{id: "ask-" + r.Node, text: r.Text, at: r.At.UnixNano()}
		later(a.id, r.Continued)
		later("intake", d.Intake)
	case len(d.Intake) > 0:
		a = foldAsk{id: "intake", text: d.Intake[0].Text, at: d.Intake[0].At.UnixNano()}
		later("intake", d.Intake[1:])
	}
	return a, true
}
