package dun

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/iodesystems/agentkit/llm"
)

// Forks — one side call that continues the conversation with a single
// appended instruction and is made to answer with a call to ONE tool.
//
// The request starts with exactly the turn's prefix (forkSystem, forkTools —
// see suggest.go), so it is a prompt-cache hit and costs only the instruction
// and the generation (plan item 9 M1: 4,870 of 5,152 prompt tokens cached).
// The tool is forced by tool_choice, which changes the grammar, not the
// prompt; measured to keep the prefix. Every tool stays in the request — that
// is what keeps the prefix — so only the forced tool's calls are run; any
// other call is dropped unrun.
//
// A fork runs between turns, never inside one: it holds turnMu, so a turn that
// starts meanwhile waits for it, and a fork asked for during a turn waits for
// the turn.

// fork runs instruction against the conversation so far, forcing a call to
// only, and hands each call of it to run. It returns run's results, in order.
func (h *Harness) fork(ctx context.Context, kind, instruction string, only llm.ToolDef, run func(args string) string) ([]string, error) {
	h.turnMu.Lock()
	defer h.turnMu.Unlock()
	s := h.Session
	if s == nil || s.Build == nil || s.Runner == nil {
		return nil, fmt.Errorf("no session")
	}
	msgs, err := s.Build(ctx, s.SessionID, forkSystem(s))
	if err != nil {
		return nil, err
	}
	msgs = append(msgs, llm.Message{Role: "user", Content: instruction})

	tools, _ := forkTools(s)
	opts := &llm.ChatOpts{}
	name := only.Function.Name
	if len(tools) > 0 {
		choice, _ := json.Marshal(map[string]any{"type": "function", "function": map[string]any{"name": name}})
		opts.ToolChoice = string(choice)
	} else if len(s.Tools) > 0 {
		// Heredoc transport: the tools are in the system prompt and calls are
		// content shaped by a grammar. A grammar over only this tool forces it.
		opts.Grammar = llm.HeredocGrammar([]llm.ToolDef{only})
	}

	start := time.Now()
	ch, err := s.Runner.ChatStream(ctx, msgs, tools, opts)
	if err != nil {
		h.noteSideCall(kind, start, nil)
		return nil, err
	}
	var content strings.Builder
	var calls []llm.ToolCall
	var usage *llm.Usage
	for c := range ch {
		if c.Error != "" {
			h.noteSideCall(kind, start, nil)
			return nil, fmt.Errorf("%s", c.Error)
		}
		content.WriteString(c.Content)
		if c.ToolCall != nil {
			calls = append(calls, *c.ToolCall)
		}
		if c.Usage != nil {
			usage = c.Usage
		}
		if c.Done {
			break
		}
	}
	h.noteSideCall(kind, start, usage)
	if len(tools) == 0 && len(s.Tools) > 0 {
		parsed, _, err := llm.ParseToolCalls(content.String())
		if err != nil {
			return nil, fmt.Errorf("the reply did not parse as a tool call: %w", err)
		}
		calls = parsed
	}
	var out []string
	for _, tc := range calls {
		if tc.Function.Name == name {
			out = append(out, run(tc.Function.Arguments))
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("the model did not call %s", name)
	}
	return out, nil
}

// rememberInstruction asks for notes from the conversation; with what set,
// for that one thing.
func rememberInstruction(what string) string {
	const rules = " Write each note to stand alone — the reader will not see this conversation: the fact, why it " +
		"holds, and where it applies, plus the question it answers in a later user's words. Do not save what " +
		"the code, the git history or the docs already record."
	if what = strings.TrimSpace(what); what != "" {
		return "The user asked you to remember this: " + what + "\n\nSave it with ONE memory call (action add)," +
			" using the conversation above to make it complete." + rules
	}
	return "The user asked you to remember what matters from the conversation above. Save what a later " +
		"session on this project would need and could not find elsewhere — the user's preferences and " +
		"corrections, decisions and their reasons, pitfalls found the hard way — one memory call (action add) " +
		"per fact." + rules
}

// Remember runs the memory fork: notes from the conversation, or the one
// thing the user named. It returns a line for the user.
func (h *Harness) Remember(ctx context.Context, what string) (string, error) {
	m := h.memoryStore()
	if m == nil {
		return "", fmt.Errorf("memory needs the docs index — /rag on first (a note nothing can search for is never read again)")
	}
	results, err := h.fork(ctx, "remember", rememberInstruction(what), memoryToolDef(), func(args string) string {
		return h.memoryCall(m, args)
	})
	if err != nil {
		return "", err
	}
	return strings.Join(results, "; "), nil
}
