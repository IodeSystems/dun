package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/taskgraph"
	"github.com/iodesystems/dun/internal/fsutil"
)

// asks.go — what the person types becomes ASKS (USER 2026-11-17, and
// 2026-11-18: "a single unacknowledged ask that grows as the user adds to
// it"). Each message goes on the intake, the one ask not yet acknowledged;
// the model makes asks of it with the asks tool (asktool.go): split, merged
// into the ask it continues, or closed at once. An ask is a top-level plan
// node "ask: <title>", with its text in the details, marked as the
// person's (planState.by, planByUser), and keeps the person's words. The
// asks tool or the plan tools close it. The turn-end check of a turn the
// person started names the intake and the touched asks not marked.
//
// Not on the intake: a message that starts with a bracketed lowercase tag
// ("[automatic] …", "[control/plan] …", "[settle] …": what dun or dund
// sends in the person's place), a sub-agent's, a --non-interactive run's
// (no one reads the plan there), and one in a session with no plan tools.
//
// The turn ledger's facts (ledger.go) go to the asks open during the turn:
// open as it began, or opened in it. When the turn's plan changes touch an
// ask, or a node that descends from one (needs, followed up), the facts go
// to those asks only; otherwise to every open ask. Facts given to more than
// one ask are marked shared. A turn that served no ask and made a tool call
// is a /journal entry of its own (kind "turn").
//
// When an ask closes, /journal gets an entry (kind "ask"): its text, the
// outcome and note, the turns that served it and the facts summed. An open
// ask is shown in /journal as open, with its facts so far. When the session
// ends (Close) with asks open, each gets one entry saying so.
//
// Kept in <session>.asks.json with the session's turn counter, so a resumed
// session keeps its asks and goes on numbering turns.

// planByUser is planState.by's word for an ask node.
const planByUser = "user"

// Journal kinds of the ledger.
const (
	JournalAsk  = "ask"
	JournalTurn = "turn"
)

const (
	askTitleMax = 60
	askTextMax  = 4000
	askTurnsMax = 200
	askDoneWhen = "the person's message is answered, or dropped with why"
)

// askTurn is one turn that served an ask.
type askTurn struct {
	N           int       `json:"n"`
	Start       time.Time `json:"start"`
	End         time.Time `json:"end"`
	Shared      bool      `json:"shared,omitempty"`
	Interrupted bool      `json:"interrupted,omitempty"`
}

// AskLedger is one ask and what was done for it.
type AskLedger struct {
	N     int       `json:"n"`
	Node  string    `json:"node"`
	Title string    `json:"title"`
	Text  string    `json:"text"`
	At    time.Time `json:"at"`
	Turns []askTurn `json:"turns,omitempty"`
	// NTurns counts every turn; Turns keeps the first askTurnsMax.
	NTurns  int         `json:"n_turns,omitempty"`
	Shared  int         `json:"shared,omitempty"`
	Facts   LedgerFacts `json:"facts"`
	Outcome string      `json:"outcome,omitempty"` // done | filed | dropped | gone; "" open
	Note    string      `json:"note,omitempty"`
	Closed  time.Time   `json:"closed,omitzero"`
	// Ended: the session ended with the ask open (the entry's only).
	Ended bool `json:"ended,omitempty"`
	// Source is the intake the ask was made from, word for word: Title and
	// Text are the model's (asktool.go).
	Source []askMsg `json:"source,omitempty"`
	// Continued is what the person wrote later that the model merged into it.
	Continued []askMsg `json:"continued,omitempty"`
	// Filed is the plan nodes the ask was filed as (Outcome "filed").
	Filed []string `json:"filed,omitempty"`
}

// askMsg is one message the person typed.
type askMsg struct {
	At   time.Time `json:"at"`
	Text string    `json:"text"`
}

// askIntakeID names the intake in the asks tool and in askState.cur.
const askIntakeID = "intake"

// TurnLedger is a turn that served no ask.
type TurnLedger struct {
	N     int         `json:"n"`
	Start time.Time   `json:"start"`
	End   time.Time   `json:"end"`
	Facts LedgerFacts `json:"facts"`
}

type askRec struct {
	AskLedger
	EndSnap bool `json:"end_snap,omitempty"` // a session-end entry was written
	// Marked is the turn in which the model last marked it (asktool.go):
	// the turn-end check names an ask the turn touched and did not mark.
	Marked int `json:"marked,omitempty"`
}

