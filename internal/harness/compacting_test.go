package harness

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/dun/internal/mcpserver"
)

// failRunner refuses every call: a fold's summary call that fails.
type failRunner struct{}

func (failRunner) ChatStream(context.Context, []llm.Message, []llm.ToolDef, *llm.ChatOpts) (<-chan llm.StreamChunk, error) {
	return nil, errors.New("endpoint down")
}

// foldEvents records what a UI is told about folds, in order: "start",
// "end:<error>" and "compaction".
func foldEvents(h *Harness) *[]string {
	var got []string
	h.cfg.OnCompacting = func(n CompactingNote) {
		if !n.Done {
			got = append(got, "start")
			return
		}
		got = append(got, "end:"+n.Err)
	}
	h.cfg.OnCompaction = func(CompactionNote) { got = append(got, "compaction") }
	return &got
}

func wantEvents(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("events %q, want %q", got, want)
	}
}

// wantFailed: the start, then an end that says why.
func wantFailed(t *testing.T, got []string) {
	t.Helper()
	if len(got) != 2 || got[0] != "start" || !strings.HasPrefix(got[1], "end:") || got[1] == "end:" {
		t.Fatalf("events %q, want start then an end with the error", got)
	}
}

// A Shaper fold that applies: the start, then its compaction, and no end.
func TestCompacting_ShaperFoldStartsThenCompacts(t *testing.T) {
	h := newNoteHarness(t)
	h.window.Store(200_000)
	h.shaper = boundaryShaper(t, h, 200_000)
	h.shaper.Pause = h.foldPause
	seedHistory(t, h, 8)
	got := foldEvents(h)

	if _, did := h.boundaryFold(context.Background()); !did {
		t.Fatal("no fold")
	}
	wantEvents(t, *got, "start", "compaction")
}

// A Shaper fold whose summary call fails: the start, then an end, so a UI's
// "compacting" does not run forever.
func TestCompacting_ShaperFoldFailureEnds(t *testing.T) {
	h := newNoteHarness(t)
	h.window.Store(200_000)
	h.shaper = boundaryShaper(t, h, 200_000)
	h.shaper.Runner = failRunner{}
	h.shaper.Pause = h.foldPause
	seedHistory(t, h, 8)
	got := foldEvents(h)

	if _, did := h.boundaryFold(context.Background()); did {
		t.Fatal("a failed fold reported as done")
	}
	wantFailed(t, *got)
}

// The rescue and overflow folds (foldHistory): the same pair, on success
// and on failure.
func TestCompacting_FoldHistory(t *testing.T) {
	h := newNoteHarness(t)
	h.client = &summaryRunner{t: t}
	seedHistory(t, h, 8)
	got := foldEvents(h)
	if _, err := h.foldHistory(context.Background(), foldByOverflow); err != nil {
		t.Fatal(err)
	}
	wantEvents(t, *got, "start", "compaction")

	h = newNoteHarness(t)
	h.client = failRunner{}
	seedHistory(t, h, 8)
	got = foldEvents(h)
	if _, err := h.foldHistory(context.Background(), foldByShaper); err == nil {
		t.Fatal("a failed summary call folded")
	}
	wantFailed(t, *got)
}

// Nothing to fold: nothing is said.
func TestCompacting_NothingToFoldSaysNothing(t *testing.T) {
	h := newNoteHarness(t)
	h.client = &summaryRunner{t: t}
	got := foldEvents(h)
	if n, err := h.foldHistory(context.Background(), foldByShaper); n != 0 || err != nil {
		t.Fatalf("n=%d err=%v", n, err)
	}
	wantEvents(t, *got)
}

// foldEnd with no fold open, and a second foldBegin, send nothing.
func TestCompacting_OnceEach(t *testing.T) {
	h := newNoteHarness(t)
	got := foldEvents(h)
	h.foldEnd(nil)
	h.foldBegin()
	h.foldBegin()
	h.foldEnd(errors.New("x"))
	h.foldEnd(nil)
	wantEvents(t, *got, "start", "end:x")
}

// modeRunner answers a fold's summary call by mode: "ok" a summary, "fail"
// an error, "cancel" cancels the call's context and returns its error.
type modeRunner struct {
	mu     sync.Mutex
	mode   string
	cancel context.CancelFunc
}

