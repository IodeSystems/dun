package dun

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/iodesystems/agentkit/agent"
)

// Resumed reports how many entries were restored from an existing session file.
func (h *Harness) Resumed() int { return h.store.Loaded() }

// HistoryItem is one replayable conversation entry for a resuming client — the
// neutral shape the -p `history` event carries so a TUI can rebuild scrollback
// without re-running any turn. A tool_call and its result share a CallID so the
// client folds them into one block.
type HistoryItem struct {
	Kind    string         `json:"kind"` // user|assistant|tool_call|tool_result|notification|reasoning
	Content string         `json:"content,omitempty"`
	Tool    string         `json:"tool,omitempty"`    // tool_call / tool_result
	CallID  string         `json:"call_id,omitempty"` // pairs a tool_call with its result
	Args    map[string]any `json:"args,omitempty"`    // decoded tool_call arguments
}

// History returns the loaded conversation as replayable items in chronological
// order, for a resuming client to render as scrollback. Empty for a fresh
// session. Compaction markers are dropped (a resumed client shows the surviving
// entries, not the fold marker) and empty assistant turns are skipped;
// tool-call arguments are decoded from the stored JSON so the client renders
// them like a live call.
func (h *Harness) History() []HistoryItem {
	return historyItems(h.store.History())
}

// SessionHistory is History read straight from a session file, with no
// harness: what a service sends a client that has fallen too far behind to
// replay. A missing file is an empty conversation.
func SessionHistory(sessionFile string) []HistoryItem {
	st, err := openSessionStore(sessionFile)
	if err != nil {
		return nil
	}
	return historyItems(st.History())
}
func historyItems(entries []agent.Entry) []HistoryItem {
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].CreatedAt != entries[j].CreatedAt {
			return entries[i].CreatedAt < entries[j].CreatedAt
		}
		return entries[i].ID < entries[j].ID
	})
	out := make([]HistoryItem, 0, len(entries))
	for _, e := range entries {
		// The round's reasoning rides the entry it produced (agentkit stamps it
		// on the reply, or on the first call of a round with no prose). It goes
		// out as its own item just BEFORE that entry — where it happened — so a
		// client that does not know the kind skips it and loses nothing else.
		if r := strings.TrimSpace(e.Reasoning); r != "" {
			out = append(out, HistoryItem{Kind: "reasoning", Content: r})
		}
		switch e.Kind {
		case agent.KindUser:
			out = append(out, HistoryItem{Kind: "user", Content: e.Content})
		case agent.KindAssistant:
			if strings.TrimSpace(e.Content) == "" {
				continue
			}
			out = append(out, HistoryItem{Kind: "assistant", Content: e.Content})
		case agent.KindToolCall:
			var args map[string]any
			_ = json.Unmarshal([]byte(e.Content), &args)
			out = append(out, HistoryItem{Kind: "tool_call", Tool: e.ToolName, CallID: e.ToolCallID, Args: args})
		case agent.KindToolResult:
			out = append(out, HistoryItem{Kind: "tool_result", Tool: e.ToolName, CallID: e.ToolCallID, Content: e.Content})
		case agent.KindNotification:
			out = append(out, HistoryItem{Kind: "notification", Content: e.Content})
		case kindCommand:
			out = append(out, HistoryItem{Kind: "command", Content: e.Content})
		}
	}
	return out
}

// Continue runs a turn with NO new user message — to process pending
// notifications (e.g. a background job's completion). This is the converge
// point: the notification (+ any queued messages) coalesce into one turn.
func (h *Harness) Continue(ctx context.Context) (agent.TurnResult, error) {
	ctx, end := h.interruptible(ctx)
	defer end()
	h.clearBudgetNotes()
	// Publish anything a tool result did not already carry (and pair off any
	// interrupted tool call), so the turn below has it — and so several jobs
	// finishing at once become ONE turn.
	h.prepareTurn(ctx)
	if h.store.pending() == 0 {
		// Nothing new to react to. Running the model anyway appends an assistant
		// message directly after the previous one, which providers reject on the
		// following request. A duplicate wake is a no-op, not a turn.
		return agent.TurnResult{}, nil
	}
	res, err := h.turnThenTriggers(ctx)
	return h.finishInterrupt(ctx, res, err)
}

