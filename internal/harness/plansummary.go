package harness

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/dun/internal/numfmt"
)

// /plan summary, and the s key on the /plan screen (USER 2026-11-17): the
// session's model, in one side call (kind "plan_summary"), says where the work
// stands in three parts, Done / Doing / Next, from the plan and the recent
// part of the session. It takes no turn lock (askOnce): a running turn is not
// interrupted and the summary does not wait for it. The last summary is kept
// in memory and rides the plan event, so every open /plan screen shows it
// and "summarizing…" while one runs. Not kept across a restart.

// Bounds on a summary's prompt: the recent session, newest kept.
const (
	planSumEntryMax  = 800
	planSumRecentMax = 12000
)

// PlanSummary is the last /plan summary.
type PlanSummary struct {
	Text    string
	At      time.Time
	Running bool
	Err     string
	// Tokens is what the call read and wrote, when the provider said.
	Tokens   int
	Cost     float64
	Currency string
	Costed   bool
}

type planSummaryState struct {
	mu  sync.Mutex
	cur PlanSummary
}

// PlanSummaryNow is the last summary, and whether one is running.
func (h *Harness) PlanSummaryNow() PlanSummary {
	h.planSum.mu.Lock()
	defer h.planSum.mu.Unlock()
	return h.planSum.cur
}

// Made is the line that says when the summary was made and what it cost.
func (s PlanSummary) Made() string {
	if s.At.IsZero() {
		return ""
	}
	parts := []string{"made " + s.At.Local().Format("15:04:05")}
	if s.Tokens > 0 {
		parts = append(parts, numfmt.TokK(s.Tokens)+" tok")
	}
	if s.Costed {
		parts = append(parts, numfmt.TurnMoney(s.Cost, s.Currency))
	}
	return strings.Join(parts, " · ")
}

const planSummaryInstruction = `Below are a coding agent's task plan and the most recent part of its session. Say where the work stands, for the person who owns it.

Exactly three sections, as markdown "##" headings in this order: Done; Doing; Next. Under each, at most four short bullets ("- "); "- nothing" when there is none.
Rules:
- Only what the plan and the record show. Never invent a file, command, result or step.
- Name files, commands and plan nodes (by id) as they appear.
- Plain and short: no preamble, no closing remarks.
- Reply with only the three sections.

`

// planSummaryPrompt is the side call's one message: the plan as text, then
// the session's latest entries, newest kept, at most planSumRecentMax runes.
func (h *Harness) planSummaryPrompt(ctx context.Context) string {
	var b strings.Builder
	b.WriteString(planSummaryInstruction)
	g, _ := planStore{h}.View(ctx)
	if len(g.Nodes) > 0 {
		b.WriteString("The plan:\n" + g.List() + "\n\n")
	} else {
		b.WriteString("The plan: empty.\n\n")
	}
	b.WriteString("The recent session (oldest first):\n")
	var entries []agent.Entry
	if h.store != nil {
		entries, _ = h.store.Context(ctx, "dun")
		sortByTime(entries)
	}
	lines := tailLines(transcriptLines(entries, planSumEntryMax), planSumRecentMax)
	if len(lines) == 0 {
		b.WriteString("(nothing yet)\n")
	}
	for _, l := range lines {
		b.WriteString(l + "\n")
	}
	return b.String()
}

// tailLines is the last lines that fit in max runes, with a line first that
// says how many earlier ones were left out.
func tailLines(lines []string, max int) []string {
	used, i := 0, len(lines)
	for i > 0 {
		n := utf8.RuneCountInString(lines[i-1]) + 1
		if used+n > max && i < len(lines) {
			break
		}
		used += n
		i--
	}
	if i == 0 {
		return lines
	}
	return append([]string{fmt.Sprintf("(%d earlier entries left out)", i)}, lines[i:]...)
}

// SummarizePlan runs one /plan summary and keeps it; a second asked while
// one runs is refused. The plan event goes out when it starts and when it
// ends, so a screen shows "summarizing…" and then the summary.
func (h *Harness) SummarizePlan(ctx context.Context) (PlanSummary, error) {
	h.planSum.mu.Lock()
	if h.planSum.cur.Running {
		h.planSum.mu.Unlock()
		return PlanSummary{}, fmt.Errorf("a summary is being made already")
	}
	h.planSum.cur.Running = true
	h.planSum.mu.Unlock()
	h.planChanged()

	text, usage, err := h.askOnce(ctx, "plan_summary", h.planSummaryPrompt(ctx))
	if err == nil && text == "" {
		err = fmt.Errorf("the model wrote no summary")
	}
	h.planSum.mu.Lock()
	s := h.planSum.cur
	s.Running = false
	if err != nil {
		s.Err = err.Error() // the last good summary stays
	} else {
		s = PlanSummary{Text: text, At: time.Now()}
		if usage != nil {
			s.Tokens = usage.PromptTokens + usage.CompletionTokens
			if usage.Cost.Reported {
				s.Cost, s.Currency, s.Costed = usage.Cost.Amount, usage.Cost.Currency, true
			}
		}
	}
	h.planSum.cur = s
	h.planSum.mu.Unlock()
	h.planChanged()
	return s, err
}
