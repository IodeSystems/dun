package harness

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/agentkit/taskgraph"
)

// ClearItems lists the open asks and the open plan nodes; the intake
// (unacknowledged messages), the fold's ask node and closed items are not
// listed. DropOpenWork drops each and empties the intake.
func TestClearItemsAndDrop(t *testing.T) {
	h, _, _ := startAsks(t)
	if !h.askOpen("fix the flaky test") {
		t.Fatal("ask not opened")
	}
	h.intakeAdd("and the docs")
	h.addPlanNode(t, "write the docs")
	h.addPlanNode(t, "done already")
	_ = planStore{h}.Update(context.Background(), func(g *taskgraph.Graph) error {
		for _, n := range g.Nodes {
			if n.Title == "done already" {
				return g.SetStatus(n.ID, taskgraph.Dropped, "test")
			}
		}
		return nil
	})
	h.planAsk(context.Background(), foldAsk{text: "the fold's ask"}) // the conversation's, not listed

	var lines []string
	for _, it := range h.ClearItems() {
		lines = append(lines, ClearItemLine(it))
	}
	got := strings.Join(lines, "\n")
	for _, want := range []string{"ask 1 ", "write the docs"} {
		if !strings.Contains(got, want) {
			t.Errorf("ClearItems lacks %q:\n%s", want, got)
		}
	}
	if len(lines) != 2 || strings.Contains(got, "done already") || strings.Contains(got, "fold") || strings.Contains(got, "and the docs") {
		t.Errorf("ClearItems lists more than the open items:\n%s", got)
	}

	h.DropOpenWork(ClearDroppedWhy)
	if items := h.ClearItems(); len(items) != 0 {
		t.Errorf("after the drop: %+v", items)
	}
	if a := askRecOf(t, h, 1); a.Outcome != "dropped" || a.Note != ClearDroppedWhy {
		t.Errorf("ask 1 = %s (%s), want dropped", a.Outcome, a.Note)
	}
	if b := h.asksBlock(); b != "" {
		t.Errorf("the model is still shown asks: %q", b)
	}
}

// DropIntake empties the intake and leaves the acknowledged asks; an intake
// alone is no item.
func TestDropIntake(t *testing.T) {
	h, _, _ := startAsks(t)
	h.intakeAdd("never acknowledged")
	if items := h.ClearItems(); len(items) != 0 {
		t.Errorf("intake listed: %+v", items)
	}
	if !h.askOpen("fix the flaky test") {
		t.Fatal("ask not opened")
	}
	h.intakeAdd("another, unacknowledged")
	h.DropIntake()
	items := h.ClearItems()
	if len(items) != 1 || items[0].Kind != "ask" {
		t.Errorf("items = %+v, want the ask alone", items)
	}
	h.asks.mu.Lock()
	n := len(h.asksLocked().Intake)
	h.asks.mu.Unlock()
	if n != 0 {
		t.Errorf("intake still has %d messages", n)
	}
}

// StopJobsForClear kills every running job and mutes it first, so no
// completion notice reaches the cleared conversation.
func TestStopJobsForClear(t *testing.T) {
	h := testHarness(t)
	j := h.newJobStarted(HostExec{Dir: t.TempDir()}, "sleep 30")
	if n := h.StopJobsForClear("user"); n != 1 {
		t.Fatalf("stopped %d, want 1", n)
	}
	select {
	case <-j.doneCh:
	case <-time.After(5 * time.Second):
		t.Fatal("the job did not stop")
	}
	if info := j.info(); info.State != "killed" || !info.Muted {
		t.Errorf("job = %s muted %v, want killed and muted", info.State, info.Muted)
	}
	for _, n := range h.notes() {
		if strings.Contains(n, "sleep 30") {
			t.Errorf("a stopped job's notice was queued: %q", n)
		}
	}
}

// The note the new conversation reads: each kept job by number, command and
// start, each kept item; nothing kept, no note.
func TestClearKeptNote(t *testing.T) {
	if ClearKeptNote(nil, nil) != "" {
		t.Error("nothing kept must say nothing")
	}
	at := time.Date(2026, 11, 24, 14, 2, 0, 0, time.Local)
	got := ClearKeptNote([]JobInfo{{ID: 3, Command: "go test ./...", Started: at.Unix()}},
		[]ClearItem{{Kind: "ask", ID: "2", Title: "fix the flaky test"}, {Kind: "node", ID: "n4", Title: "write the docs"}})
	for _, want := range []string{"[clear] ", "#3 `go test ./...` (started 14:02)", "completion notice", "- ask 2 fix the flaky test", "- plan n4 write the docs"} {
		if !strings.Contains(got, want) {
			t.Errorf("note lacks %q:\n%s", want, got)
		}
	}
}
