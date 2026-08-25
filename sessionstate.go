package dun

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
)

// session_state — durable, model-managed state for the SESSION, not the
// conversation.
//
// The conversation is already durable: it is the JSONL on disk, and recap /
// compaction / rescue keep it alive through context pressure. But the
// conversation is a LOG, and a log does not say what the agent is DOING — the
// goal it is after, the plan it is following, the steps still open, where it
// stands. On a long task, or across a compaction, or across a process restart,
// the log loses all four. The model re-derives them from what is left, and the
// re-derivation is where a long task quietly goes off the rails: it resumes
// from the last twenty messages and forgets why it started.
//
// session_state is the thing the log does not carry. It is small, structured,
// and written by the model itself: a goal, a plan, a todo list, and a status.
// It is persisted to a .state.json sidecar next to the session's .jsonl, keyed
// by session id, so it survives the process the same way the conversation does.
//
// It is rehydrated at Start — the same call that loads the JSONL — and rendered
// into the system prompt, so a resumed session wakes up knowing its own goal
// and remaining steps without the user re-stating them. Absent state is not an
// error: a session that never sets one simply has no block.
//
// The tool is idempotent by design. The parameters are all optional and
// MERGE: a call with just {status: "done"} changes the status and leaves the
// goal, plan, and todo intact. Saving the same values again updates the
// timestamp and nothing else. A call with NO parameters is a READ — it returns
// the current state, so the model can consult it without a shell or a file.

// SessionState is the durable, model-managed state for one session.
//
// It is stored whole in a .state.json sidecar next to the session's .jsonl.
// The fields are optional at the JSON level (omitempty) so a partial update
// never writes zeros over the parts it did not touch — but in Go the struct is
// always the full object; a zero SessionState simply renders as "no state".
type SessionState struct {
	Goal      string    `json:"goal,omitempty"`
	Plan      []string  `json:"plan,omitempty"`
	Todo      []string  `json:"todo,omitempty"`
	Status    string    `json:"status,omitempty"`
	UpdatedAt time.Time `json:"updatedAt,omitempty"`
}

// empty reports whether nothing has been set. A state with only a timestamp is
// still "empty" for rendering: there is nothing to show a reader of the system
// prompt.
func (s SessionState) empty() bool {
	return s.Goal == "" && len(s.Plan) == 0 && len(s.Todo) == 0 && s.Status == ""
}

// StateFile returns the .state.json sidecar path for a session file, or "" when
// the session is in-memory only (no session file, no place to persist).
//
// It mirrors MetaFile's shape: same directory, same id stem, a different
// suffix. Keeping it beside the .jsonl means a resume that finds the
// conversation finds its state in the same directory, and a session with no
// file never tries to write one.
func StateFile(sessionFile string) string {
	if sessionFile == "" {
		return ""
	}
	dir := filepath.Dir(sessionFile)
	base := strings.TrimSuffix(filepath.Base(sessionFile), ".jsonl")
	return filepath.Join(dir, base+".state.json")
}

// LoadSessionState reads the .state.json sidecar for a session file, or a zero
// state when there is none (in-memory session) or it has not been written yet.
//
// Tolerant by design, exactly like LoadSessionMeta: a missing or malformed
// sidecar must never be the reason a resume fails. The conversation is the
// session; the state is an annotation of it.
func LoadSessionState(sessionFile string) SessionState {
	p := StateFile(sessionFile)
	if p == "" {
		return SessionState{}
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return SessionState{}
	}
	var st SessionState
	if json.Unmarshal(data, &st) != nil {
		return SessionState{}
	}
	return st
}

// SaveSessionState writes the state to the session's .state.json sidecar,
// atomically (temp + rename) and setting UpdatedAt to now — so saving the same
// values again is a no-op except for the timestamp, which is what makes the
// tool idempotent. A session with no file (in-memory) is not an error: the
// state is held on the Harness and will round-trip to the model, it simply
// will not survive a process restart, which is the documented trade-off of an
// in-memory session.
func SaveSessionState(sessionFile string, st SessionState) error {
	p := StateFile(sessionFile)
	if p == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	st.UpdatedAt = time.Now()
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// renderState formats the state for the system prompt. It reads like a status
// report the agent is being handed, not a dump of a struct — the goal first
// (the "why"), then the plan (the "how"), then the open todo (the "what is
// left"), then the status. A reader of the system prompt should be able to
// adopt the task from this block alone.
func renderState(st SessionState) string {
	if st.empty() {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n# Session state\n")
	b.WriteString("The session's durable goal and plan, set with the session_state tool. ")
	b.WriteString("Adopt it: this is what the session is for and what remains. ")
	b.WriteString("Update it as the work changes — it is the first thing a resumed or ")
	b.WriteString("compacted session reads, and the last thing a stale one forgives.\n")
	if st.Goal != "" {
		b.WriteString("- goal: " + st.Goal + "\n")
	}
	if st.Status != "" {
		b.WriteString("- status: " + st.Status + "\n")
	}
	if len(st.Plan) > 0 {
		b.WriteString("- plan:\n")
		for _, p := range st.Plan {
			b.WriteString("  - " + p + "\n")
		}
	}
	if len(st.Todo) > 0 {
		b.WriteString("- todo (still open):\n")
		for _, t := range st.Todo {
			b.WriteString("  - [ ] " + t + "\n")
		}
	}
	if !st.UpdatedAt.IsZero() {
		b.WriteString("(last updated " + st.UpdatedAt.Format("2006-01-02 15:04:05") + ")\n")
	}
	return b.String()
}

// sessionStateToolDef is the MCP tool the model calls to read or update the
// session's durable state. All parameters are optional: omit them all to read,
// or send any subset to merge. There is no "required" field because an
// empty call is a legal read, and that is the whole point — the model can
// consult the state without a shell.
func sessionStateToolDef() llm.ToolDef {
	var td llm.ToolDef
	td.Type = "function"
	td.Function.Name = "session_state"
	td.Function.Description = "Read or update the session's durable state: goal, plan, todo, status. " +
		"ALL parameters are optional and MERGE — a call with just {status} changes the status and " +
		"leaves the goal, plan, and todo intact. A call with NO parameters reads the current state. " +
		"Use it to keep a long task honest: set the goal and plan when you start, update the todo " +
		"as steps close, and set status as the work progresses. The state is persisted per session " +
		"and re-injected into the system prompt on resume, so a resumed session wakes up knowing " +
		"what it is for. Idempotent: saving the same values again only updates the timestamp."
	td.Function.Parameters = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"goal": map[string]any{
				"type":        "string",
				"description": "the task the session is for — the 'why', in one line",
			},
			"plan": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "the approach, as an ordered list of steps — the 'how'",
			},
			"todo": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "the steps still open — the 'what is left'; drop items as they close",
			},
			"status": map[string]any{
				"type":        "string",
				"description": "where the work stands, e.g. 'in_progress', 'blocked', 'done'",
			},
		},
	}
	return td
}

