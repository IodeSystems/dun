package harness

import (
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
)

// Repairs: the workarounds dun and agentkit run against a model or a
// provider that misbehaves, each one a RoundEvent in the rounds ledger
// (roundstats.go), so /usage repairs and `dun usage --repairs` can say which
// model needs which repair, how often it works, and what it costs.
//
// A repair agentkit starts is a notice it appends to the transcript (the
// store's onRecord hook, recordEntry): one "repair" line per notice, at once.
// Whether it worked is known when the agentkit Turn ends (OnTurnEnd,
// noteTurnEnd): a repair worked when the Turn did not end in the error it
// repairs. That is one "repair_outcome" line per cause the Turn repaired,
// with the count in attempt. The resends (provider_fail_retry) and the length
// cuts (length_cut) are resolved the same way.

// repairNoticeTags are agentkit's repair notices, by their transcript tag.
// idle_check is a nudge, not a repair of a fault: counted, with no outcome.
var repairNoticeTags = map[string]bool{
	agent.EmptyReplyTag:    true,
	agent.ReasoningCutTag:  true,
	agent.MalformedCallTag: true,
	"repetition":           true,
	"tool_loop":            true,
	"idle_check":           true,
}

// repairFailsAs is the Turn end that says each repair cause did NOT work.
// A cause missing here has no outcome.
var repairFailsAs = map[string][]agent.TurnEndReason{
	agent.EmptyReplyTag:    {agent.TurnEmptyReply},
	agent.MalformedCallTag: {agent.TurnEmptyReply},
	agent.ReasoningCutTag:  {agent.TurnReasoningCut, agent.TurnEmptyReply},
	"repetition":           {agent.TurnRepetition},
	"tool_loop":            {agent.TurnToolLoop},
	"provider_fail":        {agent.TurnEmptyReply},
	"length_cut":           {agent.TurnOverflow},
}

// repairState counts this Turn's repairs by cause, for their outcomes.
type repairState struct {
	mu     sync.Mutex
	counts map[string]int
}

// repairCause is the cause an event counts under for an outcome, "" when
// it has none.
func repairCause(e RoundEvent) string {
	switch e.Kind {
	case "length_cut":
		return "length_cut"
	case "provider_fail_retry":
		return "provider_fail"
	case "repair":
		if _, ok := repairFailsAs[e.Cause]; ok {
			return e.Cause
		}
	}
	return ""
}

// repairRoundCause is what the NEXT round is a repair of, when e is a
// resend or a repair that runs another round; "" otherwise.
func repairRoundCause(e RoundEvent) string {
	switch e.Kind {
	case "repair":
		return e.Cause
	case "length_cut", "provider_fail_retry":
		return repairCause(e)
	case "retry":
		if e.Outcome == "" {
			return "retry:" + e.Cause
		}
	}
	return ""
}

func (r *repairState) note(e RoundEvent) {
	c := repairCause(e)
	if c == "" {
		return
	}
	r.mu.Lock()
	if r.counts == nil {
		r.counts = map[string]int{}
	}
	r.counts[c]++
	r.mu.Unlock()
}

// take is this Turn's counts, cleared.
func (r *repairState) take() map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.counts
	r.counts = nil
	return c
}

// repairOutcomes is the lines a Turn's end writes for counts: one per
// cause, repaired unless the Turn ended in what the cause fails as. A
// cancelled Turn says nothing about a repair, and writes none.
func repairOutcomes(counts map[string]int, reason agent.TurnEndReason) []RoundEvent {
	if reason == agent.TurnCanceled {
		return nil
	}
	var out []RoundEvent
	for c, n := range counts {
		o := "repaired"
		for _, f := range repairFailsAs[c] {
			if reason == f {
				o = "failed"
			}
		}
		out = append(out, RoundEvent{Kind: "repair_outcome", Cause: c, Outcome: o, Attempt: n})
	}
	return out
}

// noteTurnEnd is the Session's OnTurnEnd: the Turn's repair outcomes, and
// a MaxTurns cut.
func (h *Harness) noteTurnEnd(te agent.TurnEnd) {
	for _, e := range repairOutcomes(h.repairs.take(), te.Reason) {
		h.noteRoundEvent(e)
	}
	if te.Reason == agent.TurnMaxTurns {
		h.noteRoundEvent(RoundEvent{Kind: "turn_cap", Cause: "max_turns", Attempt: te.Rounds, Max: h.turnLimit()})
	}
}

// recordEntry is the store's onRecord hook: a repair notice, or a tool
// result that is an error, is a ledger line.
func (h *Harness) recordEntry(e agent.Entry) {
	switch e.Kind {
	case agent.KindNotification:
		if repairNoticeTags[e.Tag] {
			ev := RoundEvent{Kind: "repair", Cause: e.Tag, Trace: h.trace.Last()}
			if e.Tag == agent.MalformedCallTag {
				ev.NativeFinish = "MALFORMED_FUNCTION_CALL"
			}
			h.noteRoundEvent(ev)
		}
	case agent.KindToolResult:
		if c := toolErrorClass(e.ToolName, e.Content); c != "" {
			h.noteRoundEvent(RoundEvent{Kind: "tool_error", Tool: e.ToolName, Cause: c})
		}
	}
}

