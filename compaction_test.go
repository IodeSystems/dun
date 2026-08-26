package dun

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
	if got := shapingBudget(contextWindow(context.Background(), noWindow{})); got != 0 {
		t.Fatalf("no env and no server should be 0 (unbudgeted), got %d", got)
	}
	if got := shapingBudget(contextWindow(context.Background(), windowSayer{0})); got != 0 {
		t.Errorf("a server stating no window should be 0, got %d", got)
	}
	t.Setenv("DUN_CONTEXT_TOKENS", "not-a-number")
	if got := shapingBudget(contextWindow(context.Background(), noWindow{})); got != 0 {
		t.Errorf("garbage should fall back to unbudgeted, got %d", got)
	}
	_ = os.Unsetenv("DUN_CONTEXT_TOKENS")
}

// The server's number is used when the environment says nothing. This is the
// change: dun no longer makes somebody type a window the server will state.
func TestContextBudget_AsksTheServerWhenUnset(t *testing.T) {
	t.Setenv("DUN_CONTEXT_TOKENS", "")
	if got, want := shapingBudget(contextWindow(context.Background(), windowSayer{220160})), 220160-outputReserve(); got != want {
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
	budget := shapingBudget(contextWindow(context.Background(), windowSayer{window}))
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

// An explicit setting WINS over the server, including when it is smaller. The
// server states what the model can hold; a person setting this is expressing an
// intent about what to spend, and the second must not be overridden by the first.
func TestContextBudget_EnvironmentWinsOverTheServer(t *testing.T) {
	t.Setenv("DUN_CONTEXT_TOKENS", "100000")
	if got, want := shapingBudget(contextWindow(context.Background(), windowSayer{220160})), 100000-outputReserve(); got != want {
		t.Errorf("an explicit window should shape to it minus the response reserve: got %d, want %d", got, want)
	}
	// Garbage in the environment falls THROUGH to the server rather than
	// disabling shaping: a typo should not silently cost the session its budget.
	t.Setenv("DUN_CONTEXT_TOKENS", "not-a-number")
	if got, want := shapingBudget(contextWindow(context.Background(), windowSayer{220160})), 220160-outputReserve(); got != want {
		t.Errorf("invalid env should fall through to the server: got %d, want %d", got, want)
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
	for _, d := range []string{".git", ".dun", "cmd", "plan"} {
		if err := os.MkdirAll(filepath.Join(ws, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(ws, "README.md"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := ingestTargets(ws)
	var names []string
	for _, p := range got {
		names = append(names, filepath.Base(p))
	}
	for _, bad := range []string{".git", ".dun"} {
		if slices.Contains(names, bad) {
			t.Errorf("%s must not be handed to the indexer: %v", bad, names)
		}
	}
	for _, want := range []string{"cmd", "plan", "README.md"} {
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

// The fold destroys the prefix the AGENTS.md read lived in. The note must name
// the rules file so the model knows to re-read it, rather than depending on
// the summary (a model's reading) to have preserved the rules.
func TestNoteCompaction_NamesAgentsMDWhenPresent(t *testing.T) {
	var got []CompactionNote
	h := newNoteHarness(t)
	// A workspace with a root AGENTS.md and a nested one.
	ws := t.TempDir()
	os.MkdirAll(filepath.Join(ws, "src"), 0o755)
	os.WriteFile(filepath.Join(ws, "AGENTS.md"), []byte("root rules"), 0o644)
	os.WriteFile(filepath.Join(ws, "src", "AGENTS.md"), []byte("src rules"), 0o644)
	h.cfg.Workspace = ws
	h.cfg.OnCompaction = func(n CompactionNote) { got = append(got, n) }

	h.noteCompaction(agentCompaction(5, 100, 40))
	if len(got) != 1 {
		t.Fatalf("want 1 note, got %d", len(got))
	}
	s := got[0].Summary
	if !strings.Contains(s, "AGENTS.md") {
		t.Errorf("summary should name the rules file: %q", s)
	}
	if !strings.Contains(s, "re-read") {
		t.Errorf("summary should tell the model to re-read: %q", s)
	}
	// Both the root and the nested file should be named.
	if !strings.Contains(s, "AGENTS.md") || strings.Count(s, "AGENTS.md") < 2 {
		t.Errorf("both AGENTS.md files should be named: %q", s)
	}
}

// No AGENTS.md in the workspace: no reminder appended, summary unchanged.
func TestNoteCompaction_NoAgentsMDNoReminder(t *testing.T) {
	var got []CompactionNote
	h := newNoteHarness(t)
	h.cfg.Workspace = t.TempDir() // empty
	h.cfg.OnCompaction = func(n CompactionNote) { got = append(got, n) }

	h.noteCompaction(agentCompaction(5, 100, 40))
	if len(got) != 1 {
		t.Fatalf("want 1 note, got %d", len(got))
	}
	if !strings.EqualFold(got[0].Summary, "s") {
		t.Errorf("summary should be unchanged with no AGENTS.md: %q", got[0].Summary)
	}
}
