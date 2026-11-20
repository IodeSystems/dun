package harness

import (
	"context"
	"testing"
	"time"

	"github.com/iodesystems/agentkit/agent"
)

// boundaryShaper builds a minimal Shaper wired to the harness store with a
// summaryRunner so Compact's LLM call is answered. Mirrors the construction in
// Start (harness.go:898) minus the fields that need a live engine.
func boundaryShaper(t *testing.T, h *Harness, window int) *agent.Shaper {
	t.Helper()
	runner := &summaryRunner{t: t}
	h.client = runner
	return &agent.Shaper{
		Store:   h.store,
		Runner:  runner,
		Policy:  shaperPolicy(window, false),
		ToolLOD: toolLOD(),
	}
}

// A turn-end boundary fold fires when the prompt exceeds 0.7 × the shape
// target. The fold is recorded with the "boundary" cause and increments
// compactBoundary.
func TestTurnEndBoundaryFold_FiresAboveThreshold(t *testing.T) {
	h := newNoteHarness(t)
	window := 200_000
	target := shapingBudget(window)
	h.window.Store(int64(window))

	h.shaper = boundaryShaper(t, h, window)
	seedHistory(t, h, 8)

	// Prompt just above 0.7 × target.
	h.meter.NoteBuild(0, target*7/10+1)

	var note CompactionNote
	h.cfg.OnCompaction = func(n CompactionNote) { note = n }

	h.turnEndBoundaryFold(context.Background())

	h.compactMu.Lock()
	boundary := h.compactBoundary
	turn := h.compactTurn
	h.compactMu.Unlock()

	if boundary != 1 {
		t.Fatalf("compactBoundary=%d, want 1 (prompt=%d target=%d threshold=%d)",
			boundary, h.meter.LastPromptTokens(), target, target*7/10)
	}
	if turn != 1 {
		t.Errorf("compactTurn=%d, want 1", turn)
	}
	if note.Cause != "boundary" {
		t.Errorf("note.Cause=%q, want \"boundary\"", note.Cause)
	}
}

// Below 0.7 × target, no fold.
func TestTurnEndBoundaryFold_BelowThresholdNoFold(t *testing.T) {
	h := newNoteHarness(t)
	window := 200_000
	target := shapingBudget(window)
	h.window.Store(int64(window))

	h.shaper = boundaryShaper(t, h, window)
	seedHistory(t, h, 8)

	// Prompt at exactly 0.7 × target (not above).
	h.meter.NoteBuild(0, target * 7 / 10)

	h.turnEndBoundaryFold(context.Background())

	h.compactMu.Lock()
	boundary := h.compactBoundary
	h.compactMu.Unlock()

	if boundary != 0 {
		t.Errorf("compactBoundary=%d, want 0 (prompt=%d threshold=%d)",
			boundary, h.meter.LastPromptTokens(), target*7/10)
	}
}

// A nil shaper is a no-op: no panic, no fold.
func TestTurnEndBoundaryFold_NilShaper(t *testing.T) {
	h := newNoteHarness(t)
	h.window.Store(200_000)
	h.meter.NoteBuild(0, 100_000)

	// Must not panic.
	h.turnEndBoundaryFold(context.Background())

	h.compactMu.Lock()
	boundary := h.compactBoundary
	h.compactMu.Unlock()
	if boundary != 0 {
		t.Errorf("compactBoundary=%d, want 0 with nil shaper", boundary)
	}
}

// A zero window means no target, so no fold.
func TestTurnEndBoundaryFold_ZeroWindowNoFold(t *testing.T) {
	h := newNoteHarness(t)
	h.shaper = boundaryShaper(t, h, 0)
	seedHistory(t, h, 4)
	h.meter.NoteBuild(0, 100_000)

	h.turnEndBoundaryFold(context.Background())

	h.compactMu.Lock()
	boundary := h.compactBoundary
	h.compactMu.Unlock()
	if boundary != 0 {
		t.Errorf("compactBoundary=%d, want 0 with zero window", boundary)
	}
}

// A cold-cache fold fires when the gap since the last response exceeds the
// provider's cache TTL and the prompt is over 0.5 × the shape target.
func TestColdCacheFold_FiresAboveTTLAndThreshold(t *testing.T) {
	h := newNoteHarness(t)
	window := 200_000
	target := shapingBudget(window)
	h.window.Store(int64(window))

	h.shaper = boundaryShaper(t, h, window)
	seedHistory(t, h, 8)

	// Prompt just above 0.5 × target.
	h.meter.NoteBuild(0, target/2+1)

	// TTL 300s, gap 400s.
	ctxOverride.cacheTTL.Store(300)
	defer ctxOverride.cacheTTL.Store(0)
	h.cache = &cacheStats{lastResp: time.Now().Add(-400 * time.Second)}

	var note CompactionNote
	h.cfg.OnCompaction = func(n CompactionNote) { note = n }

	h.tryColdCacheFold(context.Background())

	h.compactMu.Lock()
	boundary := h.compactBoundary
	h.compactMu.Unlock()

	if boundary != 1 {
		t.Fatalf("compactBoundary=%d, want 1 (gap=400s TTL=300s prompt=%d target/2=%d)",
			boundary, h.meter.LastPromptTokens(), target/2)
	}
	if note.Cause != "boundary" {
		t.Errorf("note.Cause=%q, want \"boundary\"", note.Cause)
	}
}

