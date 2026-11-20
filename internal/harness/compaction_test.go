package harness

// Compaction is the one operation that destroys conversation. Two sessions in
// this workspace lost 90% and 84% of their entries to it in July 2026 —
// silently, because nothing reported it. These tests cover both halves of that
// failure: the trigger, and the invisibility.

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/agent"
)

// windowSayer is a server that states a context window; noWindow is one that
// does not (the OpenAI/Anthropic case).
type windowSayer struct{ n int }

func (w windowSayer) ContextWindow(context.Context) (int, bool) { return w.n, w.n > 0 }

type noWindow struct{}

// With no environment variable AND no server answer, the budget must still be
// 0. A 0 that anything downstream treats as "a budget of zero tokens" makes
// every turn over budget — two real sessions folded 45 times in 29 minutes and
// 38 in 7 before that was fixed in the Shaper.
func TestContextBudget_NothingKnownMeansUnbudgeted(t *testing.T) {
	t.Setenv("DUN_CONTEXT_TOKENS", "")
	if got := shapingBudget((&Harness{}).contextWindow(context.Background(), noWindow{})); got != 0 {
		t.Fatalf("no env and no server should be 0 (unbudgeted), got %d", got)
	}
	if got := shapingBudget((&Harness{}).contextWindow(context.Background(), windowSayer{0})); got != 0 {
		t.Errorf("a server stating no window should be 0, got %d", got)
	}
	t.Setenv("DUN_CONTEXT_TOKENS", "not-a-number")
	if got := shapingBudget((&Harness{}).contextWindow(context.Background(), noWindow{})); got != 0 {
		t.Errorf("garbage should fall back to unbudgeted, got %d", got)
	}
	_ = os.Unsetenv("DUN_CONTEXT_TOKENS")
}

// The server's number is used when the environment says nothing. This is the
// change: dun no longer makes somebody type a window the server will state.
func TestContextBudget_AsksTheServerWhenUnset(t *testing.T) {
	t.Setenv("DUN_CONTEXT_TOKENS", "")
	if got, want := shapingBudget((&Harness{}).contextWindow(context.Background(), windowSayer{220160})), 220160-outputReserve(); got != want {
		t.Errorf("server window should shape to window minus the response reserve: got %d, want %d", got, want)
	}
	_ = os.Unsetenv("DUN_CONTEXT_TOKENS")
}

// The prompt's budget and the response's cap are two halves of ONE window, and
// the bug they were born from is them being budgeted independently: 90%% to the
// prompt, an uncommunicated 10%% to the response, and a reasoning model that
// spent 60k tokens of the window nobody had reserved. They must still add up.
func TestContextBudget_LeavesRoomForTheResponse(t *testing.T) {
	t.Setenv("DUN_CONTEXT_TOKENS", "")
	const window = 188160
	budget := shapingBudget((&Harness{}).contextWindow(context.Background(), windowSayer{window}))
	gen, ok := generationBudget(window, budget)
	if !ok {
		t.Fatalf("a prompt shaped to its own budget must still have room to answer; got %d", gen)
	}
	if budget+gen > window {
		t.Errorf("prompt budget %d + response cap %d = %d, over the %d window",
			budget, gen, budget+gen, window)
	}
	if gen != maxOutputTokens() {
		t.Errorf("a prompt at budget should get the full response cap: got %d, want %d",
			gen, maxOutputTokens())
	}
	_ = os.Unsetenv("DUN_CONTEXT_TOKENS")
}

// A prompt that has eaten the window gets no budget at all rather than a tiny
// one. Asking for a reply in 500 tokens reproduces the exact failure this
// reserve exists to prevent — a tool call cut off mid-arguments.
func TestGenerationBudget_RefusesBelowTheFloor(t *testing.T) {
	if _, ok := generationBudget(188160, 188160-outputMargin-minOutputTokens+1); ok {
		t.Error("below the floor must not report a usable budget")
	}
	if _, ok := generationBudget(0, 1000); ok {
		t.Error("an unknown window has no budget to give")
	}
	// Room to spare is capped by the hard limit, not handed out whole.
	if got, _ := generationBudget(1_000_000, 1000); got != maxOutputTokens() {
		t.Errorf("a huge window still caps one response: got %d, want %d", got, maxOutputTokens())
	}
}

