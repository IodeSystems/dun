package main

import (
	"bytes"
	"context"
	"testing"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/dun/internal/harness"
)

// entryStore is an in-memory agent.Store.
type entryStore struct{ entries []agent.Entry }

func (s *entryStore) ClaimPending(context.Context, string, int64) (int, error) { return 0, nil }
func (s *entryStore) Append(_ context.Context, _ string, e agent.Entry) error {
	s.entries = append(s.entries, e)
	return nil
}
func (s *entryStore) Context(context.Context, string) ([]agent.Entry, error)  { return s.entries, nil }
func (s *entryStore) Compact(context.Context, string, agent.Compaction) error { return nil }

// countRunner counts requests and answers with suggestions and a done part.
type countRunner struct{ calls int }

func (r *countRunner) ChatStream(context.Context, []llm.Message, []llm.ToolDef, *llm.ChatOpts) (<-chan llm.StreamChunk, error) {
	r.calls++
	ch := make(chan llm.StreamChunk, 2)
	ch <- llm.StreamChunk{Content: `{"suggestions":[{"text":"run the tests","prob":0.5}],"done":[{"title":"Wrote tests"}]}`}
	ch <- llm.StreamChunk{Done: true}
	close(ch)
	return ch, nil
}

// --no-suggest withholds the idle call, and with it the done nodes it would
// record (idle plans): no request, no event, nothing in the plan.
func TestNoSuggest_NoCallNoPlanNodes(t *testing.T) {
	old := suggestEnabled
	t.Cleanup(func() { suggestEnabled = old })
	h := harness.NewTestHarness(harness.Endpoint{URL: "http://127.0.0.1:1/v1", Model: "m"})
	st := &entryStore{entries: []agent.Entry{{ID: "u1", Kind: agent.KindUser, Content: "write tests"}}}
	r := &countRunner{}
	h.Session = &agent.Session{SessionID: "dun", Store: st, Runner: r,
		Build: func(ctx context.Context, sid, system string) ([]llm.Message, error) {
			return agent.DefaultContextBuilder(ctx, st, sid, system)
		}}
	var buf bytes.Buffer
	suggestEnabled = false
	emitSuggestions(context.Background(), h, &emitter{w: &buf})
	if r.calls != 0 || buf.Len() != 0 || h.Plan().Total != 0 {
		t.Fatalf("--no-suggest: %d calls, %q emitted, %d plan nodes", r.calls, buf.String(), h.Plan().Total)
	}
	suggestEnabled = true // the control: the same harness does make the call
	emitSuggestions(context.Background(), h, &emitter{w: &buf})
	if r.calls != 1 || buf.Len() == 0 {
		t.Fatalf("suggest on: %d calls, %q", r.calls, buf.String())
	}
}
