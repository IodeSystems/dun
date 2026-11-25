package harness

import (
	"fmt"
	"log"
	"time"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/dun/internal/numfmt"
	"github.com/iodesystems/dun/internal/tokenmeter"
)

// CompactionNote is one history fold, for a UI and for the logs.
type CompactionNote struct {
	Subsumed      int    // entries folded into the summary
	TokensBefore  int    // estimated active-window tokens before
	TokensAfter   int    // and after: what the next build sends (without LOD: a fold lands under target)
	SummaryTokens int    // the summary marker's tokens, in TokensAfter
	PinnedTokens  int    // pinned entries older than the kept part, in TokensAfter
	Kept          int    // the newest entries kept verbatim
	KeptTokens    int    // and their tokens: TokensAfter − SummaryTokens − PinnedTokens
	Summary       string // the summary that replaced them
	Turn          int    // how many folds this turn — >1 is thrashing
	SinceLastSecs int    // seconds since the previous fold (0 = first)
	Cause         string // WHY this fold ran: "shaper" (budget) or "overflow" (endpoint cut a reply)
}

// String renders the note as one plain line: what was summarized, what was
// kept, the history size before and after, why, and when the last fold was.
// The summary, pinned and kept sizes add up to the after figure (agentkit
// v0.39.0 measures all three on the store after the fold).
func (n CompactionNote) String() string {
	s := fmt.Sprintf("compacted: oldest %d entries → summary", n.Subsumed)
	if n.SummaryTokens > 0 {
		s += " (" + numfmt.TokN(n.SummaryTokens) + ")"
	}
	if n.PinnedTokens > 0 {
		s += ", pinned (" + numfmt.TokN(n.PinnedTokens) + ")"
	}
	if n.Kept > 0 {
		s += fmt.Sprintf(", newest %d kept (%s)", n.Kept, numfmt.TokN(n.KeptTokens))
	}
	// Both ends leave out the system prompt and the tools; the arrow ends at
	// what the next build sends, without LOD (agentkit v0.39.0).
	s += fmt.Sprintf(" · history %s → %s tokens", numfmt.TokN(n.TokensBefore), numfmt.TokN(n.TokensAfter))
	if c := causeWords(n.Cause); c != "" {
		s += " · " + c
	}
	if n.Turn > 1 {
		s += " · " + ordinal(n.Turn) + " fold this turn"
	}
	if n.SinceLastSecs > 0 {
		s += " · " + foldAge(n.SinceLastSecs) + " since the last"
	}
	return s
}

// causeWords is a fold's Cause key in words. An unknown key prints as is.
func causeWords(c string) string {
	switch c {
	case "shaper":
		return "over budget"
	case "overflow":
		return "the endpoint cut a reply"
	case "boundary":
		return "turn end or cold cache"
	}
	return c
}

// ordinal is 2 → "2nd", 3 → "3rd", 11 → "11th".
func ordinal(n int) string {
	suf := "th"
	if n%100 < 11 || n%100 > 13 {
		switch n % 10 {
		case 1:
			suf = "st"
		case 2:
			suf = "nd"
		case 3:
			suf = "rd"
		}
	}
	return fmt.Sprintf("%d%s", n, suf)
}

