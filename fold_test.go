package dun

import (
	"context"
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/agent"
)

// A fold is told the session's plan and the work in flight, from dun's own
// records: the conversation being folded is what loses them.
func TestFoldTask_PlanAndInFlight(t *testing.T) {
	h := startParentOf(t, &scriptRunner{rounds: []roundFn{textRound("done"), textRound("done")}})
	if got := h.foldTask(); got != "" {
		t.Errorf("nothing set and nothing running: %q", got)
	}
	planCall(t, h, `{"action":"add","title":"wire rescue","done_when":"the rescue folds in stages"}`)
	kept := spawnKid(t, h, "read the build log and say what failed")
	gone := spawnKid(t, h, "an aside that was dismissed")
	waitIdle(t, kept)
	waitIdle(t, gone)
	gone.quit()

	task := h.foldTask()
	for _, want := range []string{"# Plan", "wire rescue", "# In flight",
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

// Right after a fold the model is told, from dun's own records, what is still
// alive: each job and child with its id, start time, time running, and the
// command or the whole task (not inFlight's one-line excerpt). The summary is
// not trusted to have kept any of it.
func TestFoldRestoreNote_ListsLiveWork(t *testing.T) {
	h := startParentOf(t, &scriptRunner{rounds: []roundFn{textRound("done"), textRound("done")}})
	h.notes()
	h.noteCompaction(agentCompaction(5, 100, 40))
	if n := h.notes(); len(n) != 0 {
		t.Fatalf("nothing in flight, nothing to say: %q", n)
	}

	running := testJob(h, 1, "make test-integration")
	finished := testJob(h, 2, "make lint")
	finished.finish(ExecResult{})
	h.bgMu.Lock()
	h.bgJobs = map[int]*bgJob{1: running, 2: finished}
	h.bgMu.Unlock()
	long := "read the build log and say what failed. " + strings.Repeat("Then check each module's test output in turn. ", 6) + "END-OF-TASK"
	kept := spawnKid(t, h, long)
	gone := spawnKid(t, h, "an aside that was dismissed")
	waitIdle(t, kept)
	waitIdle(t, gone)
	gone.quit()
	h.notes()

	h.noteCompaction(agentCompaction(5, 100, 40))
	n := h.notes()
	if len(n) != 1 {
		t.Fatalf("want one after-fold notice, got %q", n)
	}
	note := n[0]
	for _, want := range []string{"just compacted", "do not start it again",
		"job #1 — RUNNING now", "· started ", "· running ", "`make test-integration`",
		"agent #1 — idle · started ", " · ran ", "END-OF-TASK"} {
		if !strings.Contains(note, want) {
			t.Errorf("notice lacks %q:\n%s", want, note)
		}
	}
	for _, not := range []string{"make lint", "an aside that was dismissed"} {
		if strings.Contains(note, not) {
			t.Errorf("notice names finished or dismissed work %q:\n%s", not, note)
		}
	}
}
