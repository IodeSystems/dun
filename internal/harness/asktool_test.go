package harness

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/taskgraph"
)

// askOpen is a test's shortcut for a message the model acknowledged as one
// open ask: the intake grows, and the asks tool makes it an ask.
func (h *Harness) askOpen(text string) bool {
	if !h.intakeAdd(text) {
		return false
	}
	return !strings.Contains(h.asksCall(`{"asks":[{"id":0,"state":"open"}]}`), "ERROR")
}

func askRecOf(t *testing.T, h *Harness, n int) AskLedger {
	t.Helper()
	h.asks.mu.Lock()
	defer h.asks.mu.Unlock()
	r := h.askByNLocked(n)
	if r == nil {
		t.Fatalf("no ask %d", n)
	}
	return r.AskLedger
}

// asksBy is the snapshot's asks by number, and which work.
func asksBy(h *Harness) (map[int]PlanAsk, []int) {
	out := map[int]PlanAsk{}
	var working []int
	for _, a := range h.Plan().Asks {
		out[a.N] = a
		if a.Working {
			working = append(working, a.N)
		}
	}
	return out, working
}

func startAsks(t *testing.T) (*Harness, string, string) {
	t.Helper()
	t.Setenv("DUN_HOME", t.TempDir())
	dir := t.TempDir()
	sf := filepath.Join(dir, "s.jsonl")
	return startLedger(t, &nudgeRunner{}, dir, sf, nil), dir, sf
}

