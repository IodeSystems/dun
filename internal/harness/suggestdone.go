package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/iodesystems/agentkit/taskgraph"
)

// Idle plans (USER 2026-11-17; docs/taskplan.md, "Idle plans"): the idle
// suggestion call (suggest.go) also says what the last turn finished that the
// plan does not record yet, and those become DONE plan nodes. The same request,
// no second call: the instruction grows a "done" part and the plan as text,
// after the history, so the cached prefix is the turn's as before.
//
//   - A node it adds is marked as the suggester's (planState.by, planBySuggest):
//     /plan and the plan event show it ("auto").
//   - It goes under the node the model names (needs: [that id], the graph's own
//     "under", as a child adds under its node), else at the top level.
//   - It only adds. It never closes, edits or reorders a node, and adds nothing
//     when the turn changed the plan (planMemStore.rev, noted as each turn
//     begins), when the plan is empty and the turn made no tool call, or when
//     the call is off (--no-suggest: emitSuggestions never calls Suggestions).
//   - At most suggestDoneMax nodes per call, titles cut to suggestDoneTitleMax
//     runes, none whose title the plan already has.
//   - A malformed done part adds nothing; the suggestions still parse.

// planBySuggest is planState.by's word for a node the idle suggestion call wrote.
const planBySuggest = "suggest"

const (
	suggestDoneMax      = 3
	suggestDoneTitleMax = 60
	// suggestDonePlanRows bounds the plan shown to the call.
	suggestDonePlanRows = 40
)

// planTurnMark is the plan and the tool-call count as a turn began.
type planTurnMark struct {
	seq   int // counts marks: a turn that began since a check moves it
	store *planMemStore
	rev   int
	calls uint64
}

// planMarkTurn notes the plan's revision and the tool calls so far, as a turn
// begins (prepareTurn). A child's turn does not: only the root suggests.
func (h *Harness) planMarkTurn() {
	if h.isChild() {
		return
	}
	p := h.planNow()
	rev := p.store.revNow()
	var calls uint64
	if h.store != nil {
		calls = h.store.toolCallsNow()
	}
	h.planMu.Lock()
	h.planTurn = planTurnMark{seq: h.planTurn.seq + 1, store: p.store, rev: rev, calls: calls}
	h.planMu.Unlock()
}

// suggestDoneScope is whether this suggestion call may add done nodes, with the
// mark it checked and the plan as shown to the model. No turn this process
// ran (a resumed session, idle) is no: there is no last turn to read.
func (h *Harness) suggestDoneScope(ctx context.Context) (planTurnMark, taskgraph.Graph, bool) {
	if h.isChild() {
		return planTurnMark{}, taskgraph.Graph{}, false
	}
	p := h.planNow()
	h.planMu.Lock()
	m := h.planTurn
	h.planMu.Unlock()
	if m.seq == 0 || m.store != p.store || p.store.revNow() != m.rev {
		return m, taskgraph.Graph{}, false
	}
	g, err := p.store.View(ctx)
	if err != nil {
		return m, taskgraph.Graph{}, false
	}
	work := 0 // the nodes that are not asks (asks.go): one for each message
	for _, n := range g.Nodes {
		if !h.isAskNode(n.ID) {
			work++
		}
	}
	if work == 0 && (h.store == nil || h.store.toolCallsNow() == m.calls) {
		return m, taskgraph.Graph{}, false // a plain chat: nothing to record
	}
	return m, g, true
}

// suggestDoneInstruction is the done part of the instruction, with the plan.
func suggestDoneInstruction(g taskgraph.Graph) string {
	var b strings.Builder
	b.WriteString(`

ALSO list the work the LAST turn actually finished that the plan below does not record yet, as "done" in the same JSON object, after "suggestions":
{"suggestions":[…],"done":[{"title":"…","under":"…"}]}
- title: what was finished, under ` + strconv.Itoa(suggestDoneTitleMax) + ` characters.
- under: the id of the plan node it belongs under, or "" for none.
- At most ` + strconv.Itoa(suggestDoneMax) + `. Only finished work; nothing the plan already has. "done":[] when there is none.
The plan:`)
	if len(g.Nodes) == 0 {
		b.WriteString("\n(empty)")
		return b.String()
	}
	rows := g.Tree()
	shown := 0
	for _, r := range rows {
		if r.Repeat {
			continue
		}
		if shown == suggestDonePlanRows {
			fmt.Fprintf(&b, "\n… and more")
			break
		}
		b.WriteString("\n" + strings.Repeat("  ", r.Depth) + g.Line(r.Node))
		shown++
	}
	return b.String()
}

// suggestDone is one item of the reply's done part.
type suggestDone struct {
	Title, Under string
}

