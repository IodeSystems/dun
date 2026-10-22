package dun

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/iodesystems/agentkit/llm"
)

// thinkRunner streams a fixed round: some reasoning, then the reply, then
// the done chunk. It counts calls so a test can tell which runner was used.
type thinkRunner struct {
	mu     sync.Mutex
	calls  int
	chunks []llm.StreamChunk
}

func (r *thinkRunner) ChatStream(context.Context, []llm.Message, []llm.ToolDef, *llm.ChatOpts) (<-chan llm.StreamChunk, error) {
	r.mu.Lock()
	r.calls++
	r.mu.Unlock()
	ch := make(chan llm.StreamChunk, len(r.chunks))
	for _, c := range r.chunks {
		ch <- c
	}
	close(ch)
	return ch, nil
}

func thinkingRound() []llm.StreamChunk {
	return []llm.StreamChunk{
		{Reasoning: "the user "},
		{Reasoning: "wants a greeting"},
		{Content: "hello"},
		{Done: true, Usage: &llm.Usage{PromptTokens: 5, CompletionTokens: 3}},
	}
}

// The tee reports every reasoning delta, in order, and forwards every chunk
// exactly as it came — a consumer behind it sees what it saw before.
func TestReasoningTee_ReportsDeltasForwardsChunks(t *testing.T) {
	inner := &thinkRunner{chunks: thinkingRound()}
	var got []string
	r := teeReasoning(inner, func(s string) { got = append(got, s) })
	ch, err := r.ChatStream(context.Background(), nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var out []llm.StreamChunk
	for c := range ch {
		out = append(out, c)
	}
	if !reflect.DeepEqual(out, inner.chunks) {
		t.Errorf("chunks changed in transit:\n got %+v\nwant %+v", out, inner.chunks)
	}
	if want := []string{"the user ", "wants a greeting"}; !reflect.DeepEqual(got, want) {
		t.Errorf("deltas %q, want %q", got, want)
	}
	if untee(r) != inner {
		t.Error("untee does not give back the runner underneath")
	}
	if teeReasoning(inner, nil) != inner {
		t.Error("no callback should mean no tee")
	}
}

// A reader that walks away (its context cancelled) does not strand the tee's
// goroutine or the producer under it: the output channel still closes.
func TestReasoningTee_CancelledReaderDrains(t *testing.T) {
	src := make(chan llm.StreamChunk)
	ctx, cancel := context.WithCancel(context.Background())
	r := teeReasoning(chanRunner(src), func(string) {})
	out, _ := r.ChatStream(ctx, nil, nil, nil)
	src <- llm.StreamChunk{Reasoning: "a"} // taken by the tee, now blocked on out
	cancel()
	src <- llm.StreamChunk{Content: "b"} // must still be drained
	close(src)
	for range out {
	}
}

type chanRunner chan llm.StreamChunk

func (c chanRunner) ChatStream(context.Context, []llm.Message, []llm.ToolDef, *llm.ChatOpts) (<-chan llm.StreamChunk, error) {
	return c, nil
}

// End to end through a harness: a turn's reasoning reaches OnReasoning live
// and comes back in history as a "reasoning" item ahead of the reply it
// belongs to. A side call on the same session (rephrase) does not report.
func TestHarness_StreamsAndReplaysReasoning(t *testing.T) {
	dir := t.TempDir()
	runner := &thinkRunner{chunks: thinkingRound()}
	var mu sync.Mutex
	var live strings.Builder
	h, err := Start(context.Background(), Config{
		Workspace:   dir,
		Servers:     []Server{},
		Client:      runner,
		SessionFile: filepath.Join(dir, "s.jsonl"),
		OnReasoning: func(s string) { mu.Lock(); live.WriteString(s); mu.Unlock() },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if _, err := h.Ask(context.Background(), "say hi"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	got := live.String()
	mu.Unlock()
	if got != "the user wants a greeting" {
		t.Errorf("live reasoning %q", got)
	}

	var kinds []string
	for _, it := range h.History() {
		kinds = append(kinds, it.Kind)
		if it.Kind == "reasoning" && it.Content != "the user wants a greeting" {
			t.Errorf("history reasoning %q", it.Content)
		}
	}
	if want := []string{"user", "reasoning", "assistant"}; !reflect.DeepEqual(kinds, want) {
		t.Errorf("history kinds %v, want %v", kinds, want)
	}

	mu.Lock()
	live.Reset()
	mu.Unlock()
	if _, err := h.Rephrase(context.Background(), "please do the thing for me now"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if live.Len() != 0 {
		t.Errorf("a side call's reasoning reached the turn's stream: %q", live.String())
	}
}
