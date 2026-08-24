package dun

import (
	"context"
	"fmt"
	"log"

	"github.com/iodesystems/agentkit/agent"
)

// overflow.go — what dun does when the endpoint cuts a response for room.
//
// This is the OTHER end of the ladder rescue.go documents. That layer handles a
// prompt the endpoint REFUSES: the request never runs, the error is explicit,
// and folding history is the only possible answer. This layer handles a request
// that ran and was cut off partway through the reply — finish_reason=length —
// where folding history may be exactly the wrong thing to do.
//
// The distinction is the whole point, and getting it wrong is expensive in both
// directions. Two failures wear the same finish_reason:
//
//	the PROMPT ate the window   — 170k of a 188k window, ~18k to answer in, and
//	                              the reply needs more. Compacting fixes it and
//	                              nothing else does.
//	the RESPONSE ran away       — 109k of a 188k window, ~79k of room, and the
//	                              model spent all of it thinking. Compacting is
//	                              pure loss: it destroys history to make room the
//	                              model will spend the same way again.
//
// The measured case (yscr, 2026-08-24) was the second, and it is why the note
// exists at all. The model was stuck re-attempting one test edit; each attempt
// thought longer; nothing ever told it that the wall was there, so every retry
// was a fresh run at the same wall. An error result saying "your call was
// truncated" does not carry that — it reads as a transport hiccup, and the model
// answers it by trying again identically.
//
// So: measure first, act second, and SAY the numbers. The note names the window,
// the prompt, and what was left, because a model that is told "be brief" without
// them has no way to judge how brief.

// compactWhenFreeBelow is the room-left threshold that makes this a PROMPT
// problem rather than a response problem.
//
// One response's full reservation: below it, the prompt has grown into the space
// the response was promised, and no amount of asking the model to be concise
// gets that space back. Above it, the room was there and the response spent it —
// which folding history does not fix.
func compactWhenFreeBelow() int { return outputReserve() }

// OverflowNote is one length-cut round, for the UI.
//
// Reported separately from RetryNote because a cut is not a wait: nothing is
// being retried against an unavailable provider, the provider answered promptly
// and the answer did not fit. A UI that files it under "provider trouble" tells
// the user to be patient about a problem patience does not solve.
type OverflowNote struct {
	Attempt      int    `json:"attempt"`
	PromptTokens int    `json:"prompt_tokens"`
	Generated    int    `json:"generated"`
	Window       int    `json:"window"`
	Free         int    `json:"free"`         // room the response had
	InToolCall   bool   `json:"in_tool_call"` // the cut landed mid call, not in prose
	Folded       int    `json:"folded"`       // entries compacted away, 0 if none
	Hint         string `json:"hint"`         // what the model was told
}

// String is the one-line form for a log and for the TUI's retry banner. Phrased
// as what dun DID, not as what went wrong: the user's question on seeing this is
// always "so what is happening now".
func (n OverflowNote) String() string {
	what := "sent a hint"
	if n.Folded > 0 {
		what = fmt.Sprintf("folded %d entries to make room", n.Folded)
	}
	where := "the reply"
	if n.InToolCall {
		where = "a tool call"
	}
	return fmt.Sprintf("agent exceeded the context window — %s was cut after %d tokens "+
		"with %d of %d left; %s", where, n.Generated, n.Free, n.Window, what)
}