// Messages grow one intake; an automatic one does not. One call splits it
// into three asks and merges it into an existing ask, the person's words
// kept whole. The intake is then empty, one ask works, and the next message
// starts a new intake. Another call marks several asks mid-turn and links a
// plan node. None of it is in the plan graph, and all of it survives a
// restart.
func TestAsks_IntakeSplitMergeMarkAndRestart(t *testing.T) {
	h, dir, sf := startAsks(t)
	if !h.askOpen("test the login flow") {
		t.Fatal("a short real message did not become an ask")
	}
	if asks, _ := asksBy(h); len(asks) != 1 || asks[1].Title != "test the login flow" || len(h.Plan().Rows) != 0 {
		t.Fatalf("asks %+v rows %+v", asks, h.Plan().Rows)
	}
	h.ledgerBegin(originPerson)
	h.intakeAdd("Add plan items:\n1. locate tow yards\n2. vds: port 18020 is in use\n3. vehicle telemetry")
	h.SayAs(NewMessageID(), "retry")
	h.SayAs(NewMessageID(), "[automatic] resume after the upgrade")
	in := h.Plan().Intake
	if in == nil || len(in.Msgs) != 2 || in.Msgs[1].Text != "retry" || !in.Working {
		t.Fatalf("intake %+v", in)
	}
	out := h.asksCall(`{"asks":[{"id":"intake","split":[
		{"title":"locate tow yards","text":"1. locate tow yards"},
		{"title":"free port 18020 for vds","text":"2. vds: port 18020 is in use"},
		{"title":"investigate vehicle telemetry","text":"3. vehicle telemetry"}]},
		{"id":"intake","merge_into":1,"text":"the retry is for the login test"}]}`)
	if strings.Contains(out, "ERROR") {
		t.Fatal(out)
	}
	if h.Plan().Intake != nil {
		t.Fatalf("the intake was not emptied: %+v", h.Plan().Intake)
	}
	asks, working := asksBy(h)
	if len(asks) != 4 || asks[2].Title != "locate tow yards" || asks[4].State != "open" || len(h.Plan().Rows) != 0 {
		t.Fatalf("asks %+v", asks)
	}
	if !slices.Equal(working, []int{2}) {
		t.Errorf("working %v, want only 2", working)
	}
	a2 := askRecOf(t, h, 2)
	if a2.Text != "1. locate tow yards" || len(a2.Source) != 2 || !strings.Contains(a2.Source[0].Text, "vehicle telemetry") {
		t.Errorf("split ask %+v", a2)
	}
	c := askRecOf(t, h, 1).Continued
	if len(c) != 2 || !strings.Contains(c[0].Text, "locate tow yards") || c[1].Text != "retry" || c[1].Note != "the retry is for the login test" {
		t.Errorf("merged into ask 1: the person's words lost: %+v", c)
	}
	// Several asks at once, mid-turn: done, filed, reworded, a bad id.
	planCall(t, h, "plan_add", `{"title":"write the vds port fix","done_when":"vds starts"}`)
	out = h.asksCall(`{"asks":[{"id":2,"state":"done","note":"three yards listed"},
		{"id":"3","state":"filed","nodes":["1"]},
		{"id":"#4","title":"vehicle telemetry research"},
		{"id":9,"state":"done","note":"x"}]}`)
	if !strings.Contains(out, "ask 3: filed → 1") || !strings.Contains(out, "ERROR: ask 9") {
		t.Errorf("result:\n%s", out)
	}
	asks, _ = asksBy(h)
	if asks[2].State != "done" || asks[3].State != "filed" || !slices.Equal(asks[3].Nodes, []string{"1"}) {
		t.Errorf("states %+v %+v", asks[2], asks[3])
	}
	if rows := h.Plan().Rows; len(rows) != 1 || rows[0].Ask != 3 {
		t.Errorf("the node does not serve ask 3: %+v", rows)
	}
	if a4 := askRecOf(t, h, 4); a4.Title != "vehicle telemetry research" || a4.Text != "3. vehicle telemetry" {
		t.Errorf("reworded %+v", a4)
	}
	h.ledgerEnd(context.Background(), nil)
	var outcomes []string
	for _, e := range h.journalLocked() {
		outcomes = append(outcomes, e.Ask.Outcome)
	}
	if !slices.Equal(outcomes, []string{"done", "filed"}) {
		t.Errorf("journal outcomes %v", outcomes)
	}
	h.intakeAdd("one more thing")
	if in := h.Plan().Intake; in == nil || len(in.Msgs) != 1 {
		t.Fatalf("new intake %+v", in)
	}
	h.Close()

	h2 := startLedger(t, &nudgeRunner{}, dir, sf, nil)
	t.Cleanup(h2.Close)
	if in := h2.Plan().Intake; in == nil || len(in.Msgs) != 1 || in.Msgs[0].Text != "one more thing" {
		t.Fatalf("intake after restart %+v", in)
	}
	asks, _ = asksBy(h2)
	if len(asks) != 4 || asks[3].State != "filed" || asks[4].Title != "vehicle telemetry research" {
		t.Fatalf("asks after restart %+v", asks)
	}
	if c := askRecOf(t, h2, 1).Continued; len(c) != 2 {
		t.Errorf("ask 1 after restart: %+v", c)
	}
	if got := len(journalOf(h2, JournalAsk)); got != 6 { // 2 closed, 2 open at session end, 2 open
		t.Errorf("journal after restart: %d ask entries", got)
	}
}

// Merging into a done ask reopens it: the person carried on with it.
func TestAsks_MergeReopensDone(t *testing.T) {
	h, _, _ := startAsks(t)
	t.Cleanup(h.Close)
	h.askOpen("fix the flaky test")
	h.asksCall(`{"asks":[{"id":1,"state":"done","note":"retry added"}]}`)
	h.ledgerBegin(originPerson)
	h.ledgerEnd(context.Background(), nil)
	if a := askRecOf(t, h, 1); a.Outcome != "done" || len(journalOf(h, JournalAsk)) != 1 {
		t.Fatalf("not done: %+v", a)
	}
	h.intakeAdd("still flaky")
	if out := h.asksCall(`{"asks":[{"id":"intake","merge_into":"1"}]}`); !strings.Contains(out, "reopened (it was done)") {
		t.Errorf("result %q", out)
	}
	if a := askRecOf(t, h, 1); a.Outcome != "" || !a.Closed.IsZero() || a.Continued[0].Text != "still flaky" {
		t.Errorf("record not reopened: %+v", a)
	}
	if asks, working := asksBy(h); asks[1].State != "open" || !slices.Equal(working, []int{1}) {
		t.Errorf("asks %+v working %v", asks, working)
	}
}

