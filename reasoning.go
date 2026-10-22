package dun

import (
	"context"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
)

// Live reasoning. agentkit streams the model's reasoning on every chunk
// (llm.StreamChunk.Reasoning) and stamps the round's whole text on the entry
// it produced, but its loop has no callback for the deltas — only
// OnAssistantToken, for the reply. A UI that wants to show thinking AS it
// happens has to read the stream itself.
//
// reasoningTee is that reader: it sits between the Session and the LLM,
// forwards every chunk unchanged, and reports each reasoning delta on the
// way past. Only the TURN's rounds go through it. The side calls that borrow
// Session.Runner (suggest, rephrase, commit messages, the memory fork) call
// untee first: their thinking is not the conversation's, and showing it
// would put a suggestion's reasoning in the middle of a reply.

type reasoningTee struct {
	agent.LLMRunner
	on func(string)
}

// teeReasoning wraps r so on sees each reasoning delta. nil on → r itself, so
// a host that does not ask pays nothing.
func teeReasoning(r agent.LLMRunner, on func(string)) agent.LLMRunner {
	if r == nil || on == nil {
		return r
	}
	return &reasoningTee{LLMRunner: r, on: on}
}

// untee is the runner under a tee, for a call that is not the turn's.
func untee(r agent.LLMRunner) agent.LLMRunner {
	if t, ok := r.(*reasoningTee); ok {
		return t.LLMRunner
	}
	return r
}

func (t *reasoningTee) ChatStream(ctx context.Context, msgs []llm.Message, tools []llm.ToolDef, opts *llm.ChatOpts) (<-chan llm.StreamChunk, error) {
	in, err := t.LLMRunner.ChatStream(ctx, msgs, tools, opts)
	if err != nil || in == nil {
		return in, err
	}
	out := make(chan llm.StreamChunk, cap(in))
	go func() {
		defer close(out)
		for c := range in {
			if c.Reasoning != "" {
				t.on(c.Reasoning)
			}
			select {
			case out <- c:
			case <-ctx.Done():
				// The reader has gone. Drain, so the producer under us is not
				// left blocked on a send nobody will take.
				for range in {
				}
				return
			}
		}
	}()
	return out, nil
}
