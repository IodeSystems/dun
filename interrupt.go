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
}

// Interrupt stops the turn in flight, if there is one, and reports whether
// there was. Safe from any goroutine (the engine calls it from its reader).
func (h *Harness) Interrupt() bool {
	h.intMu.Lock()
	defer h.intMu.Unlock()
	if h.intCancel == nil {
		return false
	}
	h.interrupting.Store(true)
	h.intCancel(ErrInterrupted)
	return true
}

// interruptible is the turn's context, which Interrupt cancels, and the func
// that ends it. A new turn clears the last turn's interrupt.
func (h *Harness) interruptible(ctx context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(ctx)
	h.intMu.Lock()
	h.intCancel = cancel
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

// interrupted says ctx is a turn the user interrupted.
func interrupted(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), ErrInterrupted)
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
	h.healOrphans(ctx, userInterruptedCall, true)
	note := "[the user interrupted this turn] Your previous response was stopped by the user before it finished"
	if !done && strings.TrimSpace(content) != "" {
		note += "; the reply text above is partial, cut where it stands"
	}
	note += "."
	if !done {
		note += reasoningNote(reasoning, took)
	}
	note += " Do not resume or repeat it on your own: the user's next message says what to do."
	h.store.appendSilent(agent.Entry{
		ID: uuid.New().String(), Kind: agent.KindNotification, Tag: interruptTag,
		Content: note, CreatedAt: time.Now().UnixNano(),
	})
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

// withInterrupt is the outermost dispatcher: a call the turn reaches after the
// interrupt is not run, and one the interrupt cut short says so.
func withInterrupt(inner agent.ToolDispatcher) agent.ToolDispatcher {
	return func(ctx context.Context, tc llm.ToolCall) (string, error) {
		name := tc.Function.Name
		if interrupted(ctx) {
			return fmt.Sprintf("ERROR: not run — the user interrupted the turn before this %s call started.", name), nil
		}
		out, err := inner(ctx, tc)
		if !interrupted(ctx) {
			return out, err
		}
		if err != nil {
			return userInterruptedCall(name), nil
		}
		// It finished, but the interrupt may have cut it short: keep what it
		// returned and say so.
		return strings.TrimRight(out, "\n") + "\n\n[the user interrupted the turn while this call ran; " +
			"the output above may be incomplete]", nil
	}
}
