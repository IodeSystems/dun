package dun

import (
	"context"
	"encoding/json"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
)

// The session tool: what is running the model (harnessinfo.go) and the
// session's durable state (sessionstate.go), in one call (USER 2026-10-27:
// "session state and harness seem like they are doing the same job? collapse
// those"). Both answer "what is this session?"; one tool costs less to send
// and is one less choice for a weak model.
//
// No arguments reads. Any of goal, plan, todo, status merges that field and
// keeps the others. Every call returns the same JSON, so a model that set a
// field sees the whole result of setting it.
//
// The old names still dispatch, hidden from the tool list, so a resumed
// session or a model's habit still works: harness reads, session_state reads
// or merges with its arguments.

// sessionToolNames are the names withSession answers: the tool and its
// hidden aliases.
var sessionToolNames = map[string]bool{"session": true, "harness": true, "session_state": true}

func sessionToolDef() llm.ToolDef {
	var td llm.ToolDef
	td.Type = "function"
	td.Function.Name = "session"
	td.Function.Description = "This session as JSON: `harness` is what runs you (dun, its version, the model, " +
		"the session id, the git branch), and `state` is the task's goal, plan, todo and status. When a project's " +
		"rules ask a commit, PR or report to name the agent, model or version, copy them from `harness`; do not " +
		"guess. On a long task keep `state` current: a field you send replaces that field and the others are " +
		"kept. You see the state again after a restart or a compaction. No arguments reads."
	td.Function.Parameters = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"goal":   map[string]any{"type": "string", "description": "what the task is for, in one line"},
			"plan":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "the steps, in order"},
			"todo":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "the steps still open"},
			"status": map[string]any{"type": "string", "description": "where the work is, e.g. in_progress, blocked, done"},
		},
	}
	return td
}

// sessionReply is what every session call returns.
type sessionReply struct {
	Harness HarnessInfo  `json:"harness"`
	State   SessionState `json:"state"`
	// Error is set when a merge was kept in memory but not saved.
	Error string `json:"error,omitempty"`
}

// withSession answers session, and its aliases harness and session_state,
// locally. A failed save is in the reply, not an error: the merge still
// holds in memory for this process.
func withSession(inner agent.ToolDispatcher, h *Harness, onCall func(string, map[string]any, string)) agent.ToolDispatcher {
	return func(ctx context.Context, tc llm.ToolCall) (string, error) {
		name := tc.Function.Name
		if !sessionToolNames[name] {
			return inner(ctx, tc)
		}
		var args sessionStateArgs
		if name != "harness" { // harness took no arguments; ignore any it is sent
			_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
		}
		reply := sessionReply{Harness: h.HarnessInfo()}
		if args.Goal == nil && args.Status == nil && len(args.Plan) == 0 && len(args.Todo) == 0 {
			reply.State = h.currentState()
		} else {
			reply.State = h.stateMerge(args.Goal, args.Plan, args.Todo, args.Status)
			if err := SaveSessionState(h.cfg.SessionFile, reply.State); err != nil {
				reply.Error = "kept in memory; not saved: " + err.Error()
			}
		}
		b, _ := json.MarshalIndent(reply, "", "  ")
		out := string(b)
		if onCall != nil {
			onCall("session", stateArgsMap(args), out)
		}
		return out, nil
	}
}
