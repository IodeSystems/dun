package dun

import (
	"fmt"
	"strings"
	"time"

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

// foldRestoreCap is how much of a child's task, or a job's command, the
// after-fold notice quotes. The task is why the child exists; a one-line
// excerpt (inFlight's 160) is not enough to act on, and the whole of a long
// brief would cost every fold its size again.
const foldRestoreCap = 1000

// foldRestoreNote is what the model is told right after a fold: the work that
// is still alive, with each one's id, start time, how long it has run, and the
// command or task that started it. The summary may have lost all of it — the
// fold is TOLD what is in flight (foldTask), but nothing makes it write that
// down — and a model that does not know a job is running starts it again.
// Empty when nothing is in flight.
func (h *Harness) foldRestoreNote() string {
	now := time.Now()
	var lines []string
	for _, j := range h.bgJobList() {
		j.mu.Lock()
		done, started, written, command := j.done, j.started, j.written, j.command
		j.mu.Unlock()
		if done {
			continue
		}
		lines = append(lines, fmt.Sprintf("- job #%d — RUNNING now · started %s · running %s · %d bytes out — `%s`",
			j.id, started.Format("15:04:05"), roundDur(now.Sub(started)), written, oneLine(command, foldRestoreCap)))
	}
	for _, sa := range h.agentList() {
		state, status, _, since := sa.snapshot()
		if state == agentDismissed {
			continue
		}
		sa.mu.Lock()
		started, prompt := sa.started, sa.prompt
		sa.mu.Unlock()
		what := string(state)
		if state == agentRunning {
			what = "RUNNING now"
		}
		line := fmt.Sprintf("- agent #%d — %s", sa.num, what)
		if !started.IsZero() {
			// since is this run's length: still growing while running, fixed
			// once the run ended (snapshot) — so "ran", not "idle for".
			verb := "ran"
			if state == agentRunning {
				verb = "running"
			}
			line += fmt.Sprintf(" · started %s · %s %s", started.Format("15:04:05"), verb, roundDur(since))
		}
		if status != "" {
			line += " · " + status
		}
		lines = append(lines, line+" — task: "+oneLine(prompt, foldRestoreCap))
	}
	if len(lines) == 0 {
		return ""
	}
	return "Context was just compacted. This work is still alive; do not start it again. " +
		"jobs(...) reports on any of it.\n" + strings.Join(lines, "\n")
}
