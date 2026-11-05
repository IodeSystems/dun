package dun

import (
	"fmt"
	"log"
	"time"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/dun/internal/tokenmeter"
)

// CompactionNote is one history fold, for a UI and for the logs.
type CompactionNote struct {
	Subsumed      int    // entries folded into the summary
	TokensBefore  int    // estimated active-window tokens before
	TokensAfter   int    // and after
	Summary       string // the summary that replaced them
	Turn          int    // how many folds this turn — >1 is thrashing
	SinceLastSecs int    // seconds since the previous fold (0 = first)
	Cause         string // WHY this fold ran: "shaper" (budget) or "overflow" (endpoint cut a reply)
}

// String renders the note as one reviewable line.
func (n CompactionNote) String() string {
	s := fmt.Sprintf("compacted %d entries: %d → %d tokens (saved %d)",
		n.Subsumed, n.TokensBefore, n.TokensAfter, n.TokensBefore-n.TokensAfter)
	if n.Cause != "" {
		s += " · " + n.Cause
	}
	if n.Turn > 1 {
		s += fmt.Sprintf(" · %d× this turn", n.Turn)
	}
	if n.SinceLastSecs > 0 {
		s += fmt.Sprintf(" · %ds since the last", n.SinceLastSecs)
	}
	return s
}

// compactionThrashTurns is how many folds in ONE turn count as thrashing. More
// than one means the budget cannot fit what a turn structurally needs (system
// prompt + tool schemas + the pristine tail), so every turn will fold whatever
// it just summarized — spending an LLM call each time to destroy context. The
// budget is wrong, and saying so beats letting it grind.
//
// That diagnosis is only true of folds the SHAPER decided on. The overflow
// policy folds for an unrelated reason — the endpoint cut a reply, and a second
// cut escalates to shedding history — and it can do so with the budget nowhere
// near full. Observed live (2026-08-24): two overflow folds in one turn on a
// 9,824-token prompt against a 178,354-token budget, reported as "the context
// budget cannot fit one turn's floor. Raise DUN_CONTEXT_TOKENS." Every word of
// that was wrong, and it is the exact failure this session's work was about — a
// number that is confidently misattributed. So the counter is split by cause.
const compactionThrashTurns = 2

// noteUsage keeps a CHILD's token tally live. It fires after every chat round,
// not just when a turn finishes, which is what makes the parent's view of a
// child move while the child is still working.
//
// Why it matters more than it sounds: jobs(agent:N)'s report is the only thing a
// parent has to judge a child by, and its two moving parts are elapsed time and
// tokens. With both frozen, "still working" and "stopped making progress" print
// the same line — which is exactly how a wedged child went unnoticed for 15
// minutes while its parent polled it three times.
//
// A root harness has no record in a parent, so this is a child-only signal.
func (h *Harness) noteUsage(u agent.TokenUsage) {
	// Calibrate first: this is the only place the provider's own count of the
	// prompt meets our count of the chars that produced it, and every number
	// below (including Active) is denominated in the ratio it sets.
	tokenmeter.LogObservation(h.meter.NoteUsage(u))
	h.cache.noteRound(u, time.Now())
	h.noteSurfacingRound()
	// A large window earns a reminder that recap exists, naming what is filling
	// it. See maybeNudgeRecap: the system prompt alone did not move the model.
	h.maybeNudgeRecap(u.Active)
	if h.self == nil {
		return
	}
	h.self.setTokens(u.Total)
	h.self.noteActivity()
}

// noteCompaction records a fold, decorates it with the thrash counters, and
// hands it to the UI.
func (h *Harness) noteCompaction(ci agent.CompactionInfo) {
	h.noteCompactionCause(ci, foldByShaper)
}

// foldCause is why a fold happened, which decides what the thrash warning is
// allowed to conclude from a second one in the same turn.
type foldCause int

const (
	// foldByShaper: the Shaper could not fit the prompt in its budget. A second
	// of these in one turn really does mean the budget is too small.
	foldByShaper foldCause = iota
	// foldByOverflow: the endpoint cut a reply and the overflow policy escalated
	// to shedding history. Says nothing about the budget — see the note on
	// compactionThrashTurns for the live case where conflating them printed
	// advice that was wrong in every particular.
	foldByOverflow
)

func (h *Harness) noteCompactionCause(ci agent.CompactionInfo, cause foldCause) {
	h.cache.noteFold()
	h.compactMu.Lock()
	if cause == foldByOverflow {
		h.compactOverflow++
	}
	h.compactTurn++
	note := CompactionNote{
		Subsumed:     ci.SubsumedCount,
		TokensBefore: ci.TokensBefore,
		TokensAfter:  ci.TokensAfter,
		Summary:      ci.Summary,
		Turn:         h.compactTurn,
	}
	if cause == foldByOverflow {
		note.Cause = "overflow"
	} else {
		note.Cause = "shaper"
	}
	if !h.compactLast.IsZero() {
		note.SinceLastSecs = int(time.Since(h.compactLast).Seconds())
	}
	h.compactLast = time.Now()
	thrash := h.compactTurn == compactionThrashTurns
	// Only the SHAPER's folds indict the budget. If any fold this turn came from
	// the overflow policy, the honest reading is "the endpoint keeps cutting
	// replies", which no amount of DUN_CONTEXT_TOKENS fixes.
	fromOverflow := h.compactOverflow > 0
	h.compactMu.Unlock()

	log.Printf("dun: %s", note)
	// The bar's context figure drops now, not at the next request.
	h.req.adjustCtx(ci.TokensAfter - ci.TokensBefore)
	switch {
	case thrash && fromOverflow:
		log.Printf("dun: %d folds in one turn, driven by the endpoint cutting replies short — "+
			"NOT by the context budget. Look at why generation is being cut (DUN_MAX_OUTPUT_TOKENS, "+
			"or a prompt near the window) rather than at the budget.", h.compactTurn)
	case thrash:
		log.Printf("dun: compaction is THRASHING (%d folds in one turn) — the context budget "+
			"cannot fit one turn's floor. Raise DUN_CONTEXT_TOKENS or unset it (unset = no compaction).",
			h.compactTurn)
	}
	// What the summary may have dropped, from dun's own records.
	if n := h.foldRestoreNote(); n != "" {
		h.Aside(n)
	}
	if h.cfg.OnCompaction != nil {
		h.cfg.OnCompaction(note)
	}
}
