package harness

import (
	"encoding/json"
	"fmt"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/dun/internal/textfmt"
)

// textcall.go — a tool call the model wrote as text (USER 2026-11-18). A
// DeepSeek Flash session sent `<｜DSML｜…<invoke>…<parameter>` in its reply
// and no tool_calls: nothing ran and the turn ended. A reply with no tool
// calls whose text (outside code) has tool-call markup is a failed tool call:
// onIdle tells the model, at most textCallsMax times a turn, and the turn
// goes on. Inside the person's turn, so not an automatic turn and not under
// its budget (autoturn.go). corrallm may repair it upstream; this stays as the
// fallback. docs/decisions.md, "Tool calls sent as text are retried".

// textCallsMax is how many corrections one turn gets; past it the turn ends
// as before.
const textCallsMax = 2

// textCallNote is the correction; %s names the markup.
const textCallNote = "[automatic] your last reply had a tool call written as text (%s); it did not run. Call the tool through the tool API, not in the reply's text."

// textCallRefused is a tool's answer to an argument with the markup; %[1]s
// is the tool, %[2]s the markup.
const textCallRefused = "ERROR: %[1]s refused: an argument has a tool call written as text (%[2]s). Text never runs a tool: call the tool through the tool API, and keep tool-call markup out of %[1]s's arguments."

// textCallCheck is the correction when the round that just ended wrote its
// tool call as text and the turn has corrections left, else "".
func (h *Harness) textCallCheck() string {
	if h.store == nil {
		return ""
	}
	e, ok := h.store.lastRoundEntry()
	if !ok || e.Kind != agent.KindAssistant {
		return ""
	}
	what := textfmt.ToolMarkup(e.Content)
	if what == "" || !h.idle.textCallTake() {
		return ""
	}
	return fmt.Sprintf(textCallNote, what)
}

// argsMarkup names the tool-call markup in any string of a call's JSON
// arguments, "" with none.
func argsMarkup(args string) string {
	var v any
	if json.Unmarshal([]byte(args), &v) != nil {
		return ""
	}
	return markupAny(v)
}

func markupAny(v any) string {
	switch x := v.(type) {
	case string:
		return textfmt.ToolMarkup(x)
	case []any:
		for _, e := range x {
			if w := markupAny(e); w != "" {
				return w
			}
		}
	case map[string]any:
		for _, e := range x {
			if w := markupAny(e); w != "" {
				return w
			}
		}
	}
	return ""
}
