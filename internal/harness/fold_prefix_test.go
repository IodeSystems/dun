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

// A fold under budget pressure sends the round's own prefix on its first
// request: the effective system (slot note and heredoc tool text included),
// the same tools, the same prompt-cache key, and the history as the round
// sent it, so the provider serves it from the session's cache. A fold too
// big for one request is split (agentkit v0.34.0): the first request still
// carries the prefix over the head of the history, and the later stages go
// out under the worker prompt with no tools and no key. Real agent.Session
// turns through measuredBuild, the builder dun wires, on both transports.
func TestFold_SharesTheRoundsPrefix(t *testing.T) {
	for _, tc := range []struct {
		name string
		// lines is the size of tool result i, in lines.
		lines  func(i int) int
		budget int
		split  bool
	}{
		// The older results small and the kept tail large, under a budget
		// that still folds: the fold of the older ones fits one request.
		{name: "whole", lines: largeTail, budget: 12000},
		// Every result the same size: the fold does not fit one request.
		// Before v0.34.0 such a fold was staged whole under the worker
		// prompt and read nothing from the cache.
		{name: "split", lines: func(int) int { return 400 }, budget: 5000, split: true},
	} {
		for _, format := range []agent.ToolFormat{agent.ToolFormatNative, agent.ToolFormatHeredoc} {
			foldSharesTheRoundsPrefix(t, tc.name+"/"+formatName(format), tc.lines, tc.budget, tc.split, format)
		}
	}
}

func formatName(f agent.ToolFormat) string {
	if f == agent.ToolFormatNative {
		return "native"
	}
	return string(f)
}

func foldSharesTheRoundsPrefix(t *testing.T, name string, lines func(int) int, budget int, split bool, format agent.ToolFormat) {
	defs := []llm.ToolDef{{Type: "function"}}
	defs[0].Function.Name = "exec"
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
	// Results under the LOD threshold, so the round sends them whole.
	_ = h.store.Append(ctx, "dun", agent.Entry{ID: "ask", Kind: agent.KindUser, Content: repoAsk, CreatedAt: 1})
	for i := range 16 {
		id := "tc" + itoa(i)
		_ = h.store.Append(ctx, "dun", agent.Entry{ID: "c" + itoa(i), Kind: agent.KindToolCall, ToolName: "exec",
			ToolCallID: id, Content: `{"cmd":"cat f` + itoa(i) + `"}`, CreatedAt: int64(10 + i*2)})
		_ = h.store.Append(ctx, "dun", agent.Entry{ID: "r" + itoa(i), Kind: agent.KindToolResult, ToolName: "exec",
			ToolCallID: id, Content: strings.Repeat("f"+itoa(i)+"\n", lines(i)), CreatedAt: int64(11 + i*2)})
	}
	if _, err := h.Session.Turn(ctx); err != nil { // the round the cache holds
		t.Fatal(err)
	}
	if len(folds.msgs) != 0 {
		t.Fatalf("%s: folded with the budget unbounded", name)
	}
	_ = h.store.Append(ctx, "dun", agent.Entry{ID: "u2", Kind: agent.KindUser, Content: "go on", CreatedAt: 100})
	sh.Policy.BudgetTokens = budget // the next build must fold
	if _, err := h.Session.Turn(ctx); err != nil {
		t.Fatal(err)
	}
	if len(folds.msgs) == 0 {
		t.Fatalf("%s: no fold", name)
	}
	round, fold := turns.msgs[0], folds.msgs[0]
	if !reflect.DeepEqual(fold[0], round[0]) {
		t.Errorf("%s: fold system differs from the round's\nround: %q\nfold:  %q", name, round[0].Content, fold[0].Content)
	}
	if !reflect.DeepEqual(folds.tools[0], turns.tools[0]) {
		t.Errorf("%s: fold sent %d tools, the round %d", name, len(folds.tools[0]), len(turns.tools[0]))
	}
	if folds.keys[0] == "" || folds.keys[0] != turns.keys[0] {
		t.Errorf("%s: fold cache key %q, the round's %q", name, folds.keys[0], turns.keys[0])
	}
	// Every message before the summary instruction is the round's, in
	// order: the system, the ask, the oldest tool pairs. The fold leaves
	// out the tail it keeps, so it is shorter than the round.
	same := 0
	for same < len(fold)-1 && same < len(round) && reflect.DeepEqual(fold[same], round[same]) {
		same++
	}
	if same != len(fold)-1 || same < 3 {
		t.Errorf("%s: fold shares %d leading messages with the round, want all %d before the instruction",
			name, same, len(fold)-1)
	}
	if last := fold[len(fold)-1]; last.Role != "user" || !strings.Contains(last.Content, "go on") {
		t.Errorf("%s: the fold request does not end with the summary instruction: %+v", name, last)
	}
	// Still a fold to foldRunner, every request of it (agent.IsFold): the
	// summary lacks the ask ("go on"), so the check asks once more for each
	// stage, and every call is measured. The prefix request and its retry
	// come first; a split fold's later stages are the worker's.
	stages := 1
	if split {
		stages = 2
	}
	if len(folds.msgs) != 2*stages {
		t.Fatalf("%s: %d fold calls, want %d (each stage and the check's retry)", name, len(folds.msgs), 2*stages)
	}
	for i := 2; i < len(folds.msgs); i++ {
		if m := folds.msgs[i]; m[0].Content != agent.DefaultSummarySystem || len(folds.tools[i]) != 0 || folds.keys[i] != "" {
			t.Errorf("%s: stage request %d: system %.40q, %d tools, key %q; want the worker's, none, none",
				name, i, m[0].Content, len(folds.tools[i]), folds.keys[i])
		}
	}
	if got := h.SideCalls()["fold"]; got == nil || got["calls"].(int) != len(folds.msgs) {
		t.Errorf("%s: fold side calls = %v, want %d", name, got, len(folds.msgs))
	}
}

func largeTail(i int) int {
	if i >= 12 {
		return 900
	}
	return 10
}