// toolErrorClass classes a tool result: "malformed_json" (agentkit refused
// the call's arguments), "truncated" (they were cut off), "unknown_tool",
// "error" (any other ERROR result), "warned" (it worked, with a caveat: an
// incomplete or unvalidated node_edit), or "" for a plain result. A call an
// interrupt cut short or skipped is the cancel's, not the tool's: "".
func toolErrorClass(tool, content string) string {
	r := strings.TrimSpace(content)
	if !strings.HasPrefix(r, "ERROR") {
		if tool == "node_edit" && nodeEditWarned(r) {
			return "warned"
		}
		return ""
	}
	switch {
	case strings.HasPrefix(r, "ERROR: not run —"), strings.Contains(r, "call was INTERRUPTED"):
		return ""
	case strings.HasPrefix(r, "ERROR: malformed json — truncation"):
		return "truncated"
	case strings.HasPrefix(r, "ERROR: malformed json"):
		return "malformed_json"
	case strings.HasPrefix(r, "ERROR: unknown tool"):
		return "unknown_tool"
	}
	return "error"
}

// nodeEditWarned reads a node_edit result as the TUI's call mark does
// (tui/nodeeditrender.go): it landed, but incomplete or not validated.
func nodeEditWarned(result string) bool {
	var v struct {
		Validated         *bool   `json:"validated"`
		ValidationSkipped bool    `json:"validationSkipped"`
		Incomplete        bool    `json:"incomplete"`
		Diff              *string `json:"diff"`
	}
	if json.Unmarshal([]byte(result), &v) != nil || v.Diff != nil {
		return false
	}
	return v.Incomplete || (v.Validated != nil && !*v.Validated && !v.ValidationSkipped)
}

// noteRetryEvent files a request- or turn-scope retry: each wait (kind,
// status, wait), and the end of one (recovered, gave_up). A provider resend
// is provider_fail_retry's, an overflow's note length_cut's.
func (h *Harness) noteRetryEvent(n RetryNote) {
	if n.Scope != "request" && n.Scope != "turn" {
		return
	}
	e := RoundEvent{Kind: "retry", Scope: n.Scope, Cause: n.Kind, Status: n.Status, Attempt: n.Attempt,
		WaitMS: n.Delay.Milliseconds(), Trace: h.trace.Last()}
	switch n.Kind {
	case "recovered":
		e.Cause, e.Outcome, e.WaitMS = "", "recovered", 0
	case "giveup":
		e.Cause, e.Outcome, e.WaitMS = "", "gave_up", 0
	}
	h.noteRoundEvent(e)
}

// noteCancel files a turn that was stopped before it finished: by the
// person ("user": the TUI, the web, ACP session/cancel all reach
// Interrupt("")), by its budget, or by something else (a restart, a
// clear), whose words are the reason.
func (h *Harness) noteCancel(reason string, budget bool) {
	e := RoundEvent{Kind: "cancel", Cause: "user"}
	switch {
	case budget:
		e.Cause, e.Reason = "budget", reason
	case strings.Contains(reason, "restart"):
		e.Cause, e.Reason = "restart", reason
	case reason != "":
		e.Cause, e.Reason = "other", reason
	}
	h.noteRoundEvent(e)
}

// NoteCmdTime files how long a command took, measured where surface says
// (engine: the handling; tui: keypress to the frame that shows it).
func (h *Harness) NoteCmdTime(cmd, surface string, d time.Duration) {
	if h == nil || cmd == "" {
		return
	}
	h.noteRoundEvent(RoundEvent{Kind: "cmd_time", Cmd: cmd, Surface: surface, MS: d.Milliseconds()})
}

// capsLedger is where a capability clamp is filed: the process's root
// session's ledger (TrackCaps has no session). Set by the root harness.
var capsLedger atomic.Pointer[StatsLedger]

// capsSeen is the clamps filed in this process: a clamp repeats on every
// request, and one line per process says it happens.
var capsSeen sync.Map

// noteCapClamp files a translation the client made to what dun asked for
// (llm.Client.OnTranslate), once per model, field and values per process.
func noteCapClamp(provider, model string, t llm.Translation) {
	l := capsLedger.Load()
	if l == nil {
		return
	}
	key := provider + "\x00" + model + "\x00" + t.Key + "\x00" + t.Sent + "\x00" + t.Wanted
	if _, dup := capsSeen.LoadOrStore(key, true); dup {
		return
	}
	_ = l.AppendEvent(RoundEvent{Kind: "cap_clamp", Provider: provider, Model: model,
		Key: t.Key, Sent: t.Sent, Wanted: t.Wanted, Reason: t.Reason})
}
