package dun

import (
	"context"
	"strings"

	"github.com/iodesystems/agentkit/llm"
)

// Prompt rephrasing — make a vague request concrete BEFORE the agent acts on it.
//
// A one-line ask ("add a rephrase mode for dun") reaches the model as a
// conversation that is already three decisions deep (which files, which
// behavior, which tests) and never asked whether that was the intent. With
// rephrase on (/rephrase on), each user message first goes through one throwaway
// LLM call that rewrites it with specificity: a feature request comes back with
// the acceptance criteria, a vague question with the ambiguity resolved.
//
// The same guarantees as a commit message call:
//
//   - It is a TIP session — the instruction and the prompt travel in one
//     message and leave no trace in the conversation's context. The only thing
//     that enters the session is the result, and it enters AS the user
//     message: the model acts on the concrete prompt, and the human's input
//     box still shows what THEY typed.
//   - It is BOUNDED — one round-trip, no tools, no history.
//   - It is BEST-EFFORT — any failure falls back to the original prompt. A
//     rephrasing call must never be the reason a message is not acted on.
//
// Off by default: the extra round-trip is only worth it when asked for.

const rephraseInstruction = `Rewrite the user's message below so an expert coding agent can act on it without asking.

Rules:
- Keep the intent, scope, and any explicit constraints EXACTLY as stated. Never add requirements the user did not imply.
- Resolve ambiguity concretely: name the files, commands, or behaviors you believe are meant, and state any assumption as "assumption: …".
- For a feature request: add a short numbered acceptance criteria / test plan (what behavior, how it is verified).
- For a question: phrase it so exactly one answer is right; state what it does and does not cover.
- Keep it the user's voice — a prompt, not a summary about the prompt. No preamble, no closing remarks.
- Reply with ONLY the rewritten message. If the message is already concrete and unambiguous, reply with it unchanged.

User's message:
`

// Rephrase rewrites the user's message with specificity, ready to be acted on.
// It returns the original message (unchanged) on any error — rephrasing is
// best-effort and must never block the turn.
func (h *Harness) Rephrase(ctx context.Context, task string) (string, error) {
	trimmed := strings.TrimSpace(task)
	if trimmed == "" || h.Session == nil || h.Session.Runner == nil {
		return task, nil
	}
	// Slash-style directives are commands to the SESSION, not prose to
	// rewrite: "on"/"off" toggles, "yes", "continue" — rephrasing them would
	// change their meaning.
	if strings.HasPrefix(trimmed, "/") || len(strings.Fields(trimmed)) <= 2 {
		return task, nil
	}

	ch, err := h.Session.Runner.ChatStream(ctx,
		[]llm.Message{{Role: "user", Content: rephraseInstruction + task}}, nil, nil)
	if err != nil {
		return task, nil
	}
	var b strings.Builder
	for c := range ch {
		if c.Error != "" {
			return task, nil
		}
		b.WriteString(c.Content)
		if c.Done {
			break
		}
	}
	out := strings.TrimSpace(b.String())
	if out == "" || strings.TrimSpace(out) == trimmed {
		return task, nil
	}
	return out, nil
}
