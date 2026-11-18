package harness

import (
	"context"
	"testing"

	"github.com/iodesystems/agentkit/taskgraph"
)

// Done nodes: once per ask. A second idle with the same open asks adds
// none; a new ask gets its own batch.
func TestSuggestDone_OncePerAsk(t *testing.T) {
	reply := `{` + sugsOnly + `,"done":[{"title":"Wrote the parser tests","under":""}]}`
	h, r := idleHarness(t, reply, []string{"Parse the file"}, true)
	names := []string{taskgraph.ToolAdd}
	h.toolNames.Store(&names)
	if !h.askOpen("write the parser tests") {
		t.Fatal("no ask")
	}
	h.planMarkTurn()
	ctx := context.Background()
	if _, err := h.Suggestions(ctx); err != nil || !r.asksDone() {
		t.Fatalf("first idle: %v, asked for done %v", err, r.asksDone())
	}
	n := len(planGraph(t, h).Nodes)
	r.reply = `{` + sugsOnly + `,"done":[{"title":"Ran the parser tests","under":""}]}`
	h.planMarkTurn() // a turn that changed nothing
	if _, err := h.Suggestions(ctx); err != nil || r.asksDone() {
		t.Fatalf("second idle, same asks: %v, asked for done %v", err, r.asksDone())
	}
	if got := len(planGraph(t, h).Nodes); got != n {
		t.Errorf("a second batch for the same ask: %d nodes, want %d", got, n)
	}
	if !h.askOpen("now the lexer") {
		t.Fatal("no second ask")
	}
	h.planMarkTurn()
	if _, err := h.Suggestions(ctx); err != nil || !r.asksDone() {
		t.Fatalf("a new ask: %v, asked for done %v", err, r.asksDone())
	}
	if got := len(planGraph(t, h).Nodes); got != n+2 {
		t.Errorf("the new ask's batch: %d nodes, want %d", got, n+2)
	}
}
