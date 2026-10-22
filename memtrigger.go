package dun

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/iodesystems/agentkit/agent"
)

// Memory triggers — the memory fork run without being asked for, when the
// words of a turn say something should be kept and the turn did not keep it.
//
// Measured before building (plan item 9 M3, 2026-10-21) over 274 dun
// session logs and the user's Claude Code transcripts (3,047 user messages,
// 13,854 assistant texts):
//
//   - The user: "remember" opening a clause ("Remember, …", "And remember,
//     …", "… expectations. Remember …") was a standing rule or project fact
//     in 10 of 10 deduplicated cases — "Remember we don't want this prod load
//     test config to have charging…". "remember" elsewhere ("I remember",
//     "don't remember sending", "we want to remember it") was 0 of 4. So the
//     pattern is clause-initial only. About 0.3% of messages.
//   - The assistant: "I'll remember / keep that in mind" — 0 in 18k texts.
//     "I've saved that as a standing rule" is common, but in those logs it
//     followed a real save. It is caught here only when the turn made no
//     memory call, which is the case the trigger exists for: a model that
//     says it kept something and did not. "I've noted it in the plan" is not
//     memory and does not match.
//
// A turn that called memory itself fires nothing: the model already did it.

var (
	// userRememberRx: a clause that starts with an instruction to keep
	// something. Not "I remember", "don't remember", "to remember".
	userRememberRx = regexp.MustCompile(`(?i)(?:^|[.!?;:]\s+|\band\s+|,\s*)(?:remember|keep (?:that |this |it )?in mind|don'?t forget|do not forget|from now on|going forward)\b`)
	// assistantClaimRx: the reply says it will keep, or has kept, something
	// for later sessions.
	assistantClaimRx = regexp.MustCompile(`(?i)\bI(?:'ll| will) (?:remember|keep (?:that|this|it) in mind)\b|\bI(?:'ve| have) (?:saved|noted|recorded|stored)\b[^.\n]{0,60}\b(?:standing rule|as a rule|to memory|a memory|in memory|for (?:future|later|next) (?:sessions?|time))`)
)

// memoryTrigger says what a turn's entries call for: the user messages that
// ask to remember something, whether the replies claim a save, and whether
// memory was called.
type memoryTrigger struct {
	asks    []string
	claimed bool
	called  bool
}

func scanMemoryTrigger(entries []agent.Entry) memoryTrigger {
	var t memoryTrigger
	for _, e := range entries {
		switch e.Kind {
		case agent.KindUser:
			if userRememberRx.MatchString(e.Content) {
				t.asks = append(t.asks, e.Content)
			}
		case agent.KindAssistant:
			if assistantClaimRx.MatchString(e.Content) {
				t.claimed = true
			}
		case agent.KindToolCall:
			if e.ToolName == "memory" {
				t.called = true
			}
		}
	}
	return t
}

// triggerMax bounds how much of a user message goes into the fork's
// instruction: the ask is usually a sentence; a pasted document that
// happens to say "remember" is not.
const triggerMax = 1200

// triggerTimeout bounds a triggered fork, which no caller waits on.
const triggerTimeout = 2 * time.Minute

// checkMemoryTriggers looks at the entries since the last check and runs
// the memory fork when they call for it. It returns the fork's line for the
// user, or "" when nothing fired. Called after a turn, outside turnMu.
func (h *Harness) checkMemoryTriggers(ctx context.Context) string {
	// A child's "user" is its parent: the parent's words are not the
	// person's, and the parent runs its own triggers.
	if h.cfg.DisableMemoryTriggers || h.isChild() || h.memoryStore() == nil || h.store == nil {
		h.markTriggerScan()
		return ""
	}
	t := scanMemoryTrigger(h.entriesSinceTriggerScan())
	if t.called || (len(t.asks) == 0 && !t.claimed) {
		return ""
	}
	what := strings.Join(t.asks, "\n\n")
	if len(what) > triggerMax {
		what = what[:triggerMax] + "…"
	}
	why := "you said remember"
	if len(t.asks) == 0 {
		why = "the reply said it saved something, with no memory call"
	}
	msg, err := h.Remember(ctx, what)
	if err != nil {
		return "memory (" + why + "): nothing saved — " + err.Error()
	}
	return "memory (" + why + "): " + msg
}

// entriesSinceTriggerScan returns the entries newer than the last scan and
// moves the mark past them, so each message is looked at once.
func (h *Harness) entriesSinceTriggerScan() []agent.Entry {
	all, _ := h.store.Context(context.Background(), "")
	h.trigMu.Lock()
	defer h.trigMu.Unlock()
	var out []agent.Entry
	mark := h.trigMark
	for _, e := range all {
		if e.CreatedAt > h.trigMark {
			out = append(out, e)
		}
		if e.CreatedAt > mark {
			mark = e.CreatedAt
		}
	}
	h.trigMark = mark
	return out
}

// markTriggerScan moves the mark to now's newest entry without scanning: a
// resumed session's history was someone else's to act on.
func (h *Harness) markTriggerScan() {
	if h.store != nil {
		h.entriesSinceTriggerScan()
	}
}
