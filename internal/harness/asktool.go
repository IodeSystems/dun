package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"

	"github.com/iodesystems/dun/internal/numfmt"
)

// asktool.go — the person's asks are acknowledged and marked (USER
// 2026-11-18: "there should be a single unacknowledged ask that grows as
// the user adds to it"; asks are a ledger, pinned in the prompt, not plan
// nodes).
//
// A message the person types goes on the intake (asks.go intakeAdd).
// "continue", "retry", a pasted list of four requests: each grows it. The
// model acknowledges it with the asks tool, at any point in the turn: it
// splits the intake into asks with short titles, merges it into the ask it
// continues, or makes one ask of it in a state at once. The person's words
// always land in the ledger (Source, Continued). The tool also rewords an
// ask, links plan nodes to it, and marks it done, filed (the plan nodes it
// became), blocked (the question) or open; several asks in one call.
//
// The model reads the intake and the open asks in one block (asksBlock):
// put in the conversation as each turn begins (an aside keyed "asks",
// replaced, like the plan's), and in what a fold is told and the note
// after it (fold.go), so a fold never loses them. One sentence of the
// system prompt says what to do with them (systemAsks).
//
// The turn-end check (idlecheck.go) of a turn the person started names
// what is left: the intake, the asks the turn touched and did not mark,
// and, once, an open ask untouched for askStaleTurns of the person's
// turns. Nothing left, nothing said.
//
// Automatic rounds never grow the intake (they are not the person's). They
// may use the tool, unprompted: acknowledging and closing only ever lower
// the open asks, which tightens the automatic-turn budget (autoturn.go).

const asksToolName = "asks"

// asksWanted: a session that keeps asks, but for the tools (asksOn adds
// that the asks tool is in the set): the root of an interactive session.
func (h *Harness) asksWanted() bool {
	return !h.isChild() && !h.cfg.NonInteractive && !h.cfg.NoAsks
}

// Caps on what the block and a fold show; the ledger keeps the rest.
const (
	askBlockIntake  = 5   // the intake's newest messages
	askBlockQuote   = 200 // runes of each
	askBlockAsks    = 12  // open asks listed
	askFoldLater    = 3   // the newest merged messages a fold keeps verbatim
	idleAskMsgQuote = 80
)

// systemAsks is the system prompt's line on asks (USER 2026-11-18: "keep
// track of delayed user asks with the plan tool").
const systemAsks = "\n\nThe person's messages collect in the intake: acknowledge it with asks (split, merge_into, or a state), " +
	"mark each ask done as you finish it, and one you will not finish this turn (deferred, waiting, a list of later items) " +
	"file as plan_add nodes and mark it filed with them, so nothing asked is lost."

// asksToolDef is the tool's schema, flat (docs/decisions.md "The person's
// message ends the prompt"): ids are integers, 0 the intake; split is the
// new asks' titles, not objects. No union types and no nested objects, which
// Gemini's function schema takes least well.
func asksToolDef() llm.ToolDef {
	str := map[string]any{"type": "string"}
	num := map[string]any{"type": "integer"}
	item := map[string]any{
		"id":         num,
		"split":      map[string]any{"type": "array", "items": str},
		"merge_into": num,
		"title":      str,
		"text":       str,
		"state":      map[string]any{"type": "string", "enum": []string{"open", "done", "filed", "blocked"}},
		"nodes":      map[string]any{"type": "array", "items": str},
		"note":       str,
	}
	var td llm.ToolDef
	td.Type = "function"
	td.Function.Name = asksToolName
	td.Function.Description = `Acknowledge the intake (id 0: split into asks by title, merge_into an ask's number, or a state) and ` +
		`mark asks (id: its number): title, state (done/blocked: note; filed: nodes), nodes links plan nodes.`
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
	ID        json.RawMessage `json:"id"`
	Split     []askPiece      `json:"split"`
	MergeInto json.RawMessage `json:"merge_into"`
	Title     string          `json:"title"`
	Text      string          `json:"text"`
	State     string          `json:"state"`
	Nodes     []string        `json:"nodes"`
	Note      string          `json:"note"`
}

