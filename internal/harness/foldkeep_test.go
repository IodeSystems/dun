package harness

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
)

// foldKeepRun is one turn through dun's own policy (shaperPolicy, with
// FoldKeepPct set to pct) over a session of n entries shaped like the one
// reported on 2026-11-18: one ask, then exec call / result / reply triples.
// mod changes the entries before the turn. It returns the fold note, the
// prompt the turn sent, and the target the fold aimed at.
type foldKeepResult struct {
	note   CompactionNote
	folds  int
	sent   []llm.Message
	target int
	newest string
	h      *Harness
}

func foldKeepRun(t *testing.T, n, budget, pct, maxResult int, mod func([]agent.Entry)) foldKeepResult {
	t.Helper()
	h := newNoteHarness(t)
	h.sideCalls = &sideCallStats{}
	var r foldKeepResult
	h.cfg.OnCompaction = func(c CompactionNote) { r.note = c; r.folds++ }
	turns := &reqRecorder{reply: "ok"}
	// A summary near agentkit's cap, as a real fold of this size writes.
	folds := &reqRecorder{reply: foldGoodSummary(repoAsk) + "\n" + strings.Repeat("s", 24_000)}
	pol := shaperPolicy(0, false)
	pol.BudgetTokens = budget
	pol.FoldKeepPct = pct
	pol.MaxToolResultChars = maxResult
	pol.PinLatestUser = true // as harness.go sets it
	sh := &agent.Shaper{Store: h.store, Runner: folds, Estimate: &h.meter, Policy: pol, ToolLOD: toolLOD()}
	h.foldWire(sh)
	defs := []llm.ToolDef{{Type: "function"}}
	defs[0].Function.Name = "exec"
	h.Session = &agent.Session{SessionID: "dun", Store: h.store, Runner: turns,
		System: strings.Repeat("y", 30_000), Tools: defs, Build: measuredBuild(sh, h),
		ChatOpts: &llm.ChatOpts{}, Estimate: &h.meter, OnCompactionComplete: h.noteCompaction}

	var entries []agent.Entry
	add := func(e agent.Entry) {
		e.CreatedAt = int64(len(entries) + 1)
		e.ID = fmt.Sprintf("e%04d", e.CreatedAt)
		entries = append(entries, e)
	}
	add(agent.Entry{Kind: agent.KindUser, Content: repoAsk})
	for i := 0; len(entries) < n; i++ {
		id := fmt.Sprintf("c%d", i)
		add(agent.Entry{Kind: agent.KindToolCall, ToolCallID: id, ToolName: "exec", Content: `{"command":"` + strings.Repeat("a", 590) + `"}`})
		add(agent.Entry{Kind: agent.KindToolResult, ToolCallID: id, ToolName: "exec", Content: fmt.Sprintf("r%d ", i) + strings.Repeat("r", 3990)})
		add(agent.Entry{Kind: agent.KindAssistant, Content: fmt.Sprintf("a%d ", i) + strings.Repeat("t", 1880)})
	}
	if mod != nil {
		mod(entries)
	}
	ctx := context.Background()
	for _, e := range entries {
		if err := h.store.Append(ctx, "dun", e); err != nil {
			t.Fatal(err)
		}
	}
	r.newest = entries[len(entries)-1].Content
	if _, err := h.Session.Turn(ctx); err != nil {
		t.Fatal(err)
	}
	if len(turns.msgs) == 0 {
		t.Fatal("no round sent")
	}
	r.sent, r.target, r.h = turns.msgs[0], budget-10_000, h // agentkit's default LOD headroom
	return r
}

func (r foldKeepResult) sentTokens() int {
	n := 0
	for _, m := range r.sent {
		n += r.h.meter.Estimate(m.Content) + r.h.meter.Estimate(m.Role) + r.h.meter.Estimate(m.Name)
		// Native tool calls count too, as agentkit's messageTokens counts
		// them (v0.39.0).
		for _, tc := range m.ToolCalls {
			n += r.h.meter.Estimate(tc.Function.Name) + r.h.meter.Estimate(tc.Function.Arguments)
		}
	}
	return n
}