// onOverflow is the agent.Session hook. It decides between shedding context and
// telling the model what it ran into, and returns the note either way.
//
// Runs on the turn's goroutine, between the assistant reply being persisted and
// the tool calls being dispatched, so anything it does to the history is visible
// to the next build.
func (h *Harness) onOverflow(ctx context.Context, o agent.Overflow) agent.OverflowDecision {
	note := OverflowNote{
		Attempt:      o.Attempt,
		PromptTokens: o.PromptTokens,
		Generated:    o.Generated,
		Window:       h.window,
		InToolCall:   o.InToolCall(),
	}
	// Prefer the provider's count of the prompt; fall back to the measured
	// estimate when it reported none. Never to chars/4 — that is the estimate
	// whose 31% error let the window fill unnoticed in the first place.
	prompt := o.PromptTokens
	if prompt <= 0 {
		prompt = int(float64(h.meter.lastBuiltChars()) / h.meter.CharsPerToken())
		note.PromptTokens = prompt
	}
	if h.window > 0 {
		note.Free = h.window - prompt
	}

	// Fold only when the prompt is what filled the window, or when a previous
	// pass already told the model and it happened again anyway. The second
	// clause is the escalation: a hint that did not work is not worth repeating.
	crowded := h.window > 0 && note.Free < compactWhenFreeBelow()
	if crowded || o.Attempt > 1 {
		if n, err := h.foldHistory(ctx, foldByOverflow); err != nil {
			log.Printf("dun: overflow: could not fold history: %v", err)
		} else {
			note.Folded = n
		}
	}

	h.noteMu.Lock()
	h.overflowCuts++
	if note.Folded > 0 {
		h.overflowFolds++
	}
	h.noteMu.Unlock()

	note.Hint = overflowHint(note)
	log.Printf("dun: overflow: %s (%s)", note, o)
	h.reportOverflow(note)

	// Retry is only consulted for a cut in PROSE; a cut tool call already has an
	// error result on the way. Always true here because something always
	// changed: at minimum the hint is now in the history, and on the escalation
	// path the history is smaller too. agent.Session bounds how many times this
	// can say yes (MaxOverflowRetries), which is what keeps "always retry" from
	// meaning "retry forever".
	return agent.OverflowDecision{Note: note.Hint, Retry: true}
}

// overflowHint is what the MODEL reads. Three things, in this order, because
// that is the order the model needs them in: what happened to its last response,
// the arithmetic that explains it, and what to do differently.
//
// It says "the space was not there" rather than "you wrote too much" on purpose.
// The response was usually fine; the budget was not. A model told it did
// something wrong spends the next turn apologizing and re-planning, which costs
// another window.
func overflowHint(n OverflowNote) string {
	var b []byte
	what := "Your last response"
	if n.InToolCall {
		what = "Your last tool call"
	}
	b = fmt.Appendf(b, "%s was CUT OFF mid-way: the endpoint stopped generating because the "+
		"request ran out of context, not because anything was wrong with what you were writing. ", what)
	if n.Window > 0 {
		b = fmt.Appendf(b, "The prompt was %d tokens of a %d-token window, leaving about %d to answer in, "+
			"and the answer reached %d. ", n.PromptTokens, n.Window, n.Free, n.Generated)
	} else if n.Generated > 0 {
		b = fmt.Appendf(b, "It reached %d tokens before the cut. ", n.Generated)
	}
	if n.Folded > 0 {
		b = fmt.Appendf(b, "%d older entries have been folded into a summary to make room, so there "+
			"is more space now than there was. ", n.Folded)
	}
	b = append(b, "Work in smaller steps: think briefly, make ONE small tool call at a time, and "+
		"write a large file as successive edits rather than in a single call. "+
		"Do not re-send the response that was cut — redo it smaller."...)
	return string(b)
}

// reportOverflow hands the note to the UI.
//
// Rides RetryNote rather than inventing a second channel: every consumer of
// dun's events already renders one, it is already the "here is what the harness
// is doing about a problem you did not cause" surface, and a cut IS an
// interrupted turn. Kind stays "interrupted" so RetryNote.Retrying() keeps
// meaning what it means; the Reason carries the specifics.
func (h *Harness) reportOverflow(n OverflowNote) {
	h.retry(RetryNote{
		Scope:   "turn",
		Kind:    "interrupted",
		Attempt: n.Attempt,
		Reason:  n.String(),
	})
}

// clearOverflowHints drops any hint left in the history.
//
// Called when a turn ends without a cut, because that is the moment the hint
// stops being true. It is written in the present tense about the LAST response —
// leave it in place and it goes on telling the model that its most recent
// response was truncated and that it should be working in smaller steps, for the
// rest of the session. Measured cost aside, an instruction that outlives its
// reason is how an agent acquires a permanent, unexplained timidity.
func (h *Harness) clearOverflowHints() {
	if n := h.store.dropTagged(agent.OverflowTag); n > 0 {
		log.Printf("dun: dropped %d stale overflow hint(s) — the last turn was not cut", n)
	}
}
