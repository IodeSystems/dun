package dun

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
)

// Next-message suggestions — after a turn, predict what the USER is likely to
// say next (with a rough probability), so the UI can offer quick picks. It's one
// extra, non-tool LLM round-trip over the current conversation; opt-in
// (`dun --suggest`) since it costs a call per turn.

// Suggestion is one predicted next user message.
type Suggestion struct {
	Text string  `json:"text"`
	Prob float64 `json:"prob"`
}

const suggestInstruction = `Based on the conversation above between a user and a coding agent, predict the 3 messages the USER is MOST likely to send NEXT. Reply with ONLY this JSON, nothing else:
{"suggestions":[{"text":"…","prob":0.0}]}
- text: a short message the user would actually type next, in the first person (a command, a question, "yes", "go ahead", "now run the tests", …).
- prob: your rough estimate of how likely it is, 0..1 (they need not sum to 1).
Order by prob, highest first.`

// Suggestions returns the model's predicted next user messages. Errors (or a
// model that won't produce JSON) yield nil — suggestions are best-effort.
func (h *Harness) Suggestions(ctx context.Context) ([]Suggestion, error) {
	// The conversation so far (no system prompt — we append our own instruction).
	msgs, err := agent.DefaultContextBuilder(ctx, h.store, h.Session.SessionID, "")
	if err != nil {
		return nil, err
	}
	if len(msgs) == 0 {
		return nil, nil
	}
	msgs = append(msgs, llm.Message{Role: "user", Content: suggestInstruction})

	ch, err := h.Session.Runner.ChatStream(ctx, msgs, nil, &llm.ChatOpts{
		ResponseFormat: map[string]any{"type": "json_object"}, // JSON mode where supported
	})
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	for c := range ch {
		if c.Error != "" {
			return nil, nil // give up quietly
		}
		b.WriteString(c.Content)
		if c.Done {
			break
		}
	}
	return parseSuggestions(b.String()), nil
}

// parseSuggestions extracts the JSON object (defensively — small models like to
// wrap it in prose) and returns up to 4 cleaned, prob-sorted suggestions.
func parseSuggestions(s string) []Suggestion {
	i := strings.IndexByte(s, '{')
	j := strings.LastIndexByte(s, '}')
	if i < 0 || j <= i {
		return nil
	}
	var out struct {
		Suggestions []Suggestion `json:"suggestions"`
	}
	if json.Unmarshal([]byte(s[i:j+1]), &out) != nil {
		return nil
	}
	res := out.Suggestions[:0:0]
	for _, sg := range out.Suggestions {
		sg.Text = strings.TrimSpace(sg.Text)
		if sg.Text == "" {
			continue
		}
		if sg.Prob < 0 {
			sg.Prob = 0
		} else if sg.Prob > 1 {
			sg.Prob = 1
		}
		res = append(res, sg)
	}
	sort.SliceStable(res, func(a, b int) bool { return res[a].Prob > res[b].Prob })
	if len(res) > 4 {
		res = res[:4]
	}
	return res
}