func sentWhole(msgs []llm.Message, content string) bool {
	for _, m := range msgs {
		if strings.Contains(m.Content, content) {
			return true
		}
	}
	return false
}

// The reported case: a long session over target folds to 50-60% of it, keeps
// the newest entries verbatim, and says how many it kept. Before FoldKeepPct
// the same fold kept only the pristine tail and left the window nearly empty.
func TestFoldKeep_ReportedCaseLandsInTheBand(t *testing.T) {
	const budget = 200_000
	r := foldKeepRun(t, 434, budget, foldKeepPct, agent.DefaultMaxToolResultChars, nil)
	if r.folds != 1 {
		t.Fatalf("%d folds, want 1", r.folds)
	}
	after := r.sentTokens()
	t.Logf("target %d; %s; sent %d (%d%% of target)", r.target, r.note, after, after*100/r.target)
	// The fold decision counts the system prompt; CompactionInfo.TokensBefore
	// leaves it out (agentkit v0.39.0), so add it back to compare with target.
	if before := r.note.TokensBefore + r.h.meter.Estimate(r.h.Session.System); before <= r.target {
		t.Fatalf("before %d (history and system prompt) is not over target %d", before, r.target)
	}
	if after < r.target*50/100 || after > r.target*60/100 {
		t.Errorf("sent %d tokens, %d%% of target %d; want 50-60%%", after, after*100/r.target, r.target)
	}
	if !sentWhole(r.sent, r.newest) {
		t.Error("the newest entry was not sent verbatim")
	}
	base := foldKeepRun(t, 434, budget, 0, agent.DefaultMaxToolResultChars, nil)
	if r.note.Kept <= base.note.Kept {
		t.Errorf("kept %d entries, the pristine tail alone keeps %d", r.note.Kept, base.note.Kept)
	}
	if s := r.note.String(); !strings.Contains(s, fmt.Sprintf("newest %d kept as is", r.note.Kept)) {
		t.Errorf("the note does not say what it kept: %s", s)
	}
}

// Little history: the band holds no more than the pristine tail, so the fold
// is the one dun made before FoldKeepPct.
func TestFoldKeep_LittleHistoryFoldsAsBefore(t *testing.T) {
	a := foldKeepRun(t, 40, 30_000, 0, agent.DefaultMaxToolResultChars, nil)
	b := foldKeepRun(t, 40, 30_000, foldKeepPct, agent.DefaultMaxToolResultChars, nil)
	if a.folds != 1 || b.folds != 1 {
		t.Fatalf("folds: %d without, %d with", a.folds, b.folds)
	}
	if a.note.Subsumed != b.note.Subsumed || a.note.Kept != b.note.Kept {
		t.Errorf("without: %s\nwith:    %s", a.note, b.note)
	}
}

// A newest result bigger than the band by itself: the fold falls back to the
// pristine tail rather than overshoot.
func TestFoldKeep_HugeNewestResultFallsBack(t *testing.T) {
	huge := func(es []agent.Entry) {
		for i := len(es) - 1; i >= 0; i-- {
			if es[i].Kind == agent.KindToolResult {
				es[i].Content = strings.Repeat("h", 4*130_000)
				return
			}
		}
	}
	a := foldKeepRun(t, 434, 200_000, 0, 0, huge)
	b := foldKeepRun(t, 434, 200_000, foldKeepPct, 0, huge)
	if a.folds != 1 || b.folds != 1 {
		t.Fatalf("folds: %d without, %d with", a.folds, b.folds)
	}
	if b.note.Kept != a.note.Kept {
		t.Errorf("kept %d entries, the pristine tail is %d", b.note.Kept, a.note.Kept)
	}
}

// The notice names what was summarized and kept, and the arrow ends at what is sent.
func TestCompactionNote_SaysWhatItKept(t *testing.T) {
	n := CompactionNote{Subsumed: 237, Kept: 198, KeptTokens: 129_700, TokensBefore: 294971, TokensAfter: 147755, Cause: "shaper"}
	want := "compacted: oldest 237 entries → summary, newest 198 kept as is · history 295.0k → 147.8k tokens · over budget"
	if s := n.String(); s != want {
		t.Errorf("got  %s\nwant %s", s, want)
	}
}
