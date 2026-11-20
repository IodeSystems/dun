package harness

import (
	"os"
	"path/filepath"
	"testing"
)

// A cleared child stays hidden across a session resume: the flag is in its
// sidecar, which restoreAgents reads. One not cleared comes back as before.
func TestClearDoneAgents_SurvivesResume(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "s.jsonl")
	h := &Harness{agents: map[int]*subAgent{}}
	h.cfg.SessionFile = base
	for _, num := range []int{1, 2} {
		path := filepath.Join(dir, "s.sub"+string(rune('0'+num))+".jsonl")
		if err := os.WriteFile(path, []byte(""), 0o600); err != nil {
			t.Fatal(err)
		}
		sa := &subAgent{num: num, parent: h, state: agentDismissed, restore: path}
		h.agents[num] = sa
		sa.saveState()
	}
	// Clear only #1: #2 is made stopped after the clear, so it is not hidden.
	h.agents[2].state = agentIdle
	if n := h.ClearDoneAgents(); n != 1 {
		t.Fatalf("cleared %d, want 1", n)
	}
	h.agents[2].state = agentDismissed
	h.agents[2].saveState()

	// A new engine on the same session file.
	h2 := &Harness{}
	h2.cfg.SessionFile = base
	h2.restoreAgents()
	var ids []int
	for _, a := range h2.Agents() {
		ids = append(ids, a.ID)
	}
	if len(ids) != 1 || ids[0] != 2 {
		t.Errorf("after resume listed %v, want only [2]", ids)
	}
	if h2.agentByID(1) == nil {
		t.Errorf("the cleared child must stay reachable by id")
	}
	if !loadAgentState(h2.agentByID(1).restore).Cleared {
		t.Errorf("the sidecar lost the flag")
	}
}