func (r *modeRunner) ChatStream(ctx context.Context, _ []llm.Message, _ []llm.ToolDef, _ *llm.ChatOpts) (<-chan llm.StreamChunk, error) {
	r.mu.Lock()
	mode, cancel := r.mode, r.cancel
	r.mu.Unlock()
	switch mode {
	case "fail":
		return nil, errors.New("endpoint down")
	case "cancel":
		cancel()
		<-ctx.Done()
		return nil, ctx.Err()
	}
	ch := make(chan llm.StreamChunk, 1)
	ch <- llm.StreamChunk{Content: "OPEN ASKS: none.\nSTATE: done.\nNEXT: nothing.", Done: true, Usage: &llm.Usage{}, FinishReason: "stop"}
	close(ch)
	return ch, nil
}

func (r *modeRunner) Capabilities() llm.Capabilities { return llm.Capabilities{} }

func (r *modeRunner) set(mode string, cancel context.CancelFunc) {
	r.mu.Lock()
	r.mode, r.cancel = mode, cancel
	r.mu.Unlock()
}

// startFolding is a Harness from Start (its own Shaper and Pause wiring)
// over a small window and a history that does not fit it.
func startFolding(t *testing.T, r *modeRunner) (*Harness, *[]string) {
	t.Helper()
	t.Setenv("DUN_HOME", t.TempDir())
	dir := t.TempDir()
	cfg := Config{Workspace: dir, Servers: []mcpserver.Server{}, Client: r, Exec: &HostShell{Dir: dir},
		SessionFile: filepath.Join(dir, "20261120-120000.jsonl"), PlanNudgeRounds: -1, PlanNudgeFails: -1, PlanStallRounds: -1}
	h, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	h.windowRunner = nil
	h.window.Store(16_000)
	for i := 0; i < 60; i++ {
		h.store.Append(context.Background(), "dun", agent.Entry{ID: "u" + itoa(i), Kind: agent.KindUser,
			Content: "ask " + itoa(i) + " " + strings.Repeat("words ", 120), CreatedAt: int64(100 + i*2)})
		h.store.Append(context.Background(), "dun", agent.Entry{ID: "a" + itoa(i), Kind: agent.KindAssistant,
			Content: "reply " + itoa(i) + " " + strings.Repeat("prose ", 120), CreatedAt: int64(101 + i*2)})
	}
	return h, foldEvents(h)
}

// Start wires the Shaper's Pause to foldPause: a boundary fold through the
// Shaper Start built says it began.
func TestCompacting_StartWiresThePause(t *testing.T) {
	h, got := startFolding(t, &modeRunner{mode: "ok"})
	if _, did := h.boundaryFold(context.Background()); !did {
		t.Fatal("no fold")
	}
	wantEvents(t, *got, "start", "compaction")
}

// measuredBuild outside a Turn (no completion sink): a fold the build ran
// ends with an end and no error; one whose summary call fails, or whose
// context is cancelled mid-summary, ends with the error.
func TestCompacting_MeasuredBuildEnds(t *testing.T) {
	r := &modeRunner{mode: "ok"}
	h, got := startFolding(t, r)
	if _, err := measuredBuild(h.shaper, h)(context.Background(), "dun", "system"); err != nil {
		t.Fatal(err)
	}
	if len(*got) == 0 || (*got)[0] != "start" || (*got)[len(*got)-1] != "end:" {
		t.Fatalf("events %q, want start … end with no error", *got)
	}

	h, got = startFolding(t, r)
	r.set("fail", nil)
	if _, err := measuredBuild(h.shaper, h)(context.Background(), "dun", "system"); err == nil {
		t.Fatal("a failed fold built")
	}
	wantFailed(t, *got)

	h, got = startFolding(t, r)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.set("cancel", cancel)
	if _, err := measuredBuild(h.shaper, h)(ctx, "dun", "system"); err == nil {
		t.Fatal("a cancelled fold built")
	}
	wantFailed(t, *got)
}

// A rescue fold cancelled during its summary call ends with the error.
func TestCompacting_FoldHistoryCancelled(t *testing.T) {
	h := newNoteHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.client = &modeRunner{mode: "cancel", cancel: cancel}
	seedHistory(t, h, 8)
	got := foldEvents(h)
	if _, err := h.foldHistory(ctx, foldByShaper); err == nil {
		t.Fatal("a cancelled fold folded")
	}
	wantFailed(t, *got)
}