// The turn-end check names the intake while it has messages, and the asks
// the turn touched that it did not mark; once, an ask long untouched;
// nothing else, and nothing in an automatic turn. An ask left untouched
// stops lifting the automatic-turn budget.
func TestAsks_TurnEndCheckListsOnlyWhatIsLeft(t *testing.T) {
	h, _, _ := startAsks(t)
	t.Cleanup(h.Close)
	h.askOpen("fix the flaky test")
	h.askOpen("update the docs")
	h.turnBegin(originPerson)
	h.ledgerBegin(originPerson)
	if a := h.idleAsks(); a != "" {
		t.Errorf("nothing touched, yet: %q", a)
	}
	h.intakeAdd("continue")
	a := h.idleAsks()
	if !strings.Contains(a, `- intake: 1 message, not acknowledged: "continue"`) || strings.Contains(a, "- 1 ") {
		t.Errorf("check %q", a)
	}
	t.Logf("intake check: %d bytes", len(a))
	h.asksCall(`{"asks":[{"id":"intake","merge_into":1}]}`)
	planCall(t, h, "plan_add", `{"title":"write the page","done_when":"written"}`)
	h.asksCall(`{"asks":[{"id":2,"nodes":["1"]}]}`)
	a = h.idleAsks()
	if !strings.Contains(a, "- 1 fix the flaky test") || !strings.Contains(a, "- 2 update the docs") || strings.Contains(a, "intake") {
		t.Errorf("check %q", a)
	}
	t.Logf("two asks check: %d bytes", len(a))
	h.asksCall(`{"asks":[{"id":1,"state":"done","note":"fixed"},{"id":2,"state":"open"}]}`)
	if a := h.idleAsks(); a != "" {
		t.Errorf("all marked: %q", a)
	}
	if note, _ := h.idleNote(1); strings.Contains(note, "Asks") {
		t.Errorf("asks in the check: %q", note)
	}
	h.ledgerEnd(context.Background(), nil)
	// Ask 2 untouched for askStaleTurns of the person's turns: named once,
	// and the budget is no longer off for it.
	if !h.asksLiftBudget() {
		t.Fatal("an open ask touched now does not lift the budget")
	}
	for range askStaleTurns {
		h.turnBegin(originPerson)
		h.ledgerBegin(originPerson)
		h.ledgerEnd(context.Background(), nil)
	}
	if h.asksLiftBudget() {
		t.Error("an ask untouched for 5 turns still lifts the budget")
	}
	h.turnBegin(originPerson)
	h.ledgerBegin(originPerson)
	if a := h.idleAsks(); !strings.Contains(a, "- still open: 2 update the docs") || !strings.Contains(a, "keep it (state open)") {
		t.Errorf("stale check %q", a)
	}
	if a := h.idleAsks(); a != "" {
		t.Errorf("named twice: %q", a)
	}
	h.ledgerEnd(context.Background(), nil)
	h.turnBegin(originAuto)
	h.ledgerBegin(originAuto)
	h.intakeAdd("as if typed")
	if a := h.idleAsks(); a != "" {
		t.Errorf("automatic turn: %q", a)
	}
	h.ledgerEnd(context.Background(), nil)
}

