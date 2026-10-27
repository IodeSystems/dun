package dun

import (
	"context"
	"encoding/json"
	"slices"
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

// AskFunc presents a question and its choices to the user and returns their
// answer. It blocks until answered or ctx is done. multi = the user may pick
// several options (the answer is their joined selection). recommended is what
// the model recommends: exactly one option, or for multi any number of them
// (the ones to check). The user may always answer in their own words instead.
type AskFunc func(ctx context.Context, question string, options []string, multi bool, recommended []string) (string, error)

// askMinOptions: a question with fewer choices pushes all the work onto the
// user. USER 2026-10-21: an ask with no options or recommendation is invalid.
const askMinOptions = 2

func askToolDef() llm.ToolDef {
	var td llm.ToolDef
	td.Type = "function"
	td.Function.Name = "ask_user"
	// Models left `multi` off even for "which of these should I fix?" while
	// the text spoke only of one pick and one recommendation. So the two kinds
	// of question are named side by side, each with what it is for.
	td.Function.Description = "Ask the user one question when the task is ambiguous or the decision is theirs, " +
		"instead of guessing. Give at least two concrete `options` and say in the question why you recommend " +
		"what you do; the user can also answer in their own words.\n" +
		"- Pick several (`multi`: true) when the options do not exclude each other (which of these to fix, " +
		"enable, remove): ask once for the list, not once per item.\n" +
		"- Pick one (the default) when only one can be chosen (which approach, yes or no).\n" +
		"Returns the answer; several picks come back comma-separated."
	td.Function.Parameters = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"question": map[string]any{"type": "string", "description": "the question to ask"},
			"options": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"minItems":    askMinOptions,
				"description": "at least two concrete choices",
			},
			"recommended": map[string]any{
				"anyOf": []any{
					map[string]any{"type": "string"},
					map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				},
				"description": "the option you recommend, or for multi an array of them, each as written in options",
			},
			"multi": map[string]any{
				"type":        "boolean",
				"description": "true: the user may pick several",
			},
		},
		"required": []string{"question", "options", "recommended"},
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
			Question    string          `json:"question"`
			Options     []string        `json:"options"`
			Multi       bool            `json:"multi"`
			Recommended json.RawMessage `json:"recommended"`
		}
		_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
		rec := askRecommended(args.Recommended, args.Options, args.Multi)
		if msg := checkAsk(args.Question, args.Options, args.Multi, rec); msg != "" {
			// Back to the model, which asks again properly; the user sees
			// nothing until it does.
			return "ERROR: " + msg, nil
		}
		ans, err := ask(ctx, args.Question, args.Options, args.Multi, rec)
		if err != nil {
			// Feed the failure back to the model rather than aborting the turn.
			return "ERROR: could not get an answer: " + err.Error(), nil
		}
		if onCall != nil {
			onCall("ask_user", map[string]any{"question": args.Question, "options": args.Options, "multi": args.Multi,
				"recommended": rec}, ans)
		}
		return ans, nil
	}
}

// askRecommended reads `recommended`: a string or an array of strings. For
// multi, a string that is no option but whose comma-separated parts all are
// is taken as that list.
func askRecommended(raw json.RawMessage, options []string, multi bool) []string {
	var list []string
	if json.Unmarshal(raw, &list) == nil {
		return list
	}
	var one string
	if json.Unmarshal(raw, &one) != nil || one == "" {
		return nil
	}
	if multi && !slices.Contains(options, one) {
		var parts []string
		for _, p := range strings.Split(one, ",") {
			if p = strings.TrimSpace(p); !slices.Contains(options, p) {
				return []string{one} // checkAsk says why
			}
			parts = append(parts, p)
		}
		return parts
	}
	return []string{one}
}

// checkAsk is why an ask_user call is not fit to show, or "".
func checkAsk(question string, options []string, multi bool, recommended []string) string {
	if strings.TrimSpace(question) == "" {
		return "ask_user requires a question"
	}
	n := 0
	for _, o := range options {
		if strings.TrimSpace(o) != "" {
			n++
		}
	}
	if n < askMinOptions {
		return "ask_user needs at least two concrete options (the user can still type their own answer) " +
			"and the one you recommend in `recommended` — ask again with them"
	}
	if !multi && len(recommended) != 1 {
		if len(recommended) > 1 {
			return "ask_user `recommended` names several options but `multi` is false — set `multi` " +
				"to true if the user may pick several, or recommend one — ask again"
		}
		return "ask_user needs the one option you recommend in `recommended` — ask again"
	}
	for _, r := range recommended {
		if !slices.Contains(options, r) {
			return "ask_user `recommended` must name options exactly as written — ask again"
		}
	}
	return ""
}
