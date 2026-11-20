package harness

import (
	"context"
	"testing"

	"github.com/iodesystems/agentkit/taskgraph"
)

// A child that owns a blocked plan node says why in the agent list, and a
// change to the plan pushes the list again so the UI row follows.
func TestAgents_BlockedNodeSaysWhy(t *testing.T) {
	h := planHarness(t, false)
	h.agents = map[int]*subAgent{
		1: {num: 1, parent: h, state: agentIdle},
		2: {num: 2, parent: h, state: agentRunning},
	}
	var pushed []AgentInfo
	h.cfg.OnAgents = func(as []AgentInfo) { pushed = as }
	planCall(t, h, "plan_add", `{"title":"deploy","done_when":"live"}`)
	id := planIDs(h)["deploy"]
	if err := (planStore{h}).Update(context.Background(), func(g *taskgraph.Graph) error {
		g.Get(id).Owner = planOwner(1)
		return g.SetStatus(id, taskgraph.Blocked, "needs the deploy token")
	}); err != nil {
		t.Fatal(err)
	}
	if len(pushed) != 2 || pushed[0].Blocked != "needs the deploy token" || pushed[1].Blocked != "" {
		t.Fatalf("pushed %+v, want agent 1 blocked with the why, agent 2 not", pushed)
	}
}