type askFileData struct {
	Turn int       `json:"turn"`
	Asks []*askRec `json:"asks,omitempty"`
	// Intake is the person's messages not yet acknowledged, oldest first:
	// one ask that grows until the model makes asks of it.
	Intake []askMsg `json:"intake,omitempty"`
}

type askState struct {
	mu   sync.Mutex
	file string
	read bool
	d    askFileData
	// cur is what the turn works: askIntakeID, an ask's node, or "" (not
	// kept: a restarted engine runs no turn).
	cur         string
	foldDropped atomic.Bool
}

// AsksFile is the session's asks sidecar.
func AsksFile(sessionFile string) string {
	return strings.TrimSuffix(sessionFile, ".jsonl") + ".asks.json"
}

// asksLocked is the sidecar's content, read on first use and again when the
// session file changed. Under asks.mu.
func (h *Harness) asksLocked() *askFileData {
	a := &h.asks
	if a.read && a.file == h.cfg.SessionFile {
		return &a.d
	}
	a.read, a.file, a.d = true, h.cfg.SessionFile, askFileData{}
	if a.file != "" {
		if b, err := os.ReadFile(AsksFile(a.file)); err == nil {
			_ = json.Unmarshal(b, &a.d)
		}
	}
	return &a.d
}

func (h *Harness) asksSaveLocked() {
	if h.asks.file == "" {
		return
	}
	b, err := json.MarshalIndent(h.asks.d, "", " ")
	if err != nil {
		return
	}
	_ = fsutil.WriteAtomic(AsksFile(h.asks.file), append(b, '\n'), 0o600)
}

// autoTag is a message dun or dund sent in the person's place.
var autoTag = regexp.MustCompile(`^\[[a-z][a-z0-9/_ .-]*\]`)

// personAsk reports whether text is a message the person typed (a [loop]
// or [scheduled] firing is not an ask: USER 2026-11-18).
func personAsk(text string) bool { return PersonText(text) }

func interruptedErr(err error) bool { return errors.Is(err, ErrInterrupted) }

// intakeAdd puts a message the person typed on the intake, if it is one:
// the one ask not yet acknowledged, which each message grows until the
// model acknowledges it (asktool.go). It reports whether it did.
func (h *Harness) intakeAdd(text string) bool {
	if !h.asksOn() || !personAsk(text) {
		return false
	}
	h.dropFoldAskNode()
	h.asks.mu.Lock()
	d := h.asksLocked()
	d.Intake = append(d.Intake, askMsg{At: time.Now(), Text: askCut(strings.TrimSpace(text))})
	h.asks.cur = askIntakeID
	h.asksSaveLocked()
	h.asks.mu.Unlock()
	h.planChanged() // the plan screens show the intake
	return true
}

// askCut is text cut at askTextMax runes.
func askCut(text string) string {
	if r := []rune(text); len(r) > askTextMax {
		return string(r[:askTextMax]) + " …[cut]"
	}
	return text
}

// dropFoldAskNode drops the fold's "Current ask: …" node once per process:
// where asks are kept they hold the person's messages, and that node took
// the latest one, "continue" too, for the ask (fold.go planAsk).
func (h *Harness) dropFoldAskNode() {
	if h.asks.foldDropped.Swap(true) {
		return
	}
	_ = planStore{h}.Update(context.Background(), func(g *taskgraph.Graph) error {
		if n := askNode(g); n != nil {
			return g.SetStatus(n.ID, taskgraph.Dropped, "the asks hold the person's messages now")
		}
		return nil
	})
}