// The model reads the intake and the open asks in one block: as each turn
// begins (the keyed aside), in the fold's ask and in the note after a fold.
// Done asks are not in it; the intake shows its newest messages.
func TestAsks_PinnedBlockAndFold(t *testing.T) {
	h, _, _ := startAsks(t)
	t.Cleanup(h.Close)
	h.askOpen("fix the stubs dir")
	h.askOpen("add plan nodes")
	h.asksCall(`{"asks":[{"id":2,"state":"done","note":"added"},{"id":1,"title":"fix the stubs dir"}]}`)
	for i := range 8 {
		h.intakeAdd("message " + string(rune('a'+i)))
	}
	b := h.asksBlock()
	for _, w := range []string{"Open asks:", "- intake (id 0), not acknowledged (8 messages): +3 earlier,", "message h", "- 1 fix the stubs dir"} {
		if !strings.Contains(b, w) {
			t.Errorf("block lacks %q:\n%s", w, b)
		}
	}
	if strings.Contains(b, "message c") || strings.Contains(b, "add plan nodes") {
		t.Errorf("block shows an earlier message or a done ask:\n%s", b)
	}
	t.Logf("block: %d bytes; system line: %d bytes; tool def: %d bytes", len(b), len(systemAsks), toolDefBytes())
	h.planTurnLine()
	if q := strings.Join(queuedTexts(h), "\n"); !strings.Contains(q, "[asks] Open asks:") {
		t.Errorf("the turn line has no asks block: %q", q)
	}
	// The fold: the ask the turn works is the intake here.
	a := h.noteAsk(context.Background())
	if a.text != "message a" || len(a.later) != 6 || !strings.Contains(a.later[0].text, "+2 earlier") || h.askNow().text != "message a" {
		t.Errorf("fold ask %+v", a)
	}
	if n := h.foldRestoreNote(); !strings.Contains(n, "The current ask, verbatim") || !strings.Contains(n, "Open asks:") {
		t.Errorf("restore note:\n%s", n)
	}
	for _, nd := range planGraph(t, h).Nodes {
		if strings.HasPrefix(nd.Title, "Current ask") {
			t.Errorf("a fold node was written: %+v", nd)
		}
	}
	// Acknowledged into ask 1: the fold keeps it and the merged words.
	h.asksCall(`{"asks":[{"id":"intake","merge_into":1}]}`)
	if a := h.noteAsk(context.Background()); a.text != "fix the stubs dir" || !strings.Contains(a.later[len(a.later)-1].text, "message h") {
		t.Errorf("fold ask after ack %+v", a)
	}
}

func toolDefBytes() int {
	b, _ := json.Marshal(asksToolDef())
	return len(b)
}

// The schema is flat: the ids are integers, split is an array of strings,
// and no item property is an object. A split of titles makes one open ask
// per title, each with the whole intake as its text; id 0 is the intake.
func TestAsks_FlatSchema(t *testing.T) {
	var def struct {
		Properties struct {
			Asks struct {
				Items struct {
					Properties map[string]struct {
						Type  string `json:"type"`
						Items *struct {
							Type string `json:"type"`
						} `json:"items"`
					} `json:"properties"`
					Required []string `json:"required"`
				} `json:"items"`
			} `json:"asks"`
		} `json:"properties"`
	}
	b, _ := json.Marshal(asksToolDef().Function.Parameters)
	if err := json.Unmarshal(b, &def); err != nil {
		t.Fatal(err)
	}
	props := def.Properties.Asks.Items.Properties
	if props["id"].Type != "integer" || props["merge_into"].Type != "integer" {
		t.Errorf("id %q merge_into %q, want integer", props["id"].Type, props["merge_into"].Type)
	}
	if s := props["split"]; s.Type != "array" || s.Items == nil || s.Items.Type != "string" {
		t.Errorf("split %+v, want an array of strings", s)
	}
	for k, p := range props {
		if p.Type == "object" || (p.Items != nil && p.Items.Type == "object") {
			t.Errorf("%s is nested: %+v", k, p)
		}
	}
	t.Logf("tool def: %d bytes", toolDefBytes())

	h, _, _ := startAsks(t)
	t.Cleanup(h.Close)
	h.intakeAdd("fix the login test and update the docs")
	out := h.asksCall(`{"asks":[{"id":0,"split":["fix the login test","update the docs"]}]}`)
	if strings.Contains(out, "ERROR") {
		t.Fatal(out)
	}
	asks, _ := asksBy(h)
	if len(asks) != 2 || asks[1].Title != "fix the login test" || asks[2].Title != "update the docs" || asks[2].State != "open" {
		t.Fatalf("asks %+v", asks)
	}
	if a := askRecOf(t, h, 2); a.Text != "fix the login test and update the docs" {
		t.Errorf("text %q, want the intake", a.Text)
	}
	if out := h.asksCall(`{"asks":[{"id":1,"state":"done","note":"fixed"}]}`); !strings.Contains(out, "ask 1: done") {
		t.Errorf("result %q", out)
	}
	h.intakeAdd("one more")
	if out := h.asksCall(`{"asks":[{"id":0,"merge_into":0}]}`); !strings.Contains(out, "merge_into takes an ask's number") {
		t.Errorf("merge_into 0: %q", out)
	}
}

