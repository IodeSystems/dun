package dun

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/iodesystems/agentkit/agent"
)

// The turn budget, as the model hears about it (USER, 2026-10-23).
//
// The clock is a cmd/dun concern (turnclock.go): it decides WHEN. This file is
// WHAT the model reads, and through which path, because the path is the
// harness's. Before this, the budget running out cancelled the turn wherever it
// was, and the model never learned why its work had stopped — a 30-minute
// turn's last step lost, with no chance to commit it or say where it was.
//
// Two marks:
//
//  1. Budget spent: NoteBudget queues a notice. The Session's preparer appends
//     it at the top of the next round, so it reaches the model with its NEXT
//     call — a tool call already running is not disturbed, and a notice never
//     lands between a call and its result. A grace period starts.
//  2. Grace spent: NoteBudgetCut records what the cut interrupts, and the clock
//     cancels the turn with BudgetCut as the cause. The harness treats that
//     cause as an interrupt (see interrupted), so the path that already records
//     the partial reply, answers a cut tool call and leaves a note for the next
//     turn does the same here — and the note says it was the budget.

// BudgetCut is why a turn ended when its budget and grace ran out.
//
// It is carried as the context's cancellation CAUSE rather than returned,
// because nothing between the timer and the error site touches the clock: the
// turn fails somewhere inside the agent loop, and what arrives there is the
// plain "context canceled" that a cancelled context always produces. Ctrl-C
// produces exactly the same two words, so before this the two were
// indistinguishable at every place that reports a failed turn — which is how a
// 30-minute clock running out read as an unexplained cancellation, and sent
// anyone debugging one looking for a crash that never happened.
//
// It is the cause the clock cancels with, so it is in place before anything in
// the turn sees the cancellation: a cut made by a second call after the cancel
// would race the turn's own unwinding.
type BudgetCut struct{ Budget, Grace time.Duration }

func (e BudgetCut) Error() string {
	if e.Grace <= 0 {
		return fmt.Sprintf("the turn ran past its %s budget (--timeout) and was cut off", BudgetDur(e.Budget))
	}
	return fmt.Sprintf("the turn ran past its %s budget plus %s grace (--timeout, --timeout-grace) and was cut off",
		BudgetDur(e.Budget), BudgetDur(e.Grace))
}

// reason is the cut as a tool result names it.
func (e BudgetCut) reason() string {
	if e.Grace <= 0 {
		return fmt.Sprintf("the turn ran past its %s budget", BudgetDur(e.Budget))
	}
	return fmt.Sprintf("the turn ran past its %s budget plus %s grace", BudgetDur(e.Budget), BudgetDur(e.Grace))
}

// budgetTag marks the budget notice. It is about the turn it was sent in, in
// the present tense ("finish now"), so the next turn drops it like an overflow
// hint (see clearBudgetNotes).
const budgetTag = "budget"

// BudgetDur is a budget as a person writes it: 30m, 1m30s, 2h.
func BudgetDur(d time.Duration) string {
	s := d.Round(time.Second).String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// budgetNotice is what the model reads when the budget is spent.
func budgetNotice(budget, grace time.Duration) string {
	return fmt.Sprintf("[dun] This turn has used its %s time budget. dun cuts the turn off in %s. "+
		"Finish or checkpoint the current step now: commit (a WIP commit if it is not done) and note "+
		"where you are. Then summarize what is done and what is left, and end the turn.",
		BudgetDur(budget), BudgetDur(grace))
}

// NoteBudget queues the budget notice for the model's next call in the running
// turn. Safe from any goroutine (the clock's timer calls it).
func (h *Harness) NoteBudget(budget, grace time.Duration) {
	h.noteMu.Lock()
	h.budgetNote = budgetNotice(budget, grace)
	h.noteMu.Unlock()
}

// withBudgetNotice is the preparer the Session runs: the queued budget notice,
// once, then inner (nil when nothing else prepares).
//
// The preparer, and not the Notify/Aside buffer: those ride the next TOOL
// RESULT, and a model that is writing prose when the budget runs out has no
// tool result coming. The preparer runs before every model call.
func (h *Harness) withBudgetNotice(inner agent.NotificationPreparer) agent.NotificationPreparer {
	return agent.PreparerFunc(func(ctx context.Context, sessionID string) error {
		h.noteMu.Lock()
		note := h.budgetNote
		h.budgetNote = ""
		h.noteMu.Unlock()
		if note != "" {
			h.store.appendSilent(agent.Entry{
				ID: uuid.New().String(), Kind: agent.KindNotification, Tag: budgetTag,
				Content: note, CreatedAt: time.Now().UnixNano(),
			})
		}
		if inner == nil {
			return nil
		}
		return inner.PrepareNotifications(ctx, sessionID)
	})
}

// clearBudgetNotes drops the last turn's budget notice, delivered or not. Per
// TURN, not per attempt: a retry inside the turn still needs it.
func (h *Harness) clearBudgetNotes() {
	h.noteMu.Lock()
	h.budgetNote = ""
	h.noteMu.Unlock()
	h.store.dropTagged(budgetTag)
}

// NoteBudgetCut records what the running turn is doing as its budget cut
// lands, for the note the next turn reads. Call it just BEFORE cancelling the
// turn with BudgetCut: what is running is only knowable before the cut ends it.
// The turn then returns ErrInterrupted.
func (h *Harness) NoteBudgetCut(budget, grace time.Duration) {
	c := BudgetCut{budget, grace}
	spent := BudgetDur(budget) + " budget"
	if grace > 0 {
		spent += " plus " + BudgetDur(grace) + " grace"
	}
	lead := fmt.Sprintf("[dun] Your previous turn was cut off after its %s, during %s", spent, h.cutDuring())
	h.intMu.Lock()
	h.intReason, h.intLead = c.reason(), lead
	h.intMu.Unlock()
	h.interrupting.Store(true)
}

// cutDuring names what the turn was doing: the tool calls with no result yet
// (persisted before they run, so these are the running ones), else the model
// step being streamed.
func (h *Harness) cutDuring() string {
	entries, _ := h.store.Context(context.Background(), "dun")
	answered := map[string]bool{}
	for _, e := range entries {
		if e.Kind == agent.KindToolResult && e.ToolCallID != "" {
			answered[e.ToolCallID] = true
		}
	}
	var running []string
	for _, e := range entries {
		if e.Kind == agent.KindToolCall && e.ToolCallID != "" && !answered[e.ToolCallID] {
			running = append(running, e.ToolName)
		}
	}
	switch len(running) {
	case 0:
	case 1:
		return "the " + running[0] + " call"
	default:
		return "the " + strings.Join(running, ", ") + " calls"
	}
	if _, _, _, done := h.round.snapshot(); !done {
		return "a model step (your reply was being generated)"
	}
	return "a step between model calls"
}