// askPiece is one piece of a split: a title, as the schema says, or an
// object {title, text, state, nodes, note}, the shape before 2026-11-19,
// still read. A title takes the item's state, nodes and note.
type askPiece struct {
	askItem
	titleOnly bool
}

func (p *askPiece) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		p.Title, p.titleOnly = s, true
		return nil
	}
	return json.Unmarshal(b, &p.askItem)
}

// askID reads an id: 0 or "intake" for the intake, or an ask's number as
// 3, "3" or "#3". 0 and false for intake; 0 and true for none.
func askID(raw json.RawMessage) (n int, intake bool, ok bool) {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		var f float64
		if json.Unmarshal(raw, &f) != nil {
			return 0, false, false
		}
		if f == 0 {
			return 0, true, true
		}
		return int(f), false, int(f) > 0
	}
	s = strings.TrimPrefix(strings.TrimSpace(s), "#")
	if s == askIntakeID {
		return 0, true, true
	}
	n, err := strconv.Atoi(strings.TrimPrefix(s, "ask "))
	return n, false, err == nil && n > 0
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

const asksUsage = `asks {asks: [{id, split?, merge_into?, text?, title?, state?, nodes?, note?}]}: id 0 for the intake or an ask's number; split: the new asks' titles`

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
	h.asksMigrate()
	turn := h.asksTurnNow()
	h.asks.mu.Lock()
	intake := slices.Clone(h.asksLocked().Intake)
	h.asks.mu.Unlock()
	whole := intakeText(intake)
	var lines []string
	acked, cur := false, ""
	note := func(r *askRec) {
		if cur == "" && r != nil && r.Outcome == "" {
			cur = strconv.Itoa(r.N)
		}
	}
	for _, it := range args.Asks {
		n, isIntake, ok := askID(it.ID)
		if !ok {
			lines = append(lines, "ERROR: id "+string(it.ID)+`: 0 for the intake or an ask's number`)
			continue
		}
		if !isIntake {
			name := "ask " + strconv.Itoa(n)
			if len(it.Split) > 0 || len(it.MergeInto) > 0 {
				lines = append(lines, "ERROR: "+name+": split and merge_into are for the intake")
				continue
			}
			if it.Title == "" && it.State == "" && len(it.Nodes) == 0 {
				lines = append(lines, "ERROR: "+name+": say title, state or nodes")
				continue
			}
			r, line, err := h.askEdit(n, it, turn)
			if err != nil {
				lines = append(lines, "ERROR: "+name+": "+err.Error())
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
		case len(it.MergeInto) > 0:
			to, into, ok := askID(it.MergeInto)
			if !ok || into {
				lines = append(lines, "ERROR: intake: merge_into takes an ask's number")
				continue
			}
			// The person's words always; text is the model's note on them.
			msgs := slices.Clone(intake)
			msgs[len(msgs)-1].Note = strings.TrimSpace(it.Text)
			r, line, err := h.askMerge(to, msgs)
			if err != nil {
				lines = append(lines, "ERROR: intake: "+err.Error())
				continue
			}
			acked = true
			note(r)
			lines = append(lines, line)
		case len(it.Split) > 0 || it.State != "":
			var pieces []askItem
			for _, p := range it.Split {
				if p.titleOnly {
					p.askItem = askItem{Title: p.Title, State: it.State, Nodes: it.Nodes, Note: it.Note}
				}
				pieces = append(pieces, p.askItem)
			}
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
				if p.State == "" {
					p.State = "open"
				}
				p.Title = ""
				line, err := "", error(nil)
				if _, line, err = h.askEdit(r.N, p, turn); err != nil {
					line = fmt.Sprintf("intake → ask %d %q, open: not marked %s: %v", r.N, r.Title, p.State, err)
				} else {
					line = fmt.Sprintf("intake → ask %d %q · %s", r.N, r.Title, strings.TrimPrefix(line, fmt.Sprintf("ask %d: ", r.N)))
				}
				note(r)
				lines = append(lines, line)
			}
		default:
			lines = append(lines, "ERROR: intake: say split, merge_into or state")
		}
	}
	h.asks.mu.Lock()
	if acked {
		d := h.asksLocked()
		if len(d.Intake) >= len(intake) {
			d.Intake = slices.Clone(d.Intake[len(intake):])
		}
		if len(d.Intake) == 0 && (h.asks.cur == askIntakeID || h.asks.cur == "") {
			h.asks.cur = cur
		}
		h.asksSaveLocked()
	} else if cur != "" && h.asks.cur != askIntakeID {
		h.asks.cur = cur
	}
	h.asks.mu.Unlock()
	h.planChanged()
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
	case "", "open", "done", "blocked":
	case "filed":
		if len(it.Nodes) == 0 {
			return errors.New("filed needs nodes: the plan node ids it became")
		}
	default:
		return fmt.Errorf("state %q: open, done, filed or blocked", it.State)
	}
	return nil
}

