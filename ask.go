package dun

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
)

// Human-in-the-loop — the ask_user tool.
//
// When the task is ambiguous or a decision is the user's to make, the agent
// calls ask_user{question, options}; the turn PAUSES at that tool call until the
// answer comes back (over the -p `ask` event → a UI picker → an `answer` event,
// or a terminal prompt), and the answer becomes the tool result so the turn
// continues. Synchronous by design: nothing should proceed until the user
// decides. (agentkit's lifting primitive could make this async/background; a
// blocking Ask is simpler and correct for interactive clarification.)

// AskFunc presents a question (with optional choices) to the user and returns
// their answer. It blocks until answered or ctx is done. multi = the user may
// pick several options (the answer is their joined selection).
type AskFunc func(ctx context.Context, question string, options []string, multi bool) (string, error)

func askToolDef() llm.ToolDef {
	var td llm.ToolDef
	td.Type = "function"
	td.Function.Name = "ask_user"
	td.Function.Description = "Ask the user ONE question when the task is ambiguous or a decision is theirs " +
		"to make (rather than guessing). Provide `options` for a multiple-choice pick, or omit them for " +
		"free text. Set `multi` to let the user select several options. Ask one question at a time — the " +
		"answer to one often guides the next. Returns the user's answer."
	td.Function.Parameters = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"question": map[string]any{"type": "string", "description": "the question to ask"},
			"options": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "optional choices to pick from",
			},
			"multi": map[string]any{
				"type":        "boolean",
				"description": "allow selecting multiple options (default false = pick one)",
			},
		},
		"required": []string{"question"},
	}
	return td
}

// withAsk wraps a dispatcher so ask_user is handled locally (via the AskFunc);
// everything else routes onward.
func withAsk(inner agent.ToolDispatcher, ask AskFunc, onCall func(string, map[string]any, string)) agent.ToolDispatcher {
	return func(ctx context.Context, tc llm.ToolCall) (string, error) {
		if tc.Function.Name != "ask_user" {
			return inner(ctx, tc)
		}
		var args struct {
			Question string   `json:"question"`
			Options  []string `json:"options"`
			Multi    bool     `json:"multi"`
		}
		_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
		if strings.TrimSpace(args.Question) == "" {
			return "ERROR: ask_user requires a question", nil
		}
		ans, err := ask(ctx, args.Question, args.Options, args.Multi)
		if err != nil {
			// Feed the failure back to the model rather than aborting the turn.
			return "ERROR: could not get an answer: " + err.Error(), nil
		}
		if onCall != nil {
			onCall("ask_user", map[string]any{"question": args.Question, "options": args.Options, "multi": args.Multi}, ans)
		}
		return ans, nil
	}
}