// prepareTurn readies the store for a turn (or a retry of one).
//
// Two jobs, in this order because the first constrains the second:
//
//  1. Pair off any tool call that never got a result. A turn killed between
//     persisting a call and recording its result leaves the history structurally
//     invalid — providers deserialize historical tool_calls, and an
//     assistant(tool_calls) with no matching tool message is rejected before the
//     model is reached — so every later request fails identically. Healing it is
//     what makes "just send another message" actually resume the session.
//  2. Deliver everything buffered. When step 1 wrote a result, the buffer rides
//     INSIDE it, which is the whole point: the user's message and the recovery
//     from the dropped connection travel together, in one batch, costing no extra
//     turn. Otherwise the buffer is published as ordinary inbox arrivals.
//
// Called before every attempt in runTurn, so a message typed during a backoff
// wait joins the retry rather than queuing behind it.
func (h *Harness) prepareTurn(ctx context.Context) {
	// A hint from a cut in a PREVIOUS turn is stale by definition: it is written
	// in the present tense about the last response, and the last response is now
	// the one that recovered. See clearOverflowHints.
	h.clearOverflowHints()
	h.turnRounds.reset() // the turn limit counts this turn's rounds (turnlimit.go)
	if h.healOrphanToolCalls(ctx) == 0 {
		h.flushQueued()
	}
	// Asides ride whatever turn is about to run — including the one a user
	// message starts, which is where "the tool set changed" most often lands.
	// Healing already lifted them into the interrupted call's result.
	h.planTurnLine()     // before flushSilent: it rides this turn (plan.go)
	h.writeHarnessInfo() // $DUN_HARNESS: the branch may have moved since
	h.flushSilent()
}

// healOrphanToolCalls answers every persisted tool call that has no result with
// one that says the call was interrupted, and returns how many it wrote.
//
// The result is deliberately explicit about what is NOT known: an interrupted
// exec or edit may well have taken effect, so the model is told to CHECK rather
// than to assume either way. Anything buffered is lifted into the last result, so
// the model reads the interruption and the user's follow-up in the same breath.
func (h *Harness) healOrphanToolCalls(ctx context.Context) int {
	return h.healOrphans(ctx, droppedCall, false)
}

// droppedCall is a result for a call the connection cut.
func droppedCall(name string) string {
	return fmt.Sprintf(
		"ERROR: this %s call was INTERRUPTED — the connection to the model dropped before "+
			"the result was recorded. Whether it took effect is unknown: verify the current "+
			"state (re-read the file, re-run the check) before repeating it.", name)
}

// healOrphans answers every unanswered call with result(name).
//
// Normally (silent false) the results are inbox arrivals and the buffer rides
// the last one. For an interrupt (silent true) they are neither: the cut turn
// is over, nothing in it is a reason to run another, and the buffer is what
// starts the next turn.
func (h *Harness) healOrphans(ctx context.Context, result func(name string) string, silent bool) int {
	entries, err := h.store.Context(ctx, "dun")
	if err != nil {
		return 0
	}
	answered := map[string]bool{}
	for _, e := range entries {
		if e.Kind == agent.KindToolResult && e.ToolCallID != "" {
			answered[e.ToolCallID] = true
		}
	}
	var orphans []agent.Entry
	for _, e := range entries {
		if e.Kind == agent.KindToolCall && e.ToolCallID != "" && !answered[e.ToolCallID] {
			orphans = append(orphans, e)
		}
	}
	for i, o := range orphans {
		name := o.ToolName
		if name == "" {
			name = "the tool"
		}
		content := result(name)
		e := agent.Entry{
			ID: uuid.New().String(), Kind: agent.KindToolResult,
			ToolCallID: o.ToolCallID, ToolName: o.ToolName,
			CreatedAt: time.Now().UnixNano(),
		}
		if silent {
			e.Content = content
			h.store.appendSilent(e)
			continue
		}
		if i == len(orphans)-1 {
			content = h.liftQueued(content)
		}
		e.Content = content
		h.store.publish(e)
	}
	return len(orphans)
}
