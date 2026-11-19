package harness

import (
	"context"
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
	return !strings.Contains(h.asksCall(`{"asks":[{"id":"intake","state":"open"}]}`), "ERROR")
}

func askRecOf(t *testing.T, h *Harness, node string) AskLedger {
	t.Helper()
	h.asks.mu.Lock()
	defer h.asks.mu.Unlock()
	r := h.askByNodeLocked(node)
	if r == nil {
		t.Fatalf("no ask at node %s", node)
	}
	return r.AskLedger
}

// planAsks is the plan's ask rows by node id, and which work.
func planAsks(h *Harness) (map[string]PlanRow, []string) {
	rows := map[string]PlanRow{}
	var working []string
	for _, r := range h.Plan().Rows {
		if r.Ask != nil {
			rows[r.ID] = r
			if r.Ask.Working {
				working = append(working, r.ID)
			}
		}
	}
	return rows, working
}

// Messages grow one intake; an automatic one does not. One call splits it
// into three asks and merges a part into an existing ask. The intake is
// then empty, one ask works, and the next message starts a new intake.
// Another call marks several asks mid-turn. All of it survives a restart.
func TestAsks_IntakeSplitMergeMarkAndRestart(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	dir := t.TempDir()
	sf := filepath.Join(dir, "s.jsonl")
	h := startLedger(t, &nudgeRunner{}, dir, sf, nil)
	if !h.askOpen("test the login flow") {
		t.Fatal("a short real message did not become an ask")
	}
	if rows, _ := planAsks(h); len(rows) != 1 || rows["1"].Title != "ask: test the login flow" {
		t.Fatalf("rows %+v", rows)
	}
	h.ledgerBegin()
	h.intakeAdd("Add plan items:\n1. locate tow yards\n2. vds: port 18020 is in use\n3. vehicle telemetry")
	h.SayAs(NewMessageID(), "retry")
	h.SayAs(NewMessageID(), "[automatic] resume after the upgrade")
	in := h.Plan().Intake
	if in == nil || len(in.Msgs) != 2 || in.Msgs[1].Text != "retry" || !in.Working {
		t.Fatalf("intake %+v", in)
	}
	if rows, _ := planAsks(h); len(rows) != 1 {
		t.Fatalf("a message opened an ask: %+v", rows)
	}
	out := h.asksCall(`{"asks":[{"id":"intake","split":[
		{"title":"locate tow yards","text":"1. locate tow yards"},
		{"title":"free port 18020 for vds","text":"2. vds: port 18020 is in use"},
		{"title":"investigate vehicle telemetry","text":"3. vehicle telemetry"}]},
		{"id":"intake","merge_into":"1","text":"retry"}]}`)
	if strings.Contains(out, "ERROR") {
		t.Fatal(out)
	}
	if h.Plan().Intake != nil {
		t.Fatalf("the intake was not emptied: %+v", h.Plan().Intake)
	}
	rows, working := planAsks(h)
	if len(rows) != 4 || rows["2"].Title != "ask: locate tow yards" || rows["4"].Ask.State != "open" {
		t.Fatalf("rows %+v", rows)
	}
	if !slices.Equal(working, []string{"2"}) {
		t.Errorf("working %v, want only 2", working)
	}
	a2 := askRecOf(t, h, "2")
	if a2.Text != "1. locate tow yards" || len(a2.Source) != 2 || !strings.Contains(a2.Source[0].Text, "vehicle telemetry") {
		t.Errorf("split ask %+v", a2)
	}
	if a1 := askRecOf(t, h, "1"); len(a1.Continued) != 1 || a1.Continued[0].Text != "retry" {
		t.Errorf("merged into ask 1: %+v", a1.Continued)
	}
	// Several asks at once, mid-turn: done, filed, reworded.
	planCall(t, h, "plan_add", `{"title":"write the vds port fix","done_when":"vds starts"}`)
	out = h.asksCall(`{"asks":[{"id":"2","state":"done","note":"three yards listed"},
		{"id":"3","state":"filed","nodes":["5"]},
		{"id":"4","title":"vehicle telemetry research"},
		{"id":"9","state":"done","note":"x"}]}`)
	if !strings.Contains(out, "ask 3: filed → 5") || !strings.Contains(out, "ERROR: 9") {
		t.Errorf("result:\n%s", out)
	}
	rows, _ = planAsks(h)
	if rows["2"].Ask.State != "done" || rows["3"].Ask.State != "filed" || !slices.Equal(rows["3"].Ask.Filed, []string{"5"}) {
		t.Errorf("states %+v %+v", rows["2"].Ask, rows["3"].Ask)
	}
	if a4 := askRecOf(t, h, "4"); a4.Title != "vehicle telemetry research" || a4.Text != "3. vehicle telemetry" || rows["4"].Title != "ask: vehicle telemetry research" {
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
	// The next message starts a new intake.
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
	rows, _ = planAsks(h2)
	if len(rows) != 4 || rows["3"].Ask.State != "filed" || rows["4"].Title != "ask: vehicle telemetry research" {
		t.Fatalf("asks after restart %+v", rows)
	}
	if a1 := askRecOf(t, h2, "1"); len(a1.Continued) != 1 {
		t.Errorf("ask 1 after restart: %+v", a1)
	}
}

// Merging into a done ask reopens it: the person carried on with it.
func TestAsks_MergeReopensDone(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	dir := t.TempDir()
	h := startLedger(t, &nudgeRunner{}, dir, filepath.Join(dir, "s.jsonl"), nil)
	t.Cleanup(h.Close)
	h.askOpen("fix the flaky test")
	h.asksCall(`{"asks":[{"id":"1","state":"done","note":"retry added"}]}`)
	h.ledgerBegin()
	h.ledgerEnd(context.Background(), nil)
	if a := askRecOf(t, h, "1"); a.Outcome != "done" {
		t.Fatalf("not done: %+v", a)
	}
	h.intakeAdd("still flaky")
	out := h.asksCall(`{"asks":[{"id":"intake","merge_into":"1"}]}`)
	if !strings.Contains(out, "reopened (it was done)") {
		t.Errorf("result %q", out)
	}
	if a := askRecOf(t, h, "1"); a.Outcome != "" || !a.Closed.IsZero() {
		t.Errorf("record not reopened: %+v", a)
	}
	rows, working := planAsks(h)
	if rows["1"].Status != "doing" || rows["1"].Ask.State != "open" || !slices.Equal(working, []string{"1"}) {
		t.Errorf("row %+v working %v", rows["1"], working)
	}
	if !strings.Contains(rows["1"].Details, "still flaky") {
		t.Errorf("details %q", rows["1"].Details)
	}
}

// The turn-end check names the intake while it has messages, and the asks
// the turn touched that it did not mark; nothing else, and nothing in an
// automatic turn.
func TestAsks_TurnEndCheckListsOnlyWhatIsLeft(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	dir := t.TempDir()
	h := startLedger(t, &nudgeRunner{}, dir, filepath.Join(dir, "s.jsonl"), nil)
	t.Cleanup(h.Close)
	h.askOpen("fix the flaky test")
	h.askOpen("update the docs")
	h.turnBegin(originPerson)
	h.ledgerBegin()
	if a := h.idleAsks(); a != "" {
		t.Errorf("nothing touched, yet: %q", a)
	}
	h.intakeAdd("continue")
	a := h.idleAsks()
	if !strings.Contains(a, `- intake: 1 message, not acknowledged: "continue"`) || strings.Contains(a, "ask: ") {
		t.Errorf("check %q", a)
	}
	t.Logf("intake check: %d bytes", len(a))
	h.asksCall(`{"asks":[{"id":"intake","merge_into":"1"}]}`)
	planCall(t, h, "plan_add", `{"title":"write the page","done_when":"written","needs":["2"]}`)
	a = h.idleAsks()
	if !strings.Contains(a, "- 1 ask: fix the flaky test") || !strings.Contains(a, "- 2 ask: update the docs") || strings.Contains(a, "intake") {
		t.Errorf("check %q", a)
	}
	t.Logf("two asks check: %d bytes", len(a))
	h.asksCall(`{"asks":[{"id":"1","state":"done","note":"fixed"},{"id":"2","state":"open"}]}`)
	if a := h.idleAsks(); a != "" {
		t.Errorf("all marked: %q", a)
	}
	if note, _ := h.idleNote(1); strings.Contains(note, "ask:") {
		t.Errorf("asks among the open nodes: %q", note)
	}
	h.ledgerEnd(context.Background(), nil)
	h.turnBegin(originAuto)
	h.ledgerBegin()
	h.intakeAdd("ignored here") // as if typed; an automatic turn is not asked
	if a := h.idleAsks(); a != "" {
		t.Errorf("automatic turn: %q", a)
	}
	h.ledgerEnd(context.Background(), nil)
}

// Where asks are kept the fold's ask is the asks' (the intake here), and no
// "Current ask" node is written; an old one is dropped.
func TestAsks_FoldAskFromTheAsks(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	dir := t.TempDir()
	h := startLedger(t, &nudgeRunner{}, dir, filepath.Join(dir, "s.jsonl"), nil)
	t.Cleanup(h.Close)
	_ = planStore{h}.Update(context.Background(), func(g *taskgraph.Graph) error {
		n, err := g.Add(taskgraph.Node{Title: "Current ask: continue", Details: "continue\n\n" + askTag, DoneWhen: "x"})
		if err != nil {
			return err
		}
		return g.SetStatus(n.ID, taskgraph.Doing, "")
	})
	h.intakeAdd("the tools and stubs dir is not what I expect")
	h.intakeAdd("continue")
	a := h.noteAsk(context.Background())
	if a.text != "the tools and stubs dir is not what I expect" || len(a.later) != 1 || a.later[0].text != "continue" {
		t.Errorf("fold ask %+v", a)
	}
	g := planGraph(t, h)
	for _, n := range g.Nodes {
		if strings.HasPrefix(n.Title, "Current ask") && planOpen(n) {
			t.Errorf("open fold node %+v", n)
		}
	}
	// Once acknowledged: the ask the turn works, with what was merged.
	h.asksCall(`{"asks":[{"id":"intake","split":[{"title":"fix the stubs dir","text":"the tools and stubs dir is not what I expect"}]}]}`)
	h.intakeAdd("retry")
	h.asksCall(`{"asks":[{"id":"intake","merge_into":"2"}]}`)
	a = h.noteAsk(context.Background())
	if a.text != "the tools and stubs dir is not what I expect" || len(a.later) != 1 || a.later[0].text != "retry" {
		t.Errorf("fold ask after ack %+v", a)
	}
}
