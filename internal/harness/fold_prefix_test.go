package harness

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
)

// reqRecorder records each request: its messages, tools and cache key.
type reqRecorder struct {
	reply string
	msgs  [][]llm.Message
	tools [][]llm.ToolDef
	keys  []string
}

func (r *reqRecorder) ChatStream(_ context.Context, msgs []llm.Message, tools []llm.ToolDef, opts *llm.ChatOpts) (<-chan llm.StreamChunk, error) {
	r.msgs = append(r.msgs, append([]llm.Message(nil), msgs...))
	r.tools = append(r.tools, tools)
	key := ""
	if opts != nil {
		key = opts.PromptCacheKey
	}
	r.keys = append(r.keys, key)
	return replay(r.reply, ""), nil
}

// A fold under budget pressure sends the round's own prefix: the effective
// system (slot note and heredoc tool text included), the same tools, the
// same prompt-cache key, and the history as the round sent it, so the
// provider serves it from the session's cache. Real agent.Session turns
// through measuredBuild, the builder dun wires, on both transports.
func TestFold_SharesTheRoundsPrefix(t *testing.T) {
	defs := []llm.ToolDef{{Type: "function"}}
	defs[0].Function.Name = "exec"
	for _, format := range []agent.ToolFormat{agent.ToolFormatNative, agent.ToolFormatHeredoc} {
		h := newNoteHarness(t)
		h.sideCalls = &sideCallStats{}
		turns := &reqRecorder{reply: "ok"}
		folds := &reqRecorder{reply: foldGoodSummary(repoAsk)}
		sh := &agent.Shaper{Store: h.store, Runner: folds, Estimate: &h.meter,
			Policy: agent.ShaperPolicy{BudgetTokens: 1 << 20, PreserveLastMessages: 6,
				PreserveLastToolCalls: 4, PinLatestUser: true}}
		h.foldWire(sh)
		h.Session = &agent.Session{SessionID: "dun", Store: h.store, Runner: turns,
			System: "You are dun.", Tools: defs, ToolFormat: format,
			Build: measuredBuild(sh, h), ChatOpts: &llm.ChatOpts{}, Estimate: &h.meter}
		ctx := context.Background()
		// Results under the LOD threshold, so the round sends them whole. The
		// older ones small and the kept tail large, so the fold of the older
		// ones fits one request: a fold larger than the budget is staged and
		// keeps the worker prompt.
		_ = h.store.Append(ctx, "dun", agent.Entry{ID: "ask", Kind: agent.KindUser, Content: repoAsk, CreatedAt: 1})
		for i := range 16 {
			id, lines := "tc"+itoa(i), 10
			if i >= 12 {
				lines = 900
			}
			_ = h.store.Append(ctx, "dun", agent.Entry{ID: "c" + itoa(i), Kind: agent.KindToolCall, ToolName: "exec",
				ToolCallID: id, Content: `{"cmd":"cat f` + itoa(i) + `"}`, CreatedAt: int64(10 + i*2)})
			_ = h.store.Append(ctx, "dun", agent.Entry{ID: "r" + itoa(i), Kind: agent.KindToolResult, ToolName: "exec",
				ToolCallID: id, Content: strings.Repeat("f"+itoa(i)+"\n", lines), CreatedAt: int64(11 + i*2)})
		}
		if _, err := h.Session.Turn(ctx); err != nil { // the round the cache holds
			t.Fatal(err)
		}
		if len(folds.msgs) != 0 {
			t.Fatalf("%q: folded with the budget unbounded", format)
		}
		_ = h.store.Append(ctx, "dun", agent.Entry{ID: "u2", Kind: agent.KindUser, Content: "go on", CreatedAt: 100})
		sh.Policy.BudgetTokens = 3500 // the next build must fold
		if _, err := h.Session.Turn(ctx); err != nil {
			t.Fatal(err)
		}
		if len(folds.msgs) == 0 {
			t.Fatalf("%q: no fold", format)
		}
		round, fold := turns.msgs[0], folds.msgs[0]
		if !reflect.DeepEqual(fold[0], round[0]) {
			t.Errorf("%q: fold system differs from the round's\nround: %q\nfold:  %q", format, round[0].Content, fold[0].Content)
		}
		if !reflect.DeepEqual(folds.tools[0], turns.tools[0]) {
			t.Errorf("%q: fold sent %d tools, the round %d", format, len(folds.tools[0]), len(turns.tools[0]))
		}
		if folds.keys[0] == "" || folds.keys[0] != turns.keys[0] {
			t.Errorf("%q: fold cache key %q, the round's %q", format, folds.keys[0], turns.keys[0])
		}
		// Every message before the summary instruction is the round's, in
		// order: the system, the ask, the oldest tool pairs. The fold leaves
		// out the tail it keeps, so it is shorter than the round.
		same := 0
		for same < len(fold)-1 && same < len(round) && reflect.DeepEqual(fold[same], round[same]) {
			same++
		}
		if same != len(fold)-1 || same < 3 {
			t.Errorf("%q: fold shares %d leading messages with the round, want all %d before the instruction",
				format, same, len(fold)-1)
		}
		// Still a fold to foldRunner: the summary lacks the ask ("go on"),
		// so the check asks once more, and both calls are measured.
		if last := fold[len(fold)-1]; last.Role != "user" || !strings.Contains(last.Content, "go on") {
			t.Errorf("%q: the fold request does not end with the summary instruction: %+v", format, last)
		}
		if len(folds.msgs) != 2 {
			t.Errorf("%q: %d fold calls, want the first and the check's retry", format, len(folds.msgs))
		}
		if got := h.SideCalls()["fold"]; got == nil || got["calls"].(int) != 2 {
			t.Errorf("%q: fold side calls = %v, want 2", format, got)
		}
	}
}