// askByNLocked is ask n's record. Under asks.mu.
func (h *Harness) askByNLocked(n int) *askRec {
	for _, r := range h.asksLocked().Asks {
		if r.N == n {
			return r
		}
	}
	return nil
}

// askStateOf is an ask's state as the screens show it.
func askStateOf(r *askRec) string {
	switch {
	case r.Outcome != "":
		return r.Outcome
	case r.Blocked != "":
		return "blocked"
	}
	return "open"
}

// askTouch notes an ask as the turn's (the ledger's attribution).
func (h *Harness) askTouch(n int) {
	id := askTouchPrefix + strconv.Itoa(n)
	h.ledger.mu.Lock()
	if h.ledger.open && !slices.Contains(h.ledger.touched, id) {
		h.ledger.touched = append(h.ledger.touched, id)
	}
	h.ledger.mu.Unlock()
}

// askReopenLocked makes a closed ask open again. Under asks.mu.
func askReopenLocked(r *askRec) {
	r.Outcome, r.Note, r.Closed, r.Ended, r.EndSnap, r.Journaled = "", "", time.Time{}, false, false, false
}

// askEdit rewords ask n (it.Title), links plan nodes to it (it.Nodes) and
// marks it (it.State), in turn.
func (h *Harness) askEdit(n int, it askItem, turn int) (*askRec, string, error) {
	if err := askStateOK(it); err != nil {
		return nil, "", err
	}
	var nodes []string
	if len(it.Nodes) > 0 {
		g := h.planNow().store.full()
		for _, id := range it.Nodes {
			id = strings.TrimSpace(id)
			if g.Get(id) == nil {
				return nil, "", fmt.Errorf("nodes: no plan node %q", id)
			}
			nodes = append(nodes, id)
		}
	}
	why := strings.TrimSpace(it.Note)
	switch it.State {
	case "done", "blocked":
		if why == "" {
			return nil, "", fmt.Errorf("%s needs note: %s", it.State, map[string]string{"done": "what was done", "blocked": "the question"}[it.State])
		}
	case "filed":
		why = "filed → " + strings.Join(nodes, ", ")
	}
	title := headLine(strings.TrimSpace(it.Title), askTitleMax)
	h.asks.mu.Lock()
	r := h.askByNLocked(n)
	if r == nil {
		h.asks.mu.Unlock()
		return nil, "", errors.New("no such ask")
	}
	d := h.asksLocked()
	var parts []string
	if title != "" {
		r.Title = title
		parts = append(parts, fmt.Sprintf("titled %q", title))
	}
	for _, id := range nodes {
		if !slices.Contains(r.Nodes, id) {
			r.Nodes = append(r.Nodes, id)
		}
	}
	if len(nodes) > 0 && it.State != "filed" {
		parts = append(parts, "serves "+strings.Join(nodes, ", "))
	}
	if it.State != "" {
		if r.Outcome != "" {
			askReopenLocked(r) // the turn's end writes its entry again, as it is now
		}
		r.Blocked = ""
		switch it.State {
		case "done", "filed":
			r.Outcome, r.Note, r.Closed = it.State, why, time.Now()
		case "blocked":
			r.Blocked = why
		}
		parts = append(parts, map[bool]string{true: why, false: it.State}[it.State == "filed"])
		r.Marked = turn
	}
	r.Touched, r.StaleSaid = d.PersonTurns, false
	h.asksSaveLocked()
	h.asks.mu.Unlock()
	h.askTouch(n)
	return r, fmt.Sprintf("ask %d: %s", n, strings.Join(parts, " · ")), nil
}