// Gap below TTL: no fold.
func TestColdCacheFold_BelowTTLNoFold(t *testing.T) {
	h := newNoteHarness(t)
	window := 200_000
	target := shapingBudget(window)
	h.window.Store(int64(window))

	h.shaper = boundaryShaper(t, h, window)
	seedHistory(t, h, 8)
	h.meter.NoteBuild(0, target/2+1)

	ctxOverride.cacheTTL.Store(600)
	defer ctxOverride.cacheTTL.Store(0)
	// Gap 400s < TTL 600s.
	h.cache = &cacheStats{lastResp: time.Now().Add(-400 * time.Second)}

	h.tryColdCacheFold(context.Background())

	h.compactMu.Lock()
	boundary := h.compactBoundary
	h.compactMu.Unlock()
	if boundary != 0 {
		t.Errorf("compactBoundary=%d, want 0 (gap=400s TTL=600s)", boundary)
	}
}

// Prompt below 0.5 × target: no fold even with a large gap.
func TestColdCacheFold_BelowPromptThresholdNoFold(t *testing.T) {
	h := newNoteHarness(t)
	window := 200_000
	target := shapingBudget(window)
	h.window.Store(int64(window))

	h.shaper = boundaryShaper(t, h, window)
	seedHistory(t, h, 8)
	h.meter.NoteBuild(0, target/2) // at exactly 0.5, not above

	ctxOverride.cacheTTL.Store(300)
	defer ctxOverride.cacheTTL.Store(0)
	h.cache = &cacheStats{lastResp: time.Now().Add(-400 * time.Second)}

	h.tryColdCacheFold(context.Background())

	h.compactMu.Lock()
	boundary := h.compactBoundary
	h.compactMu.Unlock()
	if boundary != 0 {
		t.Errorf("compactBoundary=%d, want 0 (prompt=%d target/2=%d)",
			boundary, h.meter.LastPromptTokens(), target/2)
	}
}

// Zero TTL disables the cold-cache fold.
func TestColdCacheFold_ZeroTTLNoFold(t *testing.T) {
	h := newNoteHarness(t)
	window := 200_000
	h.window.Store(int64(window))

	h.shaper = boundaryShaper(t, h, window)
	seedHistory(t, h, 8)
	h.meter.NoteBuild(0, shapingBudget(window))

	ctxOverride.cacheTTL.Store(0)
	h.cache = &cacheStats{lastResp: time.Now().Add(-1 * time.Hour)}

	h.tryColdCacheFold(context.Background())

	h.compactMu.Lock()
	boundary := h.compactBoundary
	h.compactMu.Unlock()
	if boundary != 0 {
		t.Errorf("compactBoundary=%d, want 0 with zero TTL", boundary)
	}
}

// No previous response (lastResp is zero): no cold-cache fold.
func TestColdCacheFold_NoPreviousResponseNoFold(t *testing.T) {
	h := newNoteHarness(t)
	window := 200_000
	h.window.Store(int64(window))

	h.shaper = boundaryShaper(t, h, window)
	seedHistory(t, h, 8)
	h.meter.NoteBuild(0, shapingBudget(window))

	ctxOverride.cacheTTL.Store(300)
	defer ctxOverride.cacheTTL.Store(0)
	h.cache = &cacheStats{} // lastResp is zero

	h.tryColdCacheFold(context.Background())

	h.compactMu.Lock()
	boundary := h.compactBoundary
	h.compactMu.Unlock()
	if boundary != 0 {
		t.Errorf("compactBoundary=%d, want 0 with no previous response", boundary)
	}
}

// A nil cache is a no-op for the cold-cache fold.
func TestColdCacheFold_NilCacheNoFold(t *testing.T) {
	h := newNoteHarness(t)
	window := 200_000
	h.window.Store(int64(window))

	h.shaper = boundaryShaper(t, h, window)
	seedHistory(t, h, 8)
	h.meter.NoteBuild(0, shapingBudget(window))

	ctxOverride.cacheTTL.Store(300)
	defer ctxOverride.cacheTTL.Store(0)
	// h.cache is nil from newNoteHarness.

	h.tryColdCacheFold(context.Background())

	h.compactMu.Lock()
	boundary := h.compactBoundary
	h.compactMu.Unlock()
	if boundary != 0 {
		t.Errorf("compactBoundary=%d, want 0 with nil cache", boundary)
	}
}

// boundaryFold with a nil shaper returns no fold.
func TestBoundaryFold_NilShaper(t *testing.T) {
	h := newNoteHarness(t)
	_, did := h.boundaryFold(context.Background())
	if did {
		t.Errorf("boundaryFold with nil shaper should not report a fold")
	}
}

// boundaryFold records the fold with the "boundary" cause and does not
// increment the overflow counter.
func TestBoundaryFold_RecordsBoundaryCauseNotOverflow(t *testing.T) {
	h := newNoteHarness(t)
	window := 200_000
	h.window.Store(int64(window))

	h.shaper = boundaryShaper(t, h, window)
	seedHistory(t, h, 8)

	var note CompactionNote
	h.cfg.OnCompaction = func(n CompactionNote) { note = n }

	_, did := h.boundaryFold(context.Background())
	if !did {
		t.Fatal("boundaryFold did not report a fold")
	}

	if note.Cause != "boundary" {
		t.Errorf("note.Cause=%q, want \"boundary\"", note.Cause)
	}

	h.compactMu.Lock()
	overflow := h.compactOverflow
	boundary := h.compactBoundary
	turn := h.compactTurn
	h.compactMu.Unlock()

	if overflow != 0 {
		t.Errorf("compactOverflow=%d, want 0 — a boundary fold is not an overflow", overflow)
	}
	if boundary != 1 {
		t.Errorf("compactBoundary=%d, want 1", boundary)
	}
	if turn != 1 {
		t.Errorf("compactTurn=%d, want 1", turn)
	}
}