// askAdd makes an ask: a plan node "ask: <title>" with text in its details,
// in progress, marked as the person's, and its record. src is the intake it
// came from, kept word for word.
func (h *Harness) askAdd(title, text string, src []askMsg) (*askRec, error) {
	details := askCut(strings.TrimSpace(text))
	first := headLine(strings.TrimSpace(title), askTitleMax)
	if first == "" {
		first = headLine(details, askTitleMax)
	}
	if first == "" {
		return nil, errors.New("an ask needs a title or text")
	}
	p := h.planNow()
	h.planMu.Lock()
	prev := p.by
	h.planMu.Unlock()
	var id string
	err := p.store.Update(context.Background(), func(g *taskgraph.Graph) error {
		g.Seq++
		id = strconv.Itoa(g.Seq)
		g.Nodes = append(g.Nodes, &taskgraph.Node{ID: id, Title: "ask: " + first, Details: details,
			Status: taskgraph.Doing, DoneWhen: askDoneWhen}) // being answered: asks or plan_close take it from here
		h.planMu.Lock()
		by := make(map[string]string, len(prev)+1)
		for k, v := range prev {
			by[k] = v
		}
		by[id] = planByUser
		p.by = by // savePlan, after this returns, writes it with the graph
		h.planMu.Unlock()
		return nil
	})
	if err != nil {
		h.planMu.Lock()
		p.by = prev
		h.planMu.Unlock()
		return nil, err
	}
	h.planChanged()
	h.asks.mu.Lock()
	d := h.asksLocked()
	n := len(d.Asks) + 1
	r := &askRec{AskLedger: AskLedger{N: n, Node: id, Title: first, Text: details, At: time.Now(), Source: slices.Clone(src)}}
	if len(src) > 0 {
		r.At = src[0].At
	}
	d.Asks = append(d.Asks, r)
	h.asksSaveLocked()
	h.asks.mu.Unlock()
	h.ledger.mu.Lock()
	if h.ledger.open {
		h.ledger.asked = append(h.ledger.asked, n)
	}
	h.ledger.mu.Unlock()
	return r, nil
}

// isAskNode reports whether plan node id is an ask.
func (h *Harness) isAskNode(id string) bool {
	p := h.planNow()
	h.planMu.Lock()
	defer h.planMu.Unlock()
	return p.by[id] == planByUser
}

// openWork counts g's open nodes that are not asks: what the plan nudges
// count (an ask is the person's, not a plan the agent wrote).
func (h *Harness) openWork(g taskgraph.Graph) int {
	n := 0
	for _, nd := range g.Nodes {
		if planOpen(nd) && !h.isAskNode(nd.ID) {
			n++
		}
	}
	return n
}

// asksOpenNow is the numbers of the open asks.
func (h *Harness) asksOpenNow() []int {
	if h.isChild() {
		return nil
	}
	h.asks.mu.Lock()
	defer h.asks.mu.Unlock()
	var out []int
	for _, r := range h.asksLocked().Asks {
		if r.Outcome == "" {
			out = append(out, r.N)
		}
	}
	return out
}

// askTurnBegin counts a turn and returns its number.
func (h *Harness) askTurnBegin() int {
	h.asks.mu.Lock()
	defer h.asks.mu.Unlock()
	d := h.asksLocked()
	d.Turn++
	h.asksSaveLocked()
	return d.Turn
}

// asksTurnNow is the number of the last turn begun.
func (h *Harness) asksTurnNow() int {
	h.asks.mu.Lock()
	defer h.asks.mu.Unlock()
	return h.asksLocked().Turn
}

// askAncestors is the asks node id descends from, itself included: needs
// followed up (a node "under" another needs it).
func askAncestors(g taskgraph.Graph, id string, isAsk func(string) bool) []string {
	var out []string
	seen := map[string]bool{}
	stack := []string{id}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		if isAsk(cur) {
			out = append(out, cur)
			continue
		}
		if n := g.Get(cur); n != nil {
			stack = append(stack, n.Needs...)
		}
	}
	return out
}

