package dun

import (
	"context"
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/agent"
)

// A fold is told the session's state and the work in flight, from dun's own
// records: the conversation being folded is what loses them.
func TestFoldTask_StateAndInFlight(t *testing.T) {
	h := startParentOf(t, &scriptRunner{rounds: []roundFn{textRound("done"), textRound("done")}})
	if got := h.foldTask(); got != "" {
		t.Errorf("nothing set and nothing running: %q", got)
	}
	goal := "ship the fold budget"
	h.stateMerge(&goal, nil, []string{"wire rescue"}, nil)
	kept := spawnKid(t, h, "read the build log and say what failed")
	gone := spawnKid(t, h, "an aside that was dismissed")
	waitIdle(t, kept)
	waitIdle(t, gone)
	gone.quit()

	task := h.foldTask()
	for _, want := range []string{"goal: ship the fold budget", "[ ] wire rescue", "# In flight",
		"read the build log and say what failed"} {
		if !strings.Contains(task, want) {
			t.Errorf("fold task lacks %q:\n%s", want, task)
		}
	}
	if strings.Contains(task, "an aside that was dismissed") {
		t.Errorf("a dismissed child is not in flight:\n%s", task)
	}
}

// The rescue fold thinks nothing, is sized to what it folds, is capped to
// match, and keeps what is in flight and what was found.
func TestFoldHistory_SizedAndThinkingOff(t *testing.T) {
	h := newNoteHarness(t)
	r := &summaryRunner{t: t}
	h.client = r
	seedHistory(t, h, 8)
	if _, err := h.foldHistory(context.Background(), foldByOverflow); err != nil {
		t.Fatal(err)
	}
	o := r.lastOpts
	if o == nil || o.EnableThinking == nil || *o.EnableThinking {
		t.Fatalf("the rescue fold must send thinking off: %+v", o)
	}
	if o.MaxTokens < agent.SummaryCap(agent.SummaryMinTokens) {
		t.Errorf("MaxTokens %d: no cap from the budget", o.MaxTokens)
	}
	for _, want := range []string{"IN FLIGHT", "DISCOVERIES", "ruled out"} {
		if !strings.Contains(r.lastUser, want) {
			t.Errorf("the handoff prompt lacks %q", want)
		}
	}
	if strings.Contains(r.lastUser, "<= 800 tokens") {
		t.Error("the handoff is still a fixed 800 tokens")
	}
}

func TestFoldOpts_ThinkingOff(t *testing.T) {
	o := foldOpts()
	if o.EnableThinking == nil || *o.EnableThinking || o.ReasoningEffort != "none" {
		t.Errorf("fold options %+v", o)
	}
}
