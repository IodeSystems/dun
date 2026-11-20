package harness

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
)

// slowFolds answers every fold call after a pause and records how many were
// in flight at once. entered is signalled on the first call.
type slowFolds struct {
	inFlight, most atomic.Int32
	calls          atomic.Int32
	entered        chan struct{}
	once           sync.Once
}

func (r *slowFolds) ChatStream(ctx context.Context, _ []llm.Message, _ []llm.ToolDef, _ *llm.ChatOpts) (<-chan llm.StreamChunk, error) {
	r.calls.Add(1)
	n := r.inFlight.Add(1)
	for {
		m := r.most.Load()
		if n <= m || r.most.CompareAndSwap(m, n) {
			break
		}
	}
	r.once.Do(func() { close(r.entered) })
	time.Sleep(200 * time.Millisecond)
	r.inFlight.Add(-1)
	return replay(foldGoodSummary(repoAsk), ""), nil
}

// A turn-end boundary fold runs after the turn, outside turnMu; the idle
// suggestion call builds (and the Shaper may fold) outside turnMu too. Both
// over a history past target: the second fold waits for the first to
// commit, so no two summary calls are in flight at once.
func TestFoldMu_BoundaryFoldAndBuildDoNotInterleave(t *testing.T) {
	h := newNoteHarness(t)
	const window = 40_000
	h.window.Store(window)
	runner := &slowFolds{entered: make(chan struct{})}
	sh := &agent.Shaper{Store: h.store, Runner: runner, Estimate: &h.meter,
		Policy: shaperPolicy(window, false), ToolLOD: toolLOD()}
	h.shaper = sh
	h.Session = &agent.Session{SessionID: "dun", Store: h.store, ChatOpts: &llm.ChatOpts{}, Estimate: &h.meter}
	build := measuredBuild(sh, h)

	ctx := context.Background()
	if err := h.store.Append(ctx, "dun", agent.Entry{ID: "ask", Kind: agent.KindUser, Content: repoAsk, CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 120; i++ {
		e := agent.Entry{ID: "a" + itoa(i), Kind: agent.KindAssistant,
			Content: "step " + itoa(i) + " " + strings.Repeat("t", 2000), CreatedAt: int64(2 + i)}
		if err := h.store.Append(ctx, "dun", e); err != nil {
			t.Fatal(err)
		}
	}
	h.meter.NoteBuild(0, shapingBudget(window)) // over 0.7 × target: the turn-end fold fires

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); h.turnEndBoundaryFold(ctx) }()
	<-runner.entered // the boundary fold is in its summary call
	go func() {
		defer wg.Done()
		if _, err := build(ctx, "dun", "system"); err != nil {
			t.Errorf("build: %v", err)
		}
	}()
	wg.Wait()

	if runner.calls.Load() < 1 {
		t.Fatal("no fold ran")
	}
	if m := runner.most.Load(); m > 1 {
		t.Errorf("%d fold summary calls in flight at once, want 1", m)
	}
	// One marker: the second fold, if any, subsumed the first's.
	entries, _ := h.store.Context(ctx, "dun")
	markers := 0
	for _, e := range entries {
		if e.Kind == agent.KindCompaction {
			markers++
		}
	}
	if markers != 1 {
		t.Errorf("%d compaction markers, want 1", markers)
	}
}
