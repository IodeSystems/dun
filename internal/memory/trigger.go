package memory

import (
	"regexp"

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

// Trigger says what a turn's entries call for: the user messages that ask
// to remember something, whether the replies claim a save, and whether
// memory was called.
type Trigger struct {
	Asks    []string
	Claimed bool
	Called  bool
}

// ScanTrigger reads a turn's entries for a memory trigger.
func ScanTrigger(entries []agent.Entry) Trigger {
	var t Trigger
	for _, e := range entries {
		switch e.Kind {
		case agent.KindUser:
			if userRememberRx.MatchString(e.Content) {
				t.Asks = append(t.Asks, e.Content)
			}
		case agent.KindAssistant:
			if assistantClaimRx.MatchString(e.Content) {
				t.Claimed = true
			}
		case agent.KindToolCall:
			if e.ToolName == "memory" {
				t.Called = true
			}
		}
	}
	return t
}
