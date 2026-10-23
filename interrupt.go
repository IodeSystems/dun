package dun

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
)

// Turn interruption: the user stops the turn in flight (a bad line of
// thought, a wrong direction) and the session stays up.
//
// What it does, in order:
//
//  1. Interrupt cancels the CURRENT turn's context with ErrInterrupted as the
//     cause. Only the turn's: the engine, the session and the background jobs
//     stay up. runTurn does not retry a cancelled context, so the cut is final.
//     A turn budget cut arrives the same way, as BudgetCut on the parent
//     context (budget.go), and is recorded the same way.
//  2. The turn returns. finishInterrupt records what the model had produced, so
//     the next turn reads it:
//     - the reply text the cut round had streamed (agentkit persists a round
//       only when its stream completes, so a round cut mid-stream has no entry
//       of its own),
//     - a tool call left without a result, answered as interrupted,
//     - a notification that says the user interrupted, with a SHORT note about
//       the reasoning (its length, duration and last sentence). The reasoning
//       itself is never sent back: many providers reject it as content, and a
//       bad thought is what the user cut.
//  3. Messages the user typed during the turn are NOT lifted into the cut
//     turn's tool results (see interrupting). They stay buffered, so the
//     driver's next pass finds them pending and starts the next turn at once.

// ErrInterrupted is what Ask and Continue return for a turn the user stopped.
// It is not a failure: the transcript is consistent and the next message
// continues from it.
var ErrInterrupted = errors.New("the user interrupted the turn")

// interruptCause is the cut turn's cancellation cause: ErrInterrupted, plus
// who or what stopped it, so a tool call cut short can say so in its result
// ("" is the user).
type interruptCause struct{ reason string }

func (c interruptCause) Error() string {
	if c.reason == "" {
		return ErrInterrupted.Error()
	}
	return "the turn was stopped: " + c.reason
}

func (c interruptCause) Is(target error) bool { return target == ErrInterrupted }

// interruptReason is what stopped ctx's turn: "" for the user, or no cut.
func interruptReason(ctx context.Context) string {
	cause := context.Cause(ctx)
	var c interruptCause
	if errors.As(cause, &c) {
		return c.reason
	}
	var b BudgetCut
	if errors.As(cause, &b) {
		return b.reason()
	}
	return ""
}

// interruptTag marks the notification that records an interrupt.
const interruptTag = "interrupt"

// roundCapture is what the current round has streamed so far. It is reset at
// the start of every round (each ChatStream call of the turn) and marked done
// when the round's stream completes, after which agentkit persists the round
// itself and nothing here needs saving.
type roundCapture struct {
	mu        sync.Mutex
	content   strings.Builder
	reasoning strings.Builder
	started   time.Time
	done      bool
}

func (c *roundCapture) begin() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.content.Reset()
	c.reasoning.Reset()
	c.started, c.done = time.Now(), false
}

func (c *roundCapture) add(ch llm.StreamChunk) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.content.WriteString(ch.Content)
	c.reasoning.WriteString(ch.Reasoning)
	if ch.Done {
		c.done = true
	}
}

// reset forgets the last round, so a turn that is cut before its first
// round does not save the previous turn's text again.
func (c *roundCapture) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.content.Reset()
	c.reasoning.Reset()
	c.started, c.done = time.Time{}, true
}

func (c *roundCapture) snapshot() (content, reasoning string, took time.Duration, done bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.started.IsZero() {
		took = time.Since(c.started)
	}
	return c.content.String(), c.reasoning.String(), took, c.done
}

// interruptState is the Harness's part of this. Embedded so the fields stay
// together and out of the Harness declaration's way.
type interruptState struct {
	intMu     sync.Mutex
	intCancel context.CancelCauseFunc
	// interrupting is set from Interrupt until the next turn starts. While it
	// is set, liftQueued lifts nothing: the cut turn's tool results are the
	// wrong place for a message the user typed, and lifting it there would
	// also take it out of the buffer that starts the next turn.
	interrupting atomic.Bool
	round        roundCapture
	// turnStart is when the current turn began: a job started at or after it
	// belongs to the turn, which is what the interrupt note lists.
	turnStart time.Time
	// cutJobs is the "still running: …" line for the last interrupted turn,
	// for the engine's done event (InterruptedJobs). Guarded by intMu.
	cutJobs string
	// intReason is who or what stopped the turn, for the note the model
	// reads; "" is the user. Guarded by intMu.
	intReason string
	// intLead, when set, opens the note in place of the generic "stopped
	// before it finished": a budget cut says which budget, and during what.
	// Guarded by intMu.
	intLead string
}

