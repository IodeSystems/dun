package harness

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/iodesystems/agentkit/llm"
)

// countRunner answers every request with reply after a pause, counting.
type countRunner struct {
	mu    sync.Mutex
	n     int
	reply string
}

func (r *countRunner) ChatStream(_ context.Context, _ []llm.Message, _ []llm.ToolDef, _ *llm.ChatOpts) (<-chan llm.StreamChunk, error) {
	r.mu.Lock()
	r.n++
	r.mu.Unlock()
	ch := make(chan llm.StreamChunk, 2)
	go func() {
		time.Sleep(30 * time.Millisecond)
		ch <- llm.StreamChunk{Content: r.reply}
		ch <- llm.StreamChunk{Done: true}
		close(ch)
	}()
	return ch, nil
}

func (r *countRunner) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.n
}

// Two clients asking in the same idle get one call between them; a new
// idle makes another; after a turn the person did not start, none until
// the person is seen.
func TestIdleSuggestions_OncePerIdleAcrossClients(t *testing.T) {
	h, _ := idleHarness(t, "", nil, false)
	r := &countRunner{reply: `{` + sugsOnly + `}`}
	h.Session.Runner = r
	h.turnBegin(originPerson)
	var wg sync.WaitGroup
	got := make([][]Suggestion, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i], _ = h.IdleSuggestions(context.Background())
		}()
	}
	wg.Wait()
	if r.count() != 1 || len(got[0]) != 1 || len(got[1]) != 1 {
		t.Fatalf("%d calls for two clients; answers %v", r.count(), got)
	}
	h.turnBegin(originPerson)
	if _, err := h.IdleSuggestions(context.Background()); err != nil || r.count() != 2 {
		t.Fatalf("a new idle: %d calls, %v", r.count(), err)
	}
	h.turnBegin(originAuto)
	if s, _ := h.IdleSuggestions(context.Background()); s != nil || r.count() != 2 {
		t.Fatalf("after an automatic turn, nobody seen: %d calls, %v", r.count(), s)
	}
	h.SettlePresence()
	if s, _ := h.IdleSuggestions(context.Background()); len(s) != 1 || r.count() != 3 {
		t.Fatalf("after presence: %d calls, %v", r.count(), s)
	}
}