// askMerge adds msgs to ask n, reopening it when it was closed: the person
// carried on with it.
func (h *Harness) askMerge(n int, msgs []askMsg) (*askRec, string, error) {
	h.asks.mu.Lock()
	r := h.askByNLocked(n)
	if r == nil {
		h.asks.mu.Unlock()
		return nil, "", fmt.Errorf("merge_into %d: no such ask", n)
	}
	was := askStateOf(r)
	r.Continued = append(r.Continued, msgs...)
	r.Marked = 0 // carried on: the turn's end asks for its state again
	if r.Outcome != "" {
		askReopenLocked(r)
	}
	r.Touched, r.StaleSaid = h.asksLocked().PersonTurns, false
	h.asksSaveLocked()
	h.asks.mu.Unlock()
	h.askTouch(n)
	line := fmt.Sprintf("intake merged into ask %d %q", n, r.Title)
	if was == "done" || was == "filed" || was == "dropped" {
		line += ", reopened (it was " + was + ")"
	}
	return r, line, nil
}

// ── the turn-end check ──────────────────────────────────────────────────

// idleAsks is the turn-end check's part on asks, in a turn the person
// started: the intake if it has messages, the asks the turn touched that
// are open and not marked in it, and once each, an open ask untouched for
// askStaleTurns of the person's turns. "" when there is none.
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
	var lines, do []string
	if n := len(d.Intake); n > 0 {
		line := fmt.Sprintf("- intake: %s, not acknowledged: %s", numfmt.Plural(n, "message", "messages"), strconv.Quote(workClip(d.Intake[0].Text, idleAskMsgQuote)))
		if n > 1 {
			line += fmt.Sprintf(" +%d", n-1)
		}
		lines = append(lines, line)
		do = append(do, "Acknowledge the intake: split, merge_into or state.")
	}
	named := 0
	for _, r := range asksNamed(g, touched, d.Asks) {
		if r.Outcome != "" || r.Marked == d.Turn {
			continue
		}
		named++
		lines = append(lines, fmt.Sprintf("- %d %s", r.N, r.Title))
	}
	if named > 0 {
		do = append(do, "Mark each ask done, filed with its nodes, blocked or open.")
	}
	stale := 0
	for _, r := range d.Asks {
		if r.Outcome != "" || r.Blocked != "" || r.StaleSaid || d.PersonTurns-r.Touched < askStaleTurns {
			continue
		}
		r.StaleSaid = true
		stale++
		lines = append(lines, fmt.Sprintf("- still open: %d %s", r.N, r.Title))
	}
	if stale > 0 {
		h.asksSaveLocked()
		do = append(do, "File each still open ask, mark it, or keep it (state open).")
	}
	if len(lines) == 0 {
		return ""
	}
	return "Asks (the asks tool):\n" + strings.Join(lines, "\n") + "\n" + strings.Join(do, " ")
}

// ── what the model reads ────────────────────────────────────────────────