// sessionStateArgs is the tool's argument shape. Goal and Status are
// pointers so an omitted field is distinct from an empty string — an empty
// string is a legal value ("clear the status"), and a merge must not write
// zeros over the parts the call did not touch.
type sessionStateArgs struct {
	Goal   *string  `json:"goal"`
	Plan   []string `json:"plan"`
	Todo   []string `json:"todo"`
	Status *string  `json:"status"`
}

// withSessionState wraps a dispatcher so session_state is handled locally;
// everything else routes onward. It follows withRecap/withAsk exactly: parse
// the arguments, do the work, report through onCall, return a string result —
// errors are returned as strings, not as a non-nil error, so a failed call
// never aborts the turn.
func withSessionState(inner agent.ToolDispatcher, h *Harness, onCall func(string, map[string]any, string)) agent.ToolDispatcher {
	return func(ctx context.Context, tc llm.ToolCall) (string, error) {
		if tc.Function.Name != "session_state" {
			return inner(ctx, tc)
		}
		var args sessionStateArgs
		_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)

		// A read: no parameters at all.
		if args.Goal == nil && args.Status == nil && len(args.Plan) == 0 && len(args.Todo) == 0 {
			out := h.stateString()
			if onCall != nil {
				onCall("session_state", map[string]any{"read": true}, out)
			}
			return out, nil
		}

		st := h.stateMerge(args.Goal, args.Plan, args.Todo, args.Status)
		if err := SaveSessionState(h.cfg.SessionFile, st); err != nil {
			// A write failure is fed back, not fatal: the state is still held
			// in memory and round-trips to the model this turn.
			if onCall != nil {
				onCall("session_state", stateArgsMap(args), "state updated in memory (persist failed: "+err.Error()+")")
			}
			return "state updated in memory; persist failed: " + err.Error(), nil
		}
		out := "state updated and saved.\n" + renderState(st)
		if onCall != nil {
			onCall("session_state", stateArgsMap(args), out)
		}
		return out, nil
	}
}

// stateArgsMap builds the onCall argument map, omitting fields that were not
// sent, so the UI shows what was actually asked rather than zeros.
func stateArgsMap(args sessionStateArgs) map[string]any {
	m := map[string]any{}
	if args.Goal != nil {
		m["goal"] = *args.Goal
	}
	if len(args.Plan) > 0 {
		m["plan"] = args.Plan
	}
	if len(args.Todo) > 0 {
		m["todo"] = args.Todo
	}
	if args.Status != nil {
		m["status"] = *args.Status
	}
	return m
}

// stateMerge returns the current state with the provided fields applied.
// Only non-nil / non-empty arguments change the state; the rest carry over,
// which is what makes the tool idempotent and partial-safe.
func (h *Harness) stateMerge(goal *string, plan, todo []string, status *string) SessionState {
	h.stateMu.Lock()
	defer h.stateMu.Unlock()
	st := h.state
	if goal != nil {
		st.Goal = *goal
	}
	if len(plan) > 0 {
		st.Plan = plan
	}
	if len(todo) > 0 {
		st.Todo = todo
	}
	if status != nil {
		st.Status = *status
	}
	return st
}

// stateString renders the current in-memory state for a read call, or says
// plainly that none has been set.
func (h *Harness) stateString() string {
	h.stateMu.Lock()
	st := h.state
	h.stateMu.Unlock()
	if st.empty() {
		return "no session state set yet. Use session_state to set a goal, plan, todo, and status."
	}
	return renderState(st)
}

// applySessionState loads any persisted state for this session into the
// Harness. Called at Start, after the store is open and the Harness exists, so
// a resumed session (one that has a session file) finds its goal/plan/todo the
// same way it finds its conversation. No state is not an error: h.state stays
// zero and rebuildTools renders nothing. The sidecar path is derived from
// cfg.SessionFile at each read/write, so there is no separate path to keep in
// sync.
func (h *Harness) applySessionState() {
	h.stateMu.Lock()
	h.state = LoadSessionState(h.cfg.SessionFile)
	h.stateMu.Unlock()
}

// sessionStateBlock returns the rendered state for injection into the system
// prompt, or "" when there is none. It is the single place rebuildTools reads
// the state, so the prompt and the tool can never disagree about what is set.
func (h *Harness) sessionStateBlock() string {
	h.stateMu.Lock()
	st := h.state
	h.stateMu.Unlock()
	return renderState(st)
}
