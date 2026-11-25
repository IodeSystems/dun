package harness

import (
	"encoding/json"
	"sort"
	"strings"
)

// redoAfterShrink measures the share of tool calls in the 20 rounds following
// a shrink event that repeat an earlier tool call with the same normalized
// arguments. "Earlier" means any call in the session before the window: a
// fold that removed a file_read from the visible history, for example, makes
// the model re-read that file within a few rounds, and this metric captures
// how often that happens.
//
// The metric is computed offline over the session log: a shrink event is a
// round whose prompt dropped (already recorded in the rounds ledger), and the
// 20-round window is the next 20 model rounds (assistant entries). Tool calls
// are normalized by (tool name, canonical JSON of the arguments) so that key
// order does not matter.
//
// The window is 20 rounds because a fold at 55% keeps roughly half the
// history; the model has about 20 rounds of fresh context before the next
// fold is likely, so repeats in that span are attributable to the shrink.

// RedoShrinkWindow is how many rounds after a shrink the metric counts.
const RedoShrinkWindow = 20

// ShrinkAt is one shrink event: the round where the prompt dropped and its
// cause (the build's rewrite reason: fold, rehead, recap, lod-step, tools,
// other).
type ShrinkAt struct {
	Round int    // the 1-based round number where the drop landed
	Cause string // fold, rehead, recap, lod-step, tools, other, "" if unknown
}

// ToolCallAt is one tool call in the session: which round made it and its
// identity for repeat detection.
type ToolCallAt struct {
	Round int    // the 1-based round the call belongs to
	Tool  string // the tool name
	// Key is the canonical JSON of the call's arguments (sorted keys), so
	// two calls with the same args in different key order are the same.
	Key string
}

// RedoShrinkResult is the metric: the overall share and a per-cause breakdown
// of calls in the 20-round window after each shrink that repeat an earlier
// call.
type RedoShrinkResult struct {
	// TotalShrinks is how many shrink events were examined.
	TotalShrinks int
	// TotalCalls is the total number of tool calls in all 20-round windows.
	TotalCalls int
	// Repeats is how many of those calls repeated an earlier call.
	Repeats int
	// Share is Repeats/TotalCalls (0 when TotalCalls == 0).
	Share float64
	// PerCause breaks the metric down by shrink cause.
	PerCause map[string]RedoShrinkCause
}

// RedoShrinkCause is the metric for one shrink cause.
type RedoShrinkCause struct {
	Shrinks int     // how many shrink events of this cause
	Calls   int     // tool calls in their 20-round windows
	Repeats int     // of those, calls that repeat an earlier call
	Share   float64 // Repeats/Calls (0 when Calls == 0)
}

// RedoAfterShrink computes the redo-after-shrink metric over a session's
// shrink events and tool calls. shrinks are the shrink events (round, cause);
// calls are all tool calls in the session, in order. The function is pure:
// it does not read files or call the model.
func RedoAfterShrink(shrinks []ShrinkAt, calls []ToolCallAt) RedoShrinkResult {
	res := RedoShrinkResult{
		TotalShrinks: len(shrinks),
		PerCause:     map[string]RedoShrinkCause{},
	}
	if len(shrinks) == 0 || len(calls) == 0 {
		return res
	}

	// Build a prefix set of (tool, key) pairs seen before each round, so
	// that for a window starting at round R we can test whether a call in
	// round R+i repeats something from rounds 1..R-1 (the pre-shrink
	// history) or from rounds R..R+i-1 (earlier in the window).
	// We track the set of all (tool, key) pairs seen so far as we walk
	// rounds in order; for a window [R, R+20) a call at round R+i is a
	// repeat if its (tool, key) was seen in any round < R+i.

	// Group calls by round for fast window access.
	byRound := map[int][]ToolCallAt{}
	for _, c := range calls {
		byRound[c.Round] = append(byRound[c.Round], c)
	}

	// Walk shrinks in round order. For each, count calls in [round,
	// round+RedoShrinkWindow) that repeat an earlier (tool, key).
	// "Earlier" = any call in the session with a round strictly less than
	// the current call's round. We maintain a global seen set as we walk
	// rounds, but for each window we need the set as of the window's start.
	// Simplest correct approach: for each call in the window, check if
	// (tool, key) appears in any round < call.Round.

	// To avoid O(n^2) over calls, precompute for each (tool, key) the list
	// of rounds it appears in, then a call at round R is a repeat if the
	// list has any entry < R.
	type callKey struct {
		tool string
		key  string
	}
	firstRound := map[callKey]int{} // earliest round for each (tool, key)
	for _, c := range calls {
		k := callKey{c.Tool, c.Key}
		if fr, ok := firstRound[k]; !ok || c.Round < fr {
			firstRound[k] = c.Round
		}
	}

	for _, s := range shrinks {
		cause := s.Cause
		if cause == "" {
			cause = "unknown"
		}
		c := res.PerCause[cause]
		c.Shrinks++

		for r := s.Round; r < s.Round+RedoShrinkWindow; r++ {
			for _, tc := range byRound[r] {
				c.Calls++
				res.TotalCalls++
				k := callKey{tc.Tool, tc.Key}
				if fr, ok := firstRound[k]; ok && fr < r {
					c.Repeats++
					res.Repeats++
				}
			}
		}

		if c.Calls > 0 {
			c.Share = float64(c.Repeats) / float64(c.Calls)
		}
		res.PerCause[cause] = c
	}

	if res.TotalCalls > 0 {
		res.Share = float64(res.Repeats) / float64(res.TotalCalls)
	}
	return res
}

// NormalizeArgs returns the canonical form of a tool call's arguments for
// repeat detection: the JSON with keys sorted at every level. Invalid JSON
// (or an empty string) returns the raw string, so two identical malformed
// args still match.
func NormalizeArgs(args string) string {
	if args == "" {
		return ""
	}
	var v any
	if err := json.Unmarshal([]byte(args), &v); err != nil {
		return args
	}
	return canonicalJSON(v)
}

// canonicalJSON renders v as JSON with object keys sorted at every level.
func canonicalJSON(v any) string {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(t))
		for _, k := range keys {
			parts = append(parts, jsonString(k)+":"+canonicalJSON(t[k]))
		}
		return "{" + strings.Join(parts, ",") + "}"
	case []any:
		parts := make([]string, 0, len(t))
		for _, e := range t {
			parts = append(parts, canonicalJSON(e))
		}
		return "[" + strings.Join(parts, ",") + "]"
	case string:
		return jsonString(t)
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}

// jsonString quotes s as a JSON string.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
