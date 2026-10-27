package dun

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Session state — durable, model-managed state for the SESSION, not the
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
// The state is the thing the log does not carry. It is small, structured,
// and written by the model itself: a goal, a plan, a todo list, and a status.
// It is persisted to a .state.json sidecar next to the session's .jsonl, keyed
// by session id, so it survives the process the same way the conversation does.
//
// It is rehydrated at Start — the same call that loads the JSONL — and rendered
// into the system prompt, so a resumed session wakes up knowing its own goal
// and remaining steps without the user re-stating them. Absent state is not an
// error: a session that never sets one simply has no block.
//
// The model reads and sets it with the session tool (session_tool.go). The
// parameters are all optional and MERGE: a call with just {status: "done"}
// changes the status and leaves the goal, plan, and todo intact. A call with
// NO parameters is a READ.

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
	UpdatedAt time.Time `json:"updatedAt,omitzero"`
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
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	st.UpdatedAt = time.Now()
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
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
	b.WriteString("What this session is for and what remains, set with the session tool. Keep it current.\n")
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

// sessionStateArgs is the state half of the session tool's arguments
// (session_tool.go). Goal and Status are pointers so an omitted field is
// distinct from an empty string — an empty string is a legal value ("clear
// the status"), and a merge must not write zeros over the parts the call did
// not touch.
type sessionStateArgs struct {
	Goal   *string  `json:"goal"`
	Plan   []string `json:"plan"`
	Todo   []string `json:"todo"`
	Status *string  `json:"status"`
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

// stateMerge applies the provided fields to the session's state, keeps the
// result, and returns it. Only non-nil / non-empty arguments change the
// state; the rest carry over, which is what makes the tool idempotent and
// partial-safe.
//
// It used to return the merge without keeping it, so a second call merged
// onto the state from before the first: {goal} then {status} saved a state
// with no goal.
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
	st.UpdatedAt = time.Now()
	h.state = st
	return st
}

// currentState is the session's state now.
func (h *Harness) currentState() SessionState {
	h.stateMu.Lock()
	defer h.stateMu.Unlock()
	return h.state
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