// asksFile files a turn's facts (ledgerEnd), then writes the entries of the
// asks that closed.
func (h *Harness) asksFile(ctx context.Context, f LedgerFacts, start time.Time, turn int, touched []string, asked []int) {
	g := h.planNow().store.full()
	end := time.Now()

	h.asks.mu.Lock()
	d := h.asksLocked()
	byN := map[int]*askRec{}
	byNode := map[string]*askRec{}
	for _, r := range d.Asks {
		byN[r.N] = r
		byNode[r.Node] = r
	}
	var cands []*askRec
	for _, n := range asked {
		if r := byN[n]; r != nil && !slices.Contains(cands, r) {
			cands = append(cands, r)
		}
	}
	// The asks the turn's plan changes name, among the candidates.
	var targets []*askRec
	isAsk := func(id string) bool { return byNode[id] != nil }
	for _, id := range touched {
		for _, a := range askAncestors(g, id, isAsk) {
			if r := byNode[a]; slices.Contains(cands, r) && !slices.Contains(targets, r) {
				targets = append(targets, r)
			}
		}
	}
	if len(targets) == 0 {
		targets = cands
	}
	shared := len(targets) > 1
	for _, r := range targets {
		r.NTurns++
		if len(r.Turns) < askTurnsMax {
			r.Turns = append(r.Turns, askTurn{N: turn, Start: start, End: end, Shared: shared, Interrupted: f.Interrupted})
		}
		if shared {
			r.Shared++
		}
		r.Facts.add(f)
		r.Facts.capLists()
	}
	// Asks the plan now has closed (by the agent, or the person's edit).
	var closed []AskLedger
	for _, r := range d.Asks {
		if r.Outcome != "" {
			continue
		}
		n := g.Get(r.Node)
		switch {
		case n == nil:
			r.Outcome = "gone"
		case n.Status == taskgraph.Done && len(r.Filed) > 0:
			r.Outcome, r.Note = "filed", n.Account
		case n.Status == taskgraph.Done:
			r.Outcome, r.Note, r.Filed = "done", n.Account, nil
		case n.Status == taskgraph.Dropped:
			r.Outcome, r.Note = "dropped", n.Why
		default:
			continue
		}
		r.Closed = end
		closed = append(closed, r.AskLedger)
	}
	h.asksSaveLocked()
	h.asks.mu.Unlock()

	if len(targets) == 0 && f.ToolCalls > 0 {
		t := TurnLedger{N: turn, Start: start, End: end, Facts: f}
		e := JournalEntry{At: end, Kind: JournalTurn, Title: fmt.Sprintf("turn %d", turn), Turn: &t}
		e.Tokens = h.estimate(e.Body())
		if h.journalAdd(e) == nil {
			h.ledgerIndex(ctx, e, "ledger", fmt.Sprintf("turn-%d-%d", turn, end.UnixNano()))
		}
	}
	for _, a := range closed {
		h.askJournal(ctx, a)
	}
}

// askJournal writes an ask's entry and indexes it.
func (h *Harness) askJournal(ctx context.Context, a AskLedger) {
	at := a.Closed
	if at.IsZero() {
		at = time.Now()
	}
	e := JournalEntry{At: at, Kind: JournalAsk, Title: a.Title, Ask: &a}
	e.Tokens = h.estimate(e.Body())
	if h.journalAdd(e) == nil {
		h.ledgerIndex(ctx, e, "ask", fmt.Sprintf("ask-%d-%d", a.N, at.UnixNano()))
	}
}

// ledgerIndex puts a ledger entry in the sessions index (kind:<kind>), so
// session_search finds "which turn changed X".
func (h *Harness) ledgerIndex(ctx context.Context, e JournalEntry, kind, key string) {
	x := h.sessionIndex()
	if x == nil {
		return
	}
	_ = x.Put(ctx, agent.SessionDoc{Key: key, Kind: kind, Name: e.Line(0), Summary: e.Body()})
}

// asksSessionEnd writes an entry for each ask still open as the session
// ends, once per ask.
func (h *Harness) asksSessionEnd() {
	if h.isChild() {
		return
	}
	h.asks.mu.Lock()
	if !h.asks.read && h.cfg.SessionFile != "" {
		if _, err := os.Stat(AsksFile(h.cfg.SessionFile)); err != nil {
			h.asks.mu.Unlock()
			return
		}
	}
	d := h.asksLocked()
	var open []AskLedger
	for _, r := range d.Asks {
		if r.Outcome == "" && !r.EndSnap {
			r.EndSnap = true
			a := r.AskLedger
			a.Ended, a.Closed = true, time.Now()
			open = append(open, a)
		}
	}
	if len(open) > 0 {
		h.asksSaveLocked()
	}
	h.asks.mu.Unlock()
	for _, a := range open {
		h.askJournal(context.Background(), a)
	}
}

// openAskEntries are the open asks as /journal shows them: after the kept
// entries, not kept themselves.
func (h *Harness) openAskEntries() []JournalEntry {
	if h.isChild() {
		return nil
	}
	h.asks.mu.Lock()
	defer h.asks.mu.Unlock()
	var out []JournalEntry
	for _, r := range h.asksLocked().Asks {
		if r.Outcome == "" {
			a := r.AskLedger
			out = append(out, JournalEntry{At: a.At, Kind: JournalAsk, Title: a.Title, Ask: &a})
		}
	}
	return out
}

// ── rendering ───────────────────────────────────────────────────────────

