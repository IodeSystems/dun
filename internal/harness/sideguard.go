package harness

import (
	"context"
	"fmt"
	"log"
	"os"
	"runtime/debug"
	"unicode/utf8"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
)

// sideGuard wraps every side call's runner (sideRunner). Two jobs:
//
//   - Input cap. A side call's request is clipped to fit the session's
//     window, less the reply's room. Rephrase sends the user's message whole
//     and repaint a file whole; a request that does not fit is refused, or on
//     a proxy that accepts anything, read whole and paid for. The largest
//     message is cut first, head and tail kept, with a marker.
//   - Reply check. A reply the provider cut at its length ceiling
//     (finish_reason "length") or a reply with nothing in it is a FAILED
//     call, an error chunk, not a short answer. Before, every reader took a
//     cut reply as complete: a fold committed a truncated summary.
//
// Each failure is logged once per kind and reason; a failed fold or rescue
// is also a notice (OnNotify), since the person loses history to it.
type sideGuard struct {
	inner agent.LLMRunner
	kind  string
	h     *Harness
}

// sideCapUnknown is the input cap when the window is not known: agentkit's
// context probe ceiling, its "safe lower bound" for windowing.
const sideCapUnknown = 32768

// sideCapFloor is the least a clipped message keeps, in tokens.
const sideCapFloor = 256

func (g sideGuard) ChatStream(ctx context.Context, msgs []llm.Message, tools []llm.ToolDef, opts *llm.ChatOpts) (<-chan llm.StreamChunk, error) {
	msgs = capSideInput(g.h.sideInputLimit(opts), msgs, tools, opts)
	ch, err := g.inner.ChatStream(ctx, msgs, tools, opts)
	if err != nil {
		return nil, err
	}
	out := make(chan llm.StreamChunk, cap(ch)+1)
	go func() {
		defer close(out)
		defer RecoverSide(g.kind)
		fail := func(c llm.StreamChunk, why string) {
			g.h.sideFailed(g.kind, why)
			out <- llm.StreamChunk{Error: why, Done: true, Usage: c.Usage}
			// The reader stops at an error; the rest is drained here.
			for range ch {
			}
		}
		got := false
		for c := range ch {
			if c.Error != "" {
				out <- c
				for range ch {
				}
				return
			}
			if c.Content != "" || c.ToolCall != nil {
				got = true
			}
			if c.Done {
				switch {
				case c.StopReason == llm.StopReasonLength || c.FinishReason == "length":
					fail(c, "the reply was cut at the length limit (finish_reason length)")
					return
				case !got:
					fail(c, "the model returned an empty reply")
					return
				}
			}
			out <- c
		}
		if !got {
			g.h.sideFailed(g.kind, "the model returned an empty reply")
			out <- llm.StreamChunk{Error: "the model returned an empty reply", Done: true}
		}
	}()
	return out, nil
}

// sideInputLimit is the most a side call may send, in tokens: 90% of the
// window less the reply's cap (an eighth of the window when it sets none).
func (h *Harness) sideInputLimit(opts *llm.ChatOpts) int {
	window := h.windowTokens()
	if window <= 0 {
		window = sideCapUnknown
	}
	// inputTokens counts a set max_tokens as input already; with none set,
	// an eighth of the window is the reply's room.
	reply := window / 8
	if opts != nil && opts.MaxTokens > 0 {
		reply = 0
	}
	return max(window*9/10-reply, sideCapFloor*4)
}

// capSideInput clips msgs to limit tokens (estimateTokens), the largest
// message first, each kept to its head and tail. The input slice is not
// changed. A request under the limit is returned as is.
func capSideInput(limit int, msgs []llm.Message, tools []llm.ToolDef, opts *llm.ChatOpts) []llm.Message {
	over := inputTokens(msgs, tools, opts) - limit
	if over <= 0 || len(msgs) == 0 {
		return msgs
	}
	out := append([]llm.Message(nil), msgs...)
	for range out {
		big := 0
		for i := range out {
			if len(out[i].Content) > len(out[big].Content) {
				big = i
			}
		}
		n := estimateTokens(out[big].Content)
		if n <= sideCapFloor {
			break
		}
		keep := max(n-over, sideCapFloor)
		before := len(out[big].Content)
		out[big].Content = clipSideMiddle(out[big].Content, keep*4)
		over -= (before - len(out[big].Content)) / 4
		if over <= 0 {
			break
		}
	}
	log.Printf("dun: side call input over ~%d tokens; clipped to fit", limit)
	return out
}

// clipSideMiddle keeps about n bytes of s, two thirds from the head and one
// from the tail, cut at rune boundaries, with a marker in place of the rest.
func clipSideMiddle(s string, n int) string {
	if len(s) <= n {
		return s
	}
	head, tail := n*2/3, n/3
	for head > 0 && !utf8.RuneStart(s[head]) {
		head--
	}
	t := len(s) - tail
	for t < len(s) && !utf8.RuneStart(s[t]) {
		t++
	}
	return s[:head] + fmt.Sprintf("\n[… %d characters cut to fit the window …]\n", t-head) + s[t:]
}

// sideFailed logs a failed side call once per kind and reason, and tells the
// person once when the call was a fold or a rescue.
func (h *Harness) sideFailed(kind, why string) {
	if _, seen := h.sideWarned.LoadOrStore(kind+"\x00"+why, true); seen {
		return
	}
	log.Printf("dun: side call %s failed: %s", kind, why)
	if (kind == "fold" || kind == "rescue") && h.cfg.OnNotify != nil {
		h.cfg.OnNotify("history fold failed: " + why)
	}
}

// RecoverSide, deferred at the root of a side call's goroutine, stops a
// panic there from killing the engine: the panic and its stack go to stderr
// and the goroutine ends. Side calls only — the turn loop has none.
func RecoverSide(kind string) {
	if r := recover(); r != nil {
		fmt.Fprintf(os.Stderr, "dun: side call %s panicked: %v\n%s", kind, r, debug.Stack())
	}
}