// A per-model quality ceiling bounds the shape target: the prompt is shaped
// to min(window - the response's reserve, Q), so a big-window model is shaped
// against Q rather than running unshaped until it is near the window. Elision
// before fold is the Shaper's own ladder order (LOD/Rehead run before the
// fold), so the ceiling is the budget number, not a reordering.
func TestContextBudget_CeilingBoundsTheTarget(t *testing.T) {
	t.Setenv("DUN_CONTEXT_TOKENS", "")
	applyContextConfig(ContextConfig{Ceiling: 96000})
	t.Cleanup(func() { applyContextConfig(ContextConfig{}) })
	const window = 1_000_000
	if got, want := shapingBudget(window), 96000; got != want {
		t.Errorf("a 1M window with a 96k ceiling should shape to the ceiling: got %d, want %d", got, want)
	}
	// The ceiling only LOWERS the target: a window whose reserve already
	// leaves less is shaped as before.
	if got, want := shapingBudget(100000), 100000-outputReserve(); got != want {
		t.Errorf("a small window keeps window minus reserve: got %d, want %d", got, want)
	}
	// The target is the MINIMUM, so the ceiling can never exceed the window's
	// share.
	applyContextConfig(ContextConfig{Ceiling: 200_000})
	if got, want := shapingBudget(188160), 188160-outputReserve(); got != want {
		t.Errorf("a ceiling above the window's share must not raise it: got %d, want %d", got, want)
	}
	_ = os.Unsetenv("DUN_CONTEXT_TOKENS")
}

// The ceiling set through /context config is the one shapingBudget reads:
// configure, shape, and the budget moved to it.
func TestContextBudget_CeilingFromConfig(t *testing.T) {
	h := NewTestHarness(Endpoint{URL: "https://litellm.corp/v1", Model: "ceiling-test"})
	t.Setenv("DUN_HOME", t.TempDir())
	if _, err := h.ConfigureContext(context.Background(), map[string]int{"ceiling": 64 * 1000}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { applyContextConfig(ContextConfig{}) })
	if got, want := ceilingTokens(), 64*1000; got != want {
		t.Fatalf("the ceiling in force: got %d, want %d", got, want)
	}
	if got, want := shapingBudget(1_000_000), 64*1000; got != want {
		t.Errorf("shaping against the configured ceiling: got %d, want %d", got, want)
	}
	// Status says what is in force, and "default" clears it.
	if !strings.Contains(h.ContextStatus(), "ceiling 64,000") {
		t.Errorf("status lacks the ceiling: %s", h.ContextStatus())
	}
	if _, err := h.ConfigureContext(context.Background(), map[string]int{"ceiling": 0}); err != nil {
		t.Fatal(err)
	}
	if ceilingTokens() != 0 {
		t.Errorf("default did not clear the ceiling")
	}
}

// An explicit setting WINS over the server, including when it is smaller. The
// server states what the model can hold; a person setting this is expressing an
// intent about what to spend, and the second must not be overridden by the first.
func TestContextBudget_EnvironmentWinsOverTheServer(t *testing.T) {
	t.Setenv("DUN_CONTEXT_TOKENS", "100000")
	if got, want := shapingBudget((&Harness{}).contextWindow(context.Background(), windowSayer{220160})), 100000-outputReserve(); got != want {
		t.Errorf("an explicit window should shape to it minus the response reserve: got %d, want %d", got, want)
	}
	// Garbage in the environment falls THROUGH to the server rather than
	// disabling shaping: a typo should not silently cost the session its budget.
	t.Setenv("DUN_CONTEXT_TOKENS", "not-a-number")
	if got, want := shapingBudget((&Harness{}).contextWindow(context.Background(), windowSayer{220160})), 220160-outputReserve(); got != want {
		t.Errorf("invalid env should fall through to the server: got %d, want %d", got, want)
	}
	_ = os.Unsetenv("DUN_CONTEXT_TOKENS")
}

