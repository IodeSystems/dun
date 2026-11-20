package harness

import "testing"

// ClearDoneAgents hides stopped and dismissed children from the list and
// keeps idle and running ones; every child stays reachable by id.
func TestClearDoneAgents(t *testing.T) {
	var pushed []AgentInfo
	h := &Harness{agents: map[int]*subAgent{}}
	h.cfg.OnAgents = func(as []AgentInfo) { pushed = as }
	for num, st := range map[int]agentState{1: agentDismissed, 2: agentIdle, 3: agentRunning, 4: agentStopped} {
		h.agents[num] = &subAgent{num: num, parent: h, state: st}
	}
	if n := h.ClearDoneAgents(); n != 2 {
		t.Fatalf("cleared %d, want 2", n)
	}
	var ids []int
	for _, a := range h.Agents() {
		ids = append(ids, a.ID)
	}
	if len(ids) != 2 || ids[0] != 2 || ids[1] != 3 {
		t.Errorf("listed %v, want [2 3]", ids)
	}
	if len(pushed) != 2 {
		t.Errorf("pushed %d rows, want 2", len(pushed))
	}
	if h.agentByID(1) == nil || h.agentByID(4) == nil {
		t.Errorf("cleared agents must stay reachable by id")
	}
	if n := h.ClearDoneAgents(); n != 0 {
		t.Errorf("second clear hid %d", n)
	}
}