// asksBlock is the intake and the open asks as the model reads them, ""
// with none: the intake's newest askBlockIntake messages, and up to
// askBlockAsks open asks by number and title. Done and filed asks are not
// listed. The ledger keeps everything the block leaves out.
func (h *Harness) asksBlock() string {
	if !h.asksOn() {
		return ""
	}
	h.asks.mu.Lock()
	defer h.asks.mu.Unlock()
	d := h.asksLocked()
	var b strings.Builder
	if n := len(d.Intake); n > 0 {
		fmt.Fprintf(&b, "- intake (id 0), not acknowledged (%s):", numfmt.Plural(n, "message", "messages"))
		from := max(0, n-askBlockIntake)
		if from > 0 {
			fmt.Fprintf(&b, " +%d earlier,", from)
		}
		for _, m := range d.Intake[from:] {
			b.WriteString("\n  " + m.At.Local().Format("15:04") + " " + workClip(m.Text, askBlockQuote))
		}
		b.WriteString("\n")
	}
	shown, more := 0, 0
	for _, r := range d.Asks {
		if r.Outcome != "" {
			continue
		}
		if shown == askBlockAsks {
			more++
			continue
		}
		shown++
		fmt.Fprintf(&b, "- %d %s", r.N, r.Title)
		if r.Blocked != "" {
			b.WriteString(" (blocked: " + workClip(r.Blocked, 80) + ")")
		}
		b.WriteString("\n")
	}
	if more > 0 {
		fmt.Fprintf(&b, "- +%d more open\n", more)
	}
	if b.Len() == 0 {
		return ""
	}
	return "Open asks:\n" + strings.TrimSuffix(b.String(), "\n")
}

// asksTurnLine puts the block in the conversation as a turn begins, as the
// plan's line does (planTurnLine): an aside keyed "asks", so a newer one
// replaces it. With nothing open it says so once, so an earlier block does
// not stand.
func (h *Harness) asksTurnLine() {
	b := h.asksBlock()
	h.asks.mu.Lock()
	was := h.asks.blockSent
	h.asks.blockSent = b != ""
	h.asks.mu.Unlock()
	switch {
	case b != "":
		h.AsideWith("[asks] "+b, AsideOpts{Key: "asks", Tag: asksTag})
	case was:
		h.AsideWith("[asks] no open asks.", AsideOpts{Key: "asks", Tag: asksTag})
	}
}

// asksTag tags the block's entry: the model reads "[asks] Open asks: …",
// not "[notification] [asks] …".
const asksTag = "asks"

// asksFoldAsk is the ask a fold must keep (fold.go noteAsk) where asks are
// kept, and remembered as the fold's ask (askNow, which the note after a
// fold and the summary check read): the ask the turn works, its text with
// the newest askFoldLater messages merged into it and the intake's newest,
// or the intake alone. ok false where asks are not kept: the fold finds
// the ask in the conversation, as before.
func (h *Harness) asksFoldAsk() (foldAsk, bool) {
	if !h.asksOn() {
		return foldAsk{}, false
	}
	h.asks.mu.Lock()
	d := h.asksLocked()
	var a foldAsk
	later := func(id string, ms []askMsg, keep int) {
		if cut := len(ms) - keep; cut > 0 {
			a.later = append(a.later, askLater{id: id + "-earlier", text: fmt.Sprintf("(+%d earlier, in the asks ledger)", cut), at: ms[0].At.UnixNano()})
			ms = ms[cut:]
		}
		for i, m := range ms {
			a.later = append(a.later, askLater{id: id + "-" + strconv.Itoa(i), text: m.Text, at: m.At.UnixNano()})
		}
	}
	var r *askRec
	if n, err := strconv.Atoi(h.asks.cur); err == nil {
		r = h.askByNLocked(n)
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
		a = foldAsk{id: "ask-" + strconv.Itoa(r.N), text: r.Text, at: r.At.UnixNano()}
		later(a.id, r.Continued, askFoldLater)
		later("intake", d.Intake, askBlockIntake)
	case len(d.Intake) > 0:
		a = foldAsk{id: "intake", text: d.Intake[0].Text, at: d.Intake[0].At.UnixNano()}
		later("intake", d.Intake[1:], askBlockIntake)
	}
	h.asks.mu.Unlock()
	h.askMu.Lock()
	h.ask = a
	h.askMu.Unlock()
	return a, true
}