// foldAge is a gap as 45s, 26m or 1h3m.
func foldAge(secs int) string {
	d := time.Duration(secs) * time.Second
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", secs)
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	h, m := int(d.Hours()), int(d.Minutes())%60
	if m == 0 {
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh%dm", h, m)
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
// Why it matters more than it sounds: agent({id:N})'s report is the only thing a
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
	obs, ok := h.meter.NoteUsage(u)
	tokenmeter.LogObservation(obs.WithTrace(h.trace.Last()), ok)
	h.cache.noteRound(u, time.Now())
	h.noteSurfacingRound()
	// A large window earns a reminder that recap exists, naming what is filling
	// it. See maybeNudgeRecap: the system prompt alone did not move the model.
	h.maybeNudgeRecap(u.Active)
	h.noteTurnRound()
	h.planNudgeRound() // after the count it reads (plannudge.go)
	h.planStallRound() // the same count
	if h.cfg.OnRound != nil {
		h.cfg.OnRound(u)
	}
	if h.self == nil {
		return
	}
	h.self.setTokens(u.Total)
	h.self.noteActivity()
}

// CompactingNote is a fold in progress, for a UI: begun, or (Done) ended
// without a CompactionNote. Err is why it ended so: "" when there was
// nothing to fold.
type CompactingNote struct {
	Done bool
	Err  string
}

// foldPause is the Shaper's Pause: called once a fold is decided, before its
// summary call, for every Shaper fold (a build's and Compact's). It says the
// fold has begun, then stops the host's clock (CompactionPause).
func (h *Harness) foldPause() (resume func()) {
	h.foldBegin()
	if CompactionPause != nil {
		return CompactionPause()
	}
	return func() {}
}

// foldBegin says a fold has begun. Once per fold: a second call before the
// fold ends sends nothing.
func (h *Harness) foldBegin() {
	if h.foldOpen.CompareAndSwap(false, true) && h.cfg.OnCompacting != nil {
		h.cfg.OnCompacting(CompactingNote{})
	}
}

// foldEnd ends a begun fold that sent no CompactionNote: err is why (nil:
// nothing was folded). A no-op when no fold is open, so a caller calls it
// on every path out of a fold. noteCompactionCause is the other end.
func (h *Harness) foldEnd(err error) {
	if err != nil && h.foldOpen.Load() {
		h.noteRoundEvent(RoundEvent{Kind: "fold_failed", Reason: oneLine(err.Error(), 200)})
	}
	if !h.foldOpen.CompareAndSwap(true, false) || h.cfg.OnCompacting == nil {
		return
	}
	n := CompactingNote{Done: true}
	if err != nil {
		n.Err = err.Error()
	}
	h.cfg.OnCompacting(n)
}

// noteCompaction records a fold, decorates it with the thrash counters, and
// hands it to the UI.
func (h *Harness) noteCompaction(ci agent.CompactionInfo) {
	h.noteCompactionCause(ci, foldByShaper)
}

// foldCauseName is the cause as a ledger line names it.
func foldCauseName(c foldCause) string {
	switch c {
	case foldByOverflow:
		return "overflow"
	case foldByBoundary:
		return "boundary"
	}
	return "shaper"
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
	// foldByBoundary: a boundary trigger — turn end with the prompt over
	// 0.7×ceiling, or a cold-cache gap over the provider's TTL. The prompt
	// is not at the budget; the fold keeps the cached prefix intact and
	// preempts the next round's re-read.
	foldByBoundary
)

func (h *Harness) noteCompactionCause(ci agent.CompactionInfo, cause foldCause) {
	// The fold may have taken a once-said hint's text (forget.go). The other
	// call site is applyRecapText (recap.go).
	h.forgetHints()
	h.noteRoundEvent(RoundEvent{Kind: "fold", Cause: foldCauseName(cause), Before: ci.TokensBefore, After: ci.TokensAfter})
	if cause == foldByBoundary {
		h.cache.noteBoundaryFold()
	} else {
		h.cache.noteFold()
	}
	h.compactMu.Lock()
	switch cause {
	case foldByOverflow:
		h.compactOverflow++
	case foldByBoundary:
		h.compactBoundary++
	}
	h.compactTurn++
	note := CompactionNote{
		Subsumed:      ci.SubsumedCount,
		TokensBefore:  ci.TokensBefore,
		TokensAfter:   ci.TokensAfter,
		SummaryTokens: ci.SummaryTokens,
		PinnedTokens:  ci.PinnedTokens,
		Kept:          ci.KeptCount,
		KeptTokens:    ci.KeptTokens,
		Summary:       ci.Summary,
		Turn:          h.compactTurn,
	}
	switch cause {
	case foldByOverflow:
		note.Cause = "overflow"
	case foldByBoundary:
		note.Cause = "boundary"
	default:
		note.Cause = "shaper"
	}
	if !h.compactLast.IsZero() {
		note.SinceLastSecs = int(time.Since(h.compactLast).Seconds())
	}
	h.compactLast = time.Now()
	thrash := h.compactTurn == compactionThrashTurns
	// Only the SHAPER's folds indict the budget. If any fold this turn came from
	// the overflow policy, the honest reading is "the endpoint keeps cutting
	// replies", which no amount of DUN_CONTEXT_TOKENS fixes. Boundary folds
	// are the same: the prompt was not at the budget, so a second one does not
	// mean the budget is too small.
	fromOverflow := h.compactOverflow > 0
	fromBoundary := h.compactBoundary > 0
	h.compactMu.Unlock()

	log.Printf("dun: %s", note)
	// The bar's context figure drops now, not at the next request.
	h.req.adjustCtx(ci.TokensAfter - ci.TokensBefore)
	switch {
	case thrash && fromOverflow:
		log.Printf("dun: %d folds in one turn, driven by the endpoint cutting replies short — "+
			"NOT by the context budget. Look at why generation is being cut (DUN_MAX_OUTPUT_TOKENS, "+
			"or a prompt near the window) rather than at the budget.", h.compactTurn)
	case thrash && fromBoundary:
		log.Printf("dun: %d folds in one turn, driven by a boundary trigger (turn-end or "+
			"cold cache) — NOT by the context budget.", h.compactTurn)
	case thrash:
		log.Printf("dun: compaction is THRASHING (%d folds in one turn) — the context budget "+
			"cannot fit one turn's floor. Raise DUN_CONTEXT_TOKENS or unset it (unset = no compaction).",
			h.compactTurn)
	}
	// What the summary may have dropped, from dun's own records.
	// Keyed: a second fold in the turn replaces the first one's note.
	if n := h.foldRestoreNote(); n != "" {
		h.AsideWith(n, AsideOpts{Key: "fold-restore"})
	}
	// The note ends the fold foldBegin opened.
	h.foldOpen.Store(false)
	if h.cfg.OnCompaction != nil {
		h.cfg.OnCompaction(note)
	}
}
