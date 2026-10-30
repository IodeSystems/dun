package dun

import (
	"strings"

	"github.com/iodesystems/agentkit/llm"
)

// What a fold is asked for (USER 2026-10-30: "500 tokens doesn't sound like
// much. It should be tuned to the pressure, and it should be with thinking
// off"). Both folds — agentkit's under budget pressure, and the rescue fold
// when the endpoint overflows (rescue.go) — size their summary from what they
// fold (agent.DefaultSummaryTokens), cap the completion to match, send
// thinking off, and are told what the session is for and what is in flight.

// foldOpts are a fold call's request options: thinking off, as /effort off
// sends it. A summarizer that thinks spends its budget before it writes; on
// a server whose default thinks, that was thousands of tokens per fold.
func foldOpts() *llm.ChatOpts {
	o := &llm.ChatOpts{}
	applyEffort(o, EffortOff)
	return o
}

// foldTask is what a fold is told the session is for (agent.Shaper.TaskFunc):
// the session state and the work in flight, from dun's own records rather
// than from the conversation being folded, which is what loses them. Empty
// when there is neither: agentkit then uses the system prompt.
func (h *Harness) foldTask() string {
	var parts []string
	if st := strings.TrimSpace(renderState(h.currentState())); st != "" {
		parts = append(parts, st)
	}
	if f := h.inFlight(); f != "" {
		parts = append(parts, "# In flight\n"+f)
	}
	return strings.Join(parts, "\n\n")
}

// inFlight is the work still running, one line each: background jobs not
// finished, and children not dismissed with their task.
func (h *Harness) inFlight() string {
	var lines []string
	for _, j := range h.bgJobList() {
		j.mu.Lock()
		done := j.done
		j.mu.Unlock()
		if !done {
			lines = append(lines, "- job "+j.status())
		}
	}
	for _, sa := range h.agentList() {
		if state, _, _, _ := sa.snapshot(); state == agentDismissed {
			continue
		}
		line := "- agent " + sa.line()
		if w := sa.where(); w != "" {
			line += " on " + w
		}
		lines = append(lines, line+" — task: "+oneLine(sa.prompt, 160))
	}
	return strings.Join(lines, "\n")
}