// The working budget must stay strictly below the window's hard limit, and
// WindowBudget.PromptBudget must be the same number the Shaper shapes to, so
// the two budget values cannot diverge. The 65k fold that failed outright was
// a prompt allowed to reach the window.
func TestContextBudget_BelowTheWindowHardLimit(t *testing.T) {
	t.Setenv("DUN_CONTEXT_TOKENS", "")
	applyContextConfig(ContextConfig{Ceiling: 96000})
	t.Cleanup(func() { applyContextConfig(ContextConfig{}) })

	// Across every window size, including the small-window fallback branch
	// (window < the response's reserve) where the budget is half the window.
	for _, window := range []int{4096, 10000, 32768, 65536, 188160, 220160, 1_000_000} {
		if got := shapingBudget(window); got >= window {
			t.Errorf("window %d: shaping budget %d must stay below the window's hard limit", window, got)
		}
	}

	// A real harness: the status view and the shaper's policy come from the
	// same call, so what /context reports is what the Shaper shapes to, and
	// it is below the window.
	h := NewTestHarness(Endpoint{URL: "https://litellm.corp/v1", Model: "hard-limit"})
	h.window.Store(220160)
	wb := h.Window()
	if wb.PromptBudget != shapingBudget(220160) {
		t.Errorf("WindowBudget.PromptBudget %d is not shapingBudget(220160) = %d — the two diverged",
			wb.PromptBudget, shapingBudget(220160))
	}
	if wb.PromptBudget >= wb.Window {
		t.Errorf("PromptBudget %d is not below the %d window", wb.PromptBudget, wb.Window)
	}
	_ = os.Unsetenv("DUN_CONTEXT_TOKENS")
}

// A fold has to be reviewable after the fact: how much was destroyed, what it
// cost, and whether it is happening more than once a turn.
func TestCompactionNote_ReportsTheDamage(t *testing.T) {
	n := CompactionNote{Subsumed: 12, TokensBefore: 30000, TokensAfter: 8000}
	s := n.String()
	for _, want := range []string{"12 entries", "30000", "8000", "saved 22000"} {
		if !strings.Contains(s, want) {
			t.Errorf("note is missing %q: %s", want, s)
		}
	}
	if strings.Contains(s, "this turn") {
		t.Errorf("a single fold is not thrashing: %s", s)
	}
	// The second fold in ONE turn is the signal that the budget is wrong.
	n.Turn, n.SinceLastSecs = 2, 7
	s = n.String()
	if !strings.Contains(s, "2× this turn") || !strings.Contains(s, "7s since") {
		t.Errorf("thrash markers missing: %s", s)
	}
}

// The harness must hand every fold to the UI, with the per-turn count attached.
func TestNoteCompaction_FiresTheHook(t *testing.T) {
	var got []CompactionNote
	h := newNoteHarness(t)
	h.cfg.OnCompaction = func(n CompactionNote) { got = append(got, n) }

	h.noteCompaction(agentCompaction(5, 100, 40))
	h.noteCompaction(agentCompaction(3, 90, 30))
	if len(got) != 2 {
		t.Fatalf("want 2 notes, got %d", len(got))
	}
	if got[0].Turn != 1 || got[1].Turn != 2 {
		t.Errorf("per-turn counter not attached: %d then %d", got[0].Turn, got[1].Turn)
	}
	if got[0].Subsumed != 5 || got[1].TokensAfter != 30 {
		t.Errorf("numbers not carried through: %+v", got)
	}
}

func agentCompaction(subsumed, before, after int) agent.CompactionInfo {
	return agent.CompactionInfo{SubsumedCount: subsumed, TokensBefore: before, TokensAfter: after, Summary: "s"}
}

// dun keeps its per-session git worktrees in .dun — copies of the workspace.
// Handing raglit the workspace root indexed all of them: 16 in this repo, and
// every workspace-wide search came back stale duplicates with the live files
// nowhere in the results. The agent's own isolation poisoning its own index.
func TestIngestTargets_ExcludesDunState(t *testing.T) {
	ws := t.TempDir()
	for _, f := range []string{".git/HEAD", ".dun/worktrees/s1/README.md", "cmd/main.go", "plan/plan.md", "README.md"} {
		p := filepath.Join(ws, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("hi"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := ingestTargets(ws)
	var names []string
	for _, p := range got {
		rel, _ := filepath.Rel(ws, p)
		names = append(names, filepath.ToSlash(rel))
	}
	for _, n := range names {
		if strings.HasPrefix(n, ".git/") || strings.HasPrefix(n, ".dun/worktrees/") {
			t.Errorf("%s must not be handed to the indexer: %v", n, names)
		}
	}
	for _, want := range []string{"cmd/main.go", "plan/plan.md", "README.md"} {
		if !slices.Contains(names, want) {
			t.Errorf("real content %q was excluded: %v", want, names)
		}
	}
}

// An unreadable workspace falls back rather than silently indexing nothing.
func TestIngestTargets_FallsBack(t *testing.T) {
	if got := ingestTargets(filepath.Join(t.TempDir(), "nope")); len(got) != 1 {
		t.Errorf("want the workspace itself as a fallback, got %v", got)
	}
}
