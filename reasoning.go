package dun

import (
	"context"
	"time"

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
	// round, when set, records what each round streams, for an interrupt to
	// save (interrupt.go). Every ChatStream call through the tee is a round of
	// the turn: side calls untee first.
	round *roundCapture
}

// teeTurn is the turn's runner: teed for live reasoning (when on is set) and
// for the interrupt's partial capture.
func (h *Harness) teeTurn(r agent.LLMRunner, on func(string)) agent.LLMRunner {
	if r == nil {
		return r
	}
	return &reasoningTee{LLMRunner: r, on: on, round: &h.round}
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
	// A turn already cut (an interrupt during a tool call) does not start
	// another round.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	in, err := t.LLMRunner.ChatStream(ctx, msgs, tools, opts)
	if err != nil || in == nil {
		return in, err
	}
	if t.round != nil {
		t.round.begin()
	}
	out := make(chan llm.StreamChunk, cap(in))
	go func() {
		defer close(out)
		sawDone := false
		for c := range in {
			if t.round != nil {
				t.round.add(c)
			}
			if c.Reasoning != "" && t.on != nil {
				t.on(c.Reasoning)
			}
			sawDone = sawDone || c.Done
			select {
			case out <- c:
			case <-ctx.Done():
				// The reader may have gone. Drain, so the producer under us is
				// not left blocked on a send nobody will take.
				go func() {
					for range in {
					}
				}()
				if !sawDone {
					cancelled(ctx, out)
				}
				return
			}
		}
		if ctx.Err() != nil && !sawDone {
			cancelled(ctx, out)
		}
	}()
	return out, nil
}

// teeGrace is how long the tee offers a cancelled stream's error to a reader
// that may already have gone.
const teeGrace = time.Second

// cancelled tells a reader still ranging over out that the stream was cut.
// A plain close would read as a finished round: agentkit would persist the
// partial text as the reply and end the turn as if it had succeeded, which is
// what an interrupt is NOT.
func cancelled(ctx context.Context, out chan<- llm.StreamChunk) {
	select {
	case out <- llm.StreamChunk{Error: "stream cut: " + context.Cause(ctx).Error()}:
	case <-time.After(teeGrace):
	}
}