// Interrupt stops the turn in flight, if there is one, and reports whether
// there was. Safe from any goroutine (the engine calls it from its reader).
// reason names what stopped it, for the note the model reads (e.g. "dund
// restarted into build X while this turn was running"); "" means the user.
func (h *Harness) Interrupt(reason string) bool {
	h.intMu.Lock()
	defer h.intMu.Unlock()
	if h.intCancel == nil {
		return false
	}
	h.intReason, h.intLead = reason, ""
	h.interrupting.Store(true)
	h.intCancel(interruptCause{reason})
	return true
}

// interruptible is the turn's context, which Interrupt cancels, and the func
// that ends it. A new turn clears the last turn's interrupt.
func (h *Harness) interruptible(ctx context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(ctx)
	h.intMu.Lock()
	h.intCancel = cancel
	h.turnStart, h.cutJobs, h.intReason, h.intLead = time.Now(), "", "", ""
	h.intMu.Unlock()
	h.interrupting.Store(false)
	h.round.reset()
	return ctx, func() {
		h.intMu.Lock()
		h.intCancel = nil
		h.intMu.Unlock()
		cancel(nil)
	}
}

// interrupted says ctx is a turn that was stopped: by the user, by dund, or by
// its budget running out (BudgetCut arrives as the PARENT context's cause).
func interrupted(ctx context.Context) bool {
	cause := context.Cause(ctx)
	var b BudgetCut
	return errors.Is(cause, ErrInterrupted) || errors.As(cause, &b)
}

// finishInterrupt turns a turn the user cut into ErrInterrupted, after
// recording what it had produced. Anything else passes through.
func (h *Harness) finishInterrupt(ctx context.Context, res agent.TurnResult, err error) (agent.TurnResult, error) {
	if err == nil || !interrupted(ctx) {
		return res, err
	}
	h.recordInterrupt(ctx)
	return res, ErrInterrupted
}

// recordInterrupt writes the cut turn's partial work and the marker. Silent
// appends: none of this is a reason to run a turn (the next message is).
func (h *Harness) recordInterrupt(ctx context.Context) {
	content, reasoning, took, done := h.round.snapshot()
	now := time.Now().UnixNano()
	if !done && strings.TrimSpace(content) != "" {
		h.store.appendSilent(agent.Entry{
			ID: uuid.New().String(), Kind: agent.KindAssistant, Content: content,
			// Stored for the log and the history view, never replayed (see
			// agent.Entry.Reasoning).
			Reasoning: reasoning,
			CreatedAt: now,
		})
		now++
	}
	h.intMu.Lock()
	reason, lead := h.intReason, h.intLead
	h.intMu.Unlock()
	if reason == "" {
		reason = interruptReason(ctx) // a budget cut nobody noted
	}
	cut := userInterruptedCall
	note := "[the user interrupted this turn] Your previous response was stopped by the user before it finished"
	if reason != "" {
		cut = func(name string) string { return interruptedCallFor(name, reason) }
		note = "[" + reason + "] Your previous response was stopped before it finished"
	}
	if lead != "" {
		note = lead
	}
	h.healOrphans(ctx, cut, true)
	if !done && strings.TrimSpace(content) != "" {
		note += "; the reply text above is partial, cut where it stands"
	}
	note += "."
	if !done {
		note += reasoningNote(reasoning, took)
	}
	if jobs := h.cutTurnJobs(); len(jobs) > 0 {
		list := make([]string, len(jobs))
		now := time.Now()
		for i, j := range jobs {
			list[i] = JobLine(j, now)
		}
		note += " Background jobs this turn started are STILL RUNNING — the interrupt does not stop them: " +
			strings.Join(list, ", ") + ". The user can stop one with /jobs kill N; leave them running " +
			"unless the user asks you to stop them (exec_monitor kill)."
	}
	switch {
	case reason == "":
		note += " Do not resume or repeat it on your own: the user's next message says what to do."
	case lead != "":
		note += " Check the state before you continue."
	}
	h.store.appendSilent(agent.Entry{
		ID: uuid.New().String(), Kind: agent.KindNotification, Tag: interruptTag,
		Content: note, CreatedAt: time.Now().UnixNano(),
	})
}

// cutTurnJobs is the running jobs the cut turn started. It also records the
// user's line for them (InterruptedJobs).
func (h *Harness) cutTurnJobs() []JobInfo {
	h.intMu.Lock()
	since := h.turnStart
	h.intMu.Unlock()
	if since.IsZero() {
		return nil
	}
	jobs := h.jobsStartedSince(since)
	line := interruptedJobsLine(jobs, time.Now())
	h.intMu.Lock()
	h.cutJobs = line
	h.intMu.Unlock()
	return jobs
}