// parseSuggestDone reads the reply's done part; anything malformed is nil.
func parseSuggestDone(s string) []suggestDone {
	i := strings.IndexByte(s, '{')
	j := strings.LastIndexByte(s, '}')
	if i < 0 || j <= i {
		return nil
	}
	var out struct {
		Done []struct {
			Title string          `json:"title"`
			Under json.RawMessage `json:"under"`
		} `json:"done"`
	}
	if json.Unmarshal([]byte(s[i:j+1]), &out) != nil {
		return nil
	}
	var res []suggestDone
	for _, d := range out.Done {
		var under string
		if json.Unmarshal(d.Under, &under) != nil {
			var n json.Number // "under": 3
			if json.Unmarshal(d.Under, &n) == nil {
				under = n.String()
			}
		}
		res = append(res, suggestDone{Title: d.Title, Under: strings.TrimSpace(under)})
	}
	return res
}

// doneKey finds the done part, so a reply whose done part is broken JSON can be
// read without it (parseSuggestions).
var doneKey = regexp.MustCompile(`,\s*"done"\s*:`)

// withoutDone is s with everything from the done key on cut, and the object
// closed: the suggestions of a reply whose done part is malformed. ok false: s
// has no done key.
func withoutDone(s string) (string, bool) {
	loc := doneKey.FindStringIndex(s)
	if loc == nil {
		return "", false
	}
	return s[:loc[0]] + "}", true
}

var errNothingDone = errors.New("nothing to add")

// addSuggestDone adds the done nodes, if the plan is still as m found it. It
// returns the ids it added.
func (h *Harness) addSuggestDone(ctx context.Context, m planTurnMark, items []suggestDone) []string {
	if len(items) == 0 {
		return nil
	}
	p := h.planNow()
	if p.store != m.store {
		return nil
	}
	h.planMu.Lock()
	prev := p.by
	h.planMu.Unlock()
	var ids []string
	err := p.store.Update(ctx, func(g *taskgraph.Graph) error {
		ids = nil
		h.planMu.Lock()
		defer h.planMu.Unlock()
		// A turn began, or the plan moved, during the call: what the model
		// read is not the plan now. (Update holds the store's lock: rev is read
		// directly.)
		if h.planTurn.seq != m.seq || p.store.rev != m.rev {
			return errNothingDone
		}
		shown := g.Clone()
		hideArchived(&shown, p.archived)
		have := map[string]bool{}
		for _, n := range g.Nodes {
			have[strings.ToLower(n.Title)] = true
		}
		by := map[string]string{}
		for k, v := range prev {
			by[k] = v
		}
		// The turn's ledger entry is the ask (or turn) entry that turn's
		// facts went to (asks.go): the node names the turn.
		account := fmt.Sprintf("recorded after turn %d by the idle suggestion call", h.asksTurnNow())
		for _, it := range items {
			if len(ids) == suggestDoneMax {
				break
			}
			title := taskgraph.Clip(strings.Join(strings.Fields(it.Title), " "), suggestDoneTitleMax)
			key := strings.ToLower(title)
			if title == "" || have[key] {
				continue
			}
			have[key] = true
			var needs []string
			if it.Under != "" && shown.Get(it.Under) != nil {
				needs = []string{it.Under}
			}
			g.Seq++
			id := strconv.Itoa(g.Seq)
			g.Nodes = append(g.Nodes, &taskgraph.Node{ID: id, Title: title, Status: taskgraph.Done, Needs: needs,
				DoneWhen: "the last turn did it", Account: account})
			by[id] = planBySuggest
			ids = append(ids, id)
		}
		if len(ids) == 0 {
			return errNothingDone
		}
		p.by = by // savePlan, after this returns, writes it with the graph
		return nil
	})
	if err != nil {
		h.planMu.Lock()
		p.by = prev // a failed save: memory keeps what the file has
		h.planMu.Unlock()
		return nil
	}
	h.planChanged()
	return ids
}

// planListBy marks the suggester's nodes in Graph.List's text: "5 [done]
// (auto) Title". List is agentkit's and knows nothing of who wrote a node.
func planListBy(list string, by map[string]string) string {
	if len(by) == 0 {
		return list
	}
	lines := strings.Split(list, "\n")
	for i, l := range lines {
		t := strings.TrimLeft(l, " ")
		id, rest, ok := strings.Cut(t, " ")
		if !ok || by[id] != planBySuggest || !strings.HasPrefix(rest, "[") {
			continue
		}
		if k := strings.Index(rest, "] "); k > 0 {
			lines[i] = l[:len(l)-len(t)] + id + " " + rest[:k+2] + "(auto) " + rest[k+2:]
		}
	}
	return strings.Join(lines, "\n")
}