// A sidecar from before the ledger moves in: the ask nodes leave the
// graph, their states go to the records, a node that needed one serves it,
// and a closed one gets no second entry.
func TestAsks_MigrateFromPlanNodes(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	dir := t.TempDir()
	sf := filepath.Join(dir, "s.jsonl")
	g := taskgraph.Graph{Seq: 4, Nodes: []*taskgraph.Node{
		{ID: "1", Title: "ask: fix the flaky test", Status: taskgraph.Done, DoneWhen: "x", Account: "fixed"},
		{ID: "2", Title: "ask: update the docs", Status: taskgraph.Doing, DoneWhen: "x"},
		{ID: "3", Title: "write the page", Status: taskgraph.Todo, DoneWhen: "x", Needs: []string{"2"}},
		{ID: "4", Title: "ask: which region", Status: taskgraph.Blocked, DoneWhen: "x", Why: "eu or us?"},
	}}
	plan, _ := json.Marshal(planFileData{Graph: g, By: map[string]string{"1": planByUser, "2": planByUser, "4": planByUser}})
	_ = os.WriteFile(PlanFile(sf), plan, 0o600)
	old := `{"turn":3,"asks":[{"n":1,"node":"1","title":"fix the flaky test","outcome":"done","facts":{}},
		{"n":2,"node":"2","title":"update the docs","facts":{}},{"n":3,"node":"4","title":"which region","facts":{}}]}`
	_ = os.WriteFile(AsksFile(sf), []byte(old), 0o600)
	h := startLedger(t, &nudgeRunner{}, dir, sf, nil)
	t.Cleanup(h.Close)
	asks, _ := asksBy(h)
	if asks[1].State != "done" || asks[2].State != "open" || asks[3].State != "blocked" || asks[3].Why != "eu or us?" ||
		!slices.Equal(asks[2].Nodes, []string{"3"}) {
		t.Errorf("migrated %+v", asks)
	}
	rows := h.Plan().Rows
	if len(rows) != 1 || rows[0].ID != "3" || len(rows[0].Needs) != 0 || rows[0].Ask != 2 {
		t.Errorf("plan after the move %+v", rows)
	}
	h.ledgerBegin(originPerson)
	h.ledgerEnd(context.Background(), nil)
	if n := len(h.journalLocked()); n != 0 {
		t.Errorf("a migrated closed ask got another entry: %d", n)
	}
}

// The network wait counts resumes per intake, not under one id.
func TestAsks_NetWaitIDsPerIntake(t *testing.T) {
	h, _, _ := startAsks(t)
	t.Cleanup(h.Close)
	h.intakeAdd("fix it")
	ids, _ := h.openAsks()
	h.asksCall(`{"asks":[{"id":"intake","state":"done","note":"fixed"}]}`)
	h.intakeAdd("and the other one")
	ids2, _ := h.openAsks()
	if !slices.Equal(ids, []string{"intake-1"}) || !slices.Equal(ids2, []string{"intake-2"}) {
		t.Errorf("ids %v then %v", ids, ids2)
	}
}

// The asks tool is listed where asks are kept, and only there: not with
// NoAsks, not in a sub-agent; asksOn reads the same condition back.
func TestAsks_ToolGating(t *testing.T) {
	h, _, _ := startAsks(t)
	t.Cleanup(h.Close)
	if !slices.Contains(h.ToolNames(), asksToolName) || !h.asksOn() {
		t.Fatalf("root: tools %v", h.ToolNames())
	}
	dir := t.TempDir()
	off := startLedger(t, &nudgeRunner{}, dir, filepath.Join(dir, "s.jsonl"), func(c *Config) { c.NoAsks = true })
	t.Cleanup(off.Close)
	if slices.Contains(off.ToolNames(), asksToolName) || off.asksOn() || off.intakeAdd("hi") {
		t.Errorf("NoAsks: tools %v", off.ToolNames())
	}
	if child := (&Harness{parent: h}); child.asksWanted() {
		t.Error("a sub-agent would be offered the asks tool")
	}
}
