package dun

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/iodesystems/agentkit/llm"
)

// Next-message suggestions — predict what the USER is likely to say next (with
// a rough probability), so the UI can offer quick picks.
//
// It is one extra, non-tool LLM round-trip, and it is ON by default
// (`--no-suggest` withholds it). That default is only defensible because of
// WHEN it now runs: the UI asks for it after the turn is done and the person
// has sat still for a few seconds with an empty input box, once per idle. It
// used to fire from the engine at the end of EVERY turn — including the
// autonomous ones a heartbeat provoked — which is how a session that nobody was
// typing into still made two calls a minute.
//
// The prompt goes through the session's own Build, not DefaultContextBuilder.
// Building it the raw way meant this call was the one request in dun that was
// never shaped (no LOD stubs, no compaction) and never logged — so "built
// prompt" undercounted the session's real traffic, and the unshaped copy could
// be larger than the turn it followed.

// The round-trip is measured (h.noteSideCall, kind "suggest") — the same
// reason as the "never logged" one: idle suggestions fire often enough that an
// unmeasured call is a cost no view can see.

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
	// The conversation so far, SHAPED — the same builder the turn itself uses,
	// so this request is compacted and measured like every other. No system
	// prompt: we append our own instruction instead.
	if h.Session == nil || h.Session.Build == nil {
		return nil, nil
	}
	msgs, err := h.Session.Build(ctx, h.Session.SessionID, "")
	if err != nil {
		return nil, err
	}
	if len(msgs) == 0 {
		return nil, nil
	}
	msgs = append(msgs, llm.Message{Role: "user", Content: suggestInstruction})

	start := time.Now()
	ch, err := h.Session.Runner.ChatStream(ctx, msgs, nil, &llm.ChatOpts{
		ResponseFormat: map[string]any{"type": "json_object"}, // JSON mode where supported
	})
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	var usage *llm.Usage
	for c := range ch {
		if c.Error != "" {
			h.noteSideCall("suggest", start, nil) // best-effort: count the failed call too
			return nil, nil
		}
		b.WriteString(c.Content)
		if c.Usage != nil {
			usage = c.Usage
		}
		if c.Done {
			break
		}
	}
	h.noteSideCall("suggest", start, usage)
	return parseSuggestions(b.String()), nil
}

// fillerPhrases are suggestions that carry no action — polite acknowledgments
// the model generates because it was trained to be agreeable. They make bad
// quick-picks: the user has nothing to do with them.
var fillerPhrases = map[string]struct{}{
	"looks good":                 {},
	"looks good to me":           {},
	"thanks":                     {},
	"thank you":                  {},
	"thank you!":                 {},
	"thanks!":                    {},
	"ok":                         {},
	"okay":                       {},
	"sure":                       {},
	"got it":                     {},
	"understood":                 {},
	"that's it":                  {},
	"that's all":                 {},
	"done":                       {},
	"all done":                   {},
	"i'm done":                   {},
	"i think that's it":          {},
	"i think that's all":         {},
	"nothing else":               {},
	"no thanks":                  {},
	"no thank you":               {},
	"i'm good":                   {},
	"i'm good, thanks":           {},
	"that's perfect":             {},
	"perfect":                    {},
	"great":                      {},
	"awesome":                    {},
	"nice":                       {},
	"cool":                       {},
	"good":                       {},
	"good job":                   {},
	"well done":                  {},
	"excellent":                  {},
	"brilliant":                  {},
	"wow":                        {},
	"wow, that's great":          {},
	"i don't have anything else": {},
	"i have nothing else":        {},
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
		if _, fill := fillerPhrases[strings.ToLower(sg.Text)]; fill {
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