// outcomeWord is an ask's state in a row.
func (a AskLedger) outcomeWord() string {
	switch {
	case a.Ended:
		return "open at session end"
	case a.Outcome == "":
		return "open"
	}
	return a.Outcome
}

// turnSpan is "turn 12", "turns 12–15", or "".
func (a AskLedger) turnSpan() string {
	if len(a.Turns) == 0 {
		return ""
	}
	first, last := a.Turns[0].N, a.Turns[len(a.Turns)-1].N
	if first == last {
		return "turn " + strconv.Itoa(first)
	}
	return fmt.Sprintf("turns %d–%d", first, last)
}

// Row is the ask as /journal lists it: `ask 3 · done · "fix the flaky
// test" · turns 12–15 · 6 files (+120/−34) · 1 commit · 11 cmds (1 failed) · 9m`.
func (a AskLedger) Row() string {
	parts := []string{fmt.Sprintf("ask %d", a.N), a.outcomeWord(), strconv.Quote(workClip(a.Title, 40))}
	if s := a.turnSpan(); s != "" {
		parts = append(parts, s)
	}
	if s := a.Facts.Summary(); s != "" {
		parts = append(parts, s)
	}
	if a.Facts.WallMS > 0 {
		parts = append(parts, workDuration(time.Duration(a.Facts.WallMS)*time.Millisecond))
	}
	return strings.Join(parts, " · ")
}

// Full is the ask's entry in full.
func (a AskLedger) Full() string {
	var b strings.Builder
	b.WriteString(a.Row() + "\n\n")
	b.WriteString("asked " + a.At.Local().Format("Jan 2 15:04") + ":\n" + a.Text + "\n\n")
	if len(a.Source) > 0 && !(len(a.Source) == 1 && a.Source[0].Text == a.Text) {
		b.WriteString("from what the person wrote:\n")
		for _, m := range a.Source {
			b.WriteString(m.At.Local().Format("15:04") + "  " + m.Text + "\n")
		}
		b.WriteString("\n")
	}
	for _, m := range a.Continued {
		b.WriteString("continued " + m.At.Local().Format("Jan 2 15:04") + ":\n" + m.Text + "\n\n")
	}
	switch {
	case a.Ended:
		b.WriteString("still open when the session ended\n")
	case a.Outcome == "":
		b.WriteString("open\n")
	default:
		fmt.Fprintf(&b, "%s %s", a.Outcome, a.Closed.Local().Format("Jan 2 15:04"))
		if a.Note != "" {
			b.WriteString(": " + a.Note)
		}
		b.WriteString("\n")
	}
	if len(a.Turns) > 0 {
		var ts []string
		for _, t := range a.Turns {
			s := fmt.Sprintf("%d (%s–%s", t.N, t.Start.Local().Format("15:04"), t.End.Local().Format("15:04"))
			if t.Shared {
				s += ", shared"
			}
			if t.Interrupted {
				s += ", interrupted"
			}
			ts = append(ts, s+")")
		}
		fmt.Fprintf(&b, "turns (%d): %s", a.NTurns, strings.Join(ts, ", "))
		if a.NTurns > len(a.Turns) {
			fmt.Fprintf(&b, " … +%d more", a.NTurns-len(a.Turns))
		}
		b.WriteString("\n")
		if a.Shared > 0 {
			fmt.Fprintf(&b, "%d of them served another ask too: their facts are counted in each\n", a.Shared)
		}
	}
	b.WriteString("\n" + a.Facts.Text())
	return b.String()
}

// Row is the turn as /journal lists it: "turn 14 · 6 files (+120/−34) ·
// 2 commits · 11 cmds (1 failed) · 4m".
func (t TurnLedger) Row() string {
	parts := []string{"turn " + strconv.Itoa(t.N)}
	if s := t.Facts.Summary(); s != "" {
		parts = append(parts, s)
	}
	if t.Facts.Interrupted {
		parts = append(parts, "interrupted")
	}
	parts = append(parts, workDuration(time.Duration(t.Facts.WallMS)*time.Millisecond))
	return strings.Join(parts, " · ")
}

// Full is the turn's entry in full.
func (t TurnLedger) Full() string {
	return fmt.Sprintf("%s\n%s–%s, no ask open\n\n%s", t.Row(), t.Start.Local().Format("Jan 2 15:04:05"),
		t.End.Local().Format("15:04:05"), t.Facts.Text())
}