// interruptedJobsLine is what the user reads under "interrupted": the jobs
// still running and how to stop them. "" when there are none.
func interruptedJobsLine(jobs []JobInfo, now time.Time) string {
	if len(jobs) == 0 {
		return ""
	}
	list := make([]string, len(jobs))
	for i, j := range jobs {
		list[i] = JobLine(j, now)
	}
	hint := fmt.Sprintf("/jobs kill %d to stop it", jobs[0].ID)
	if len(jobs) > 1 {
		hint = fmt.Sprintf("/jobs kill %d to stop one, /jobs kill all for all", jobs[0].ID)
	}
	return "still running: " + strings.Join(list, ", ") + " — " + hint
}

// InterruptedJobs is the "still running: …" line for the last interrupted
// turn, or "" when it left no job running.
func (h *Harness) InterruptedJobs() string {
	h.intMu.Lock()
	defer h.intMu.Unlock()
	return h.cutJobs
}

// reasoningNoteMax bounds the last sentence the note quotes.
const reasoningNoteMax = 200

// reasoningNote is the cut round's reasoning as one short sentence: how long,
// how much, and where it had got to. "" when there was none.
func reasoningNote(reasoning string, took time.Duration) string {
	r := strings.TrimSpace(reasoning)
	if r == "" {
		return ""
	}
	s := fmt.Sprintf(" You had been reasoning for %s (%d chars) when it was cut; that reasoning was discarded",
		took.Round(time.Second), utf8.RuneCountInString(r))
	if last := lastSentence(r, reasoningNoteMax); last != "" {
		s += fmt.Sprintf(". Its last sentence was: %q", last)
	}
	return s + "."
}

// lastSentence is the text's last sentence, at most max runes (its end kept).
func lastSentence(text string, max int) string {
	t := strings.TrimSpace(text)
	t = strings.TrimRight(t, ".!?… \n\t")
	cut := -1
	for i, r := range t {
		if r == '.' || r == '!' || r == '?' || r == '\n' {
			// A terminator followed by space ends a sentence; "3.5" does not.
			if j := i + utf8.RuneLen(r); j >= len(t) || t[j] == ' ' || t[j] == '\n' {
				cut = j
			}
		}
	}
	if cut >= 0 {
		t = t[cut:]
	}
	t = strings.Join(strings.Fields(t), " ")
	if rs := []rune(t); len(rs) > max {
		t = "…" + string(rs[len(rs)-max+1:])
	}
	return t
}

// userInterruptedCall is a cut call's result: what happened, and that its
// effect is unknown.
func userInterruptedCall(name string) string {
	return fmt.Sprintf("ERROR: this %s call was INTERRUPTED by the user before its result was "+
		"recorded. Whether it took effect is unknown: verify the current state before repeating "+
		"it, and repeat it only if the user's next message still wants it.", name)
}

// interruptedCallFor is a cut call's result when something other than the
// user stopped the turn (reason says what).
func interruptedCallFor(name, reason string) string {
	return fmt.Sprintf("ERROR: this %s call was INTERRUPTED before its result was recorded (%s). "+
		"Whether it took effect is unknown: verify the current state before repeating it.", name, reason)
}

// withInterrupt is the outermost dispatcher: a call the turn reaches after the
// interrupt is not run, and one the interrupt cut short says so.
func withInterrupt(inner agent.ToolDispatcher) agent.ToolDispatcher {
	return func(ctx context.Context, tc llm.ToolCall) (string, error) {
		name := tc.Function.Name
		// Who stopped the turn, as the result names it. A budget cut is not
		// the user, and must not read as one.
		by := func() string {
			if r := interruptReason(ctx); r != "" {
				return "the turn was stopped (" + r + ")"
			}
			return "the user interrupted the turn"
		}
		if interrupted(ctx) {
			return fmt.Sprintf("ERROR: not run — %s before this %s call started.", by(), name), nil
		}
		out, err := inner(ctx, tc)
		if !interrupted(ctx) {
			return out, err
		}
		if err != nil {
			if r := interruptReason(ctx); r != "" {
				return interruptedCallFor(name, r), nil
			}
			return userInterruptedCall(name), nil
		}
		// It finished, but the interrupt may have cut it short: keep what it
		// returned and say so.
		return strings.TrimRight(out, "\n") + "\n\n[" + by() + " while this call ran; " +
			"the output above may be incomplete]", nil
	}
}
