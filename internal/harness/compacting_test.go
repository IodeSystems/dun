package harness

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/llm"
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