// asksLiftBudget reports whether an ask holds the automatic-turn budget
// off (autoturn.go): the intake has messages, or an ask is open, not
// blocked, and touched within askStaleTurns of the person's turns. An ask
// left open and untouched stops counting, so the budget is never off for
// good.
func (h *Harness) asksLiftBudget() bool {
	h.asks.mu.Lock()
	defer h.asks.mu.Unlock()
	d := h.asksLocked()
	if len(d.Intake) > 0 {
		return true
	}
	for _, r := range d.Asks {
		if r.Outcome == "" && r.Blocked == "" && d.PersonTurns-r.Touched < askStaleTurns {
			return true
		}
	}
	return false
}

// asksWaiting is the intake (one id per intake: "intake-<seq>") and the
// open asks not blocked ("ask-<n>"), with their titles: what the network
// wait resumes and counts.
func (h *Harness) asksWaiting() (ids, titles []string) {
	h.asks.mu.Lock()
	defer h.asks.mu.Unlock()
	d := h.asksLocked()
	if len(d.Intake) > 0 {
		ids = append(ids, "intake-"+strconv.Itoa(d.IntakeSeq))
		titles = append(titles, headLine(d.Intake[0].Text, askTitleMax))
	}
	for _, r := range d.Asks {
		if r.Outcome == "" && r.Blocked == "" {
			ids = append(ids, "ask-"+strconv.Itoa(r.N))
			titles = append(titles, r.Title)
		}
	}
	return ids, titles
}

// ── what the screens show ───────────────────────────────────────────────

// PlanAsk is an ask, for the screens.
type PlanAsk struct {
	N       int
	Title   string
	State   string // open, blocked, done, filed, dropped, gone
	Why     string // blocked: the question
	At      time.Time
	Working bool // the ask the turn works (one at most, or the intake)
	Nodes   []string
}

// PlanIntake is the intake for the screens.
type PlanIntake struct {
	Msgs    []PlanMsg
	Working bool
}

// PlanMsg is one message on the intake.
type PlanMsg struct {
	At   time.Time
	Text string
}

// asksView is the asks, the intake (nil when empty), and which ask each
// plan node serves.
func (h *Harness) asksView() ([]PlanAsk, *PlanIntake, map[string]int) {
	if !h.asksWanted() {
		return nil, nil, nil
	}
	h.asksMigrate()
	h.asks.mu.Lock()
	defer h.asks.mu.Unlock()
	d := h.asksLocked()
	var asks []PlanAsk
	serves := map[string]int{}
	for _, r := range d.Asks {
		st := askStateOf(r)
		asks = append(asks, PlanAsk{N: r.N, Title: r.Title, State: st, Why: r.Blocked, At: r.At, Nodes: slices.Clone(r.Nodes),
			Working: h.asks.cur == strconv.Itoa(r.N) && r.Outcome == ""})
		for _, n := range r.Nodes {
			if _, ok := serves[n]; !ok {
				serves[n] = r.N
			}
		}
	}
	var in *PlanIntake
	if len(d.Intake) > 0 {
		in = &PlanIntake{Working: h.asks.cur == askIntakeID}
		for _, m := range d.Intake {
			in.Msgs = append(in.Msgs, PlanMsg{At: m.At, Text: m.Text})
		}
	}
	return asks, in, serves
}

// AskBrief is a session's newest open ask, "ask 3: fix the stubs dir", or
// the intake's first message, read from its sidecar: what dund lists the
// session as doing (dun ps, dun_list); "" with none.
func AskBrief(sessionFile string) string {
	b, err := os.ReadFile(AsksFile(sessionFile))
	if err != nil {
		return ""
	}
	var d askFileData
	if json.Unmarshal(b, &d) != nil {
		return ""
	}
	for i := len(d.Asks) - 1; i >= 0; i-- {
		if r := d.Asks[i]; r.Outcome == "" && r.Node == "" {
			return fmt.Sprintf("ask %d: %s", r.N, r.Title)
		}
	}
	if len(d.Intake) > 0 {
		return "new: " + headLine(d.Intake[0].Text, askTitleMax)
	}
	return ""
}
