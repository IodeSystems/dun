package harness

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/agentkit/taskgraph"
)

// A fold is told the session's plan and the work in flight, from dun's own
// records: the conversation being folded is what loses them.
func TestFoldTask_PlanAndInFlight(t *testing.T) {
	h := startParentOf(t, &scriptRunner{rounds: []roundFn{textRound("done"), textRound("done")}})
	if got := h.foldTask(); got != "" {
		t.Errorf("nothing set and nothing running: %q", got)
	}
	planCall(t, h, "plan_add", `{"title":"wire rescue","done_when":"the rescue folds in stages"}`)
	kept := spawnKid(t, h, "read the build log and say what failed")
	gone := spawnKid(t, h, "an aside that was dismissed")
	waitIdle(t, kept)
	waitIdle(t, gone)
	gone.quit()

	task := h.foldTask()
	for _, want := range []string{"# Plan", "wire rescue", "# In flight",
		"read the build log and say what failed"} {
		if !strings.Contains(task, want) {
			t.Errorf("fold task lacks %q:\n%s", want, task)
		}
	}
	if strings.Contains(task, "an aside that was dismissed") {
		t.Errorf("a dismissed child is not in flight:\n%s", task)
	}
}

// The rescue fold thinks nothing, is sized to what it folds, is capped to
// match, and keeps what is in flight and what was found.
func TestFoldHistory_SizedAndThinkingOff(t *testing.T) {
	h := newNoteHarness(t)
	r := &summaryRunner{t: t}
	h.client = r
	seedHistory(t, h, 8)
	if _, err := h.foldHistory(context.Background(), foldByOverflow); err != nil {
		t.Fatal(err)
	}
	o := r.lastOpts
	if o == nil || o.Effort != llm.EffortNone {
		t.Fatalf("the rescue fold must send thinking off: %+v", o)
	}
	if o.MaxTokens < agent.SummaryCap(agent.SummaryMinTokens) {
		t.Errorf("MaxTokens %d: no cap from the budget", o.MaxTokens)
	}
	for _, want := range []string{"IN FLIGHT", "DISCOVERIES", "ruled out"} {
		if !strings.Contains(r.lastUser, want) {
			t.Errorf("the handoff prompt lacks %q", want)
		}
	}
	if strings.Contains(r.lastUser, "<= 800 tokens") {
		t.Error("the handoff is still a fixed 800 tokens")
	}
}

func TestFoldOpts_ThinkingOff(t *testing.T) {
	h := &Harness{}
	o := h.foldOpts()
	if o.Effort != llm.EffortNone || o.ReasoningBudgetTokens != nil {
		t.Errorf("fold options %+v", o)
	}
}

// Right after a fold the model is told, from dun's own records, what is still
// alive: each job and child with its id, start time, time running, and the
// command or the whole task (not inFlight's one-line excerpt). The summary is
// not trusted to have kept any of it.
func TestFoldRestoreNote_ListsLiveWork(t *testing.T) {
	h := startParentOf(t, &scriptRunner{rounds: []roundFn{textRound("done"), textRound("done")}})
	h.notes()
	h.noteCompaction(agentCompaction(5, 100, 40))
	if n := h.notes(); len(n) != 0 {
		t.Fatalf("nothing in flight, nothing to say: %q", n)
	}

	running := testJob(h, 1, "make test-integration")
	finished := testJob(h, 2, "make lint")
	finished.finish(ExecResult{})
	h.bgMu.Lock()
	h.bgJobs = map[int]*bgJob{1: running, 2: finished}
	h.bgMu.Unlock()
	long := "read the build log and say what failed. " + strings.Repeat("Then check each module's test output in turn. ", 6) + "END-OF-TASK"
	kept := spawnKid(t, h, long)
	gone := spawnKid(t, h, "an aside that was dismissed")
	waitIdle(t, kept)
	waitIdle(t, gone)
	gone.quit()
	h.notes()

	h.noteCompaction(agentCompaction(5, 100, 40))
	n := h.notes()
	if len(n) != 1 {
		t.Fatalf("want one after-fold notice, got %q", n)
	}
	note := n[0]
	for _, want := range []string{"just compacted", "do not start it again",
		"job #1 — RUNNING now", "· started ", "· running ", "`make test-integration`",
		"agent #1 — idle · started ", " · ran ", "END-OF-TASK"} {
		if !strings.Contains(note, want) {
			t.Errorf("notice lacks %q:\n%s", want, note)
		}
	}
	for _, not := range []string{"make lint", "an aside that was dismissed"} {
		if strings.Contains(note, not) {
			t.Errorf("notice names finished or dismissed work %q:\n%s", not, note)
		}
	}
}

// ── the current ask survives a long tool-heavy turn ─────────────────────

// foldFake answers the fold calls from a script and keeps what it was sent.
type foldFake struct {
	replies []string
	sent    []string // the user message of each call
}

func (f *foldFake) ChatStream(_ context.Context, msgs []llm.Message, _ []llm.ToolDef, _ *llm.ChatOpts) (<-chan llm.StreamChunk, error) {
	f.sent = append(f.sent, msgs[len(msgs)-1].Content)
	r := f.replies[min(len(f.sent)-1, len(f.replies)-1)]
	ch := make(chan llm.StreamChunk, 1)
	ch <- llm.StreamChunk{Content: r, Done: true}
	close(ch)
	return ch, nil
}

const repoAsk = "Rename the Widget type to Gadget in pkg/widget and fix every caller"

func foldGoodSummary(ask string) string {
	return "## Ask\n" + ask + "\n## Done\nread 6 files\n## Learned\nWidget is used in 4 places\n## Next\nedit callers"
}

// toolHeavyTurn is one user ask followed by n tool call/result pairs, each
// result big enough that the budget forces a fold.
func toolHeavyTurn(t *testing.T, h *Harness, n int, from int) {
	t.Helper()
	ctx := context.Background()
	if from == 0 {
		h.store.Append(ctx, "dun", agent.Entry{ID: "ask", Kind: agent.KindUser, Content: repoAsk, CreatedAt: 1})
	}
	for i := from; i < from+n; i++ {
		id := "tc" + itoa(i)
		h.store.Append(ctx, "dun", agent.Entry{ID: "c" + itoa(i), Kind: agent.KindToolCall, ToolName: "file_read",
			ToolCallID: id, Content: `{"path":"f` + itoa(i) + `"}`, CreatedAt: int64(10 + i*2)})
		h.store.Append(ctx, "dun", agent.Entry{ID: "r" + itoa(i), Kind: agent.KindToolResult, ToolName: "file_read",
			ToolCallID: id, Content: strings.Repeat("line of file "+itoa(i)+"\n", 300), CreatedAt: int64(11 + i*2)})
	}
}

// The repro: one ask, then more tool pairs than the pristine tail keeps. The
// fold subsumes the ask; the model must still see it after each fold, the plan
// must hold it once, and a summary that lacks it is asked for again.
func TestFold_CurrentAskSurvivesTwoFolds(t *testing.T) {
	h := newNoteHarness(t)
	fake := &foldFake{replies: []string{
		"## Done\nread some files", // no sections, no ask: rejected
		foldGoodSummary(repoAsk),   // the retry
		foldGoodSummary(repoAsk),   // second fold
	}}
	sh := &agent.Shaper{Store: h.store, Runner: fake, Estimate: &h.meter,
		Policy: agent.ShaperPolicy{BudgetTokens: 100000, PreserveLastMessages: 6, PreserveLastToolCalls: 4}}
	h.foldWire(sh)
	ctx := context.Background()
	h.addPlanNode(t, "wire the rename")

	toolHeavyTurn(t, h, 8, 0)
	h.compactOnce(t, sh)
	live, _ := h.store.Context(ctx, "dun")
	for _, e := range live {
		if e.ID == "ask" {
			t.Fatal("the ask is still an entry: the fold did not reach it, so this is not the repro")
		}
	}
	if len(fake.sent) != 2 {
		t.Fatalf("fold calls = %d, want 2 (the first summary lacked the ask and sections: one retry)", len(fake.sent))
	}
	if !strings.Contains(fake.sent[0], repoAsk) {
		t.Errorf("the fold was not told the ask:\n%s", fake.sent[0])
	}
	if !strings.Contains(fake.sent[1], "previous summary was rejected") ||
		!strings.Contains(fake.sent[1], "## Ask") || !strings.Contains(fake.sent[1], "word for word") {
		t.Errorf("the retry does not state the failure:\n%s", fake.sent[1])
	}
	h.expectRestore(t, "after fold 1")

	// More work in the same turn, then a second fold, which refolds the marker.
	toolHeavyTurn(t, h, 8, 8)
	h.compactOnce(t, sh)
	if len(fake.sent) != 3 {
		t.Fatalf("fold calls = %d, want 3", len(fake.sent))
	}
	if !strings.Contains(fake.sent[2], repoAsk) {
		t.Errorf("second fold was not told the ask:\n%s", fake.sent[2])
	}
	h.expectRestore(t, "after fold 2")

	g, _ := planStore{h}.View(ctx)
	n := 0
	for _, nd := range g.Nodes {
		if strings.Contains(nd.Details, repoAsk) {
			n++
			if nd.Status != taskgraph.Doing {
				t.Errorf("ask node is %s, want doing", nd.Status)
			}
		}
	}
	if n != 1 {
		t.Errorf("plan holds the ask %d times, want 1: %+v", n, g.Nodes)
	}
}

// addPlanNode is a plan item that is not the ask.
func (h *Harness) addPlanNode(t *testing.T, title string) {
	t.Helper()
	err := planStore{h}.Update(context.Background(), func(g *taskgraph.Graph) error {
		_, err := g.Add(taskgraph.Node{Title: title, DoneWhen: "done"})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// compactOnce is one fold as the Session runs it: the Shaper folds, then the
// harness is told (Session.OnCompactionComplete = noteCompaction). Returns the
// prompt the model would be sent next.
func (h *Harness) compactOnce(t *testing.T, sh *agent.Shaper) []llm.Message {
	t.Helper()
	ctx := context.Background()
	info, did, err := sh.Compact(ctx, "dun")
	if err != nil || !did {
		t.Fatalf("compact: did=%v err=%v", did, err)
	}
	h.noteCompaction(info)
	msgs, err := sh.Build(ctx, "dun", "system")
	if err != nil {
		t.Fatal(err)
	}
	return msgs
}

// expectRestore checks the after-fold aside: the ask verbatim and the open
// plan, and that two folds leave one such note, not two.
func (h *Harness) expectRestore(t *testing.T, when string) {
	t.Helper()
	h.noteMu.Lock()
	defer h.noteMu.Unlock()
	var notes []string
	for _, q := range h.queue {
		if q.key == "fold-restore" {
			notes = append(notes, q.text)
		}
	}
	if len(notes) != 1 {
		t.Fatalf("%s: %d restore notes queued, want 1", when, len(notes))
	}
	if c := strings.Count(notes[0], repoAsk); c != 1 {
		t.Errorf("%s: the ask is printed %d times in the note, want 1:\n%s", when, c, notes[0])
	}
	for _, want := range []string{"<ask>\n" + repoAsk + "\n</ask>", "Open plan:", "wire the rename"} {
		if !strings.Contains(notes[0], want) {
			t.Errorf("%s: restore note lacks %q:\n%s", when, want, notes[0])
		}
	}
}

// A summary that fails twice is kept; the restore note is what carries the ask.
func TestFold_SummaryKeptAfterSecondFailure(t *testing.T) {
	h := newNoteHarness(t)
	fake := &foldFake{replies: []string{"nothing useful"}}
	sh := &agent.Shaper{Store: h.store, Runner: fake, Estimate: &h.meter,
		Policy: agent.ShaperPolicy{BudgetTokens: 100000, PreserveLastMessages: 6, PreserveLastToolCalls: 4}}
	h.foldWire(sh)
	h.addPlanNode(t, "wire the rename")
	toolHeavyTurn(t, h, 8, 0)
	h.compactOnce(t, sh)
	if len(fake.sent) != 2 {
		t.Errorf("fold calls = %d, want exactly one retry", len(fake.sent))
	}
	h.expectRestore(t, "after a poor summary")
}

func TestFoldSummaryProblem(t *testing.T) {
	ask := "fix   the\nbug in x.go"
	good := "## Ask\nfix the bug in x.go\n## Done\n.\n## Learned\n.\n## Next\n."
	if p := foldProblem(good, ask); p != "" {
		t.Errorf("good summary rejected: %s", p)
	}
	if p := foldProblem(strings.Replace(good, "fix the bug", "repair the defect", 1), ask); !strings.Contains(p, "ask") {
		t.Errorf("paraphrased ask passed: %q", p)
	}
	if p := foldProblem("## Ask\nfix the bug in x.go", ask); !strings.Contains(p, "## Next") {
		t.Errorf("missing sections passed: %q", p)
	}
	if p := foldProblem("anything", ""); !strings.Contains(p, "## Ask") {
		t.Errorf("sections are required even without an ask: %q", p)
	}
}

func foldProblem(summary, ask string) string {
	return strings.Join(foldSummaryProblems(summary, ask), "; ")
}

func askNodes(h *Harness) (open []*taskgraph.Node, all int) {
	g, _ := planStore{h}.View(context.Background())
	for _, n := range g.Nodes {
		if strings.Contains(n.Details, askTag) {
			all++
			if planOpen(n) {
				open = append(open, n)
			}
		}
	}
	return
}

// A newer user message replaces the ask, and the one ask node is rewritten,
// not joined by a second.
func TestFold_SecondUserAskReplacesTheFirst(t *testing.T) {
	h := newNoteHarness(t)
	ctx := context.Background()
	h.store.Append(ctx, "dun", agent.Entry{ID: "u1", Kind: agent.KindUser, Content: "first ask", CreatedAt: 1})
	h.noteAsk(ctx)
	h.noteAsk(ctx) // a second fold, same ask
	h.store.Append(ctx, "dun", agent.Entry{ID: "u2", Kind: agent.KindUser, Content: "second ask", CreatedAt: 5})
	a := h.noteAsk(ctx)
	if a.text != "second ask" || len(a.later) != 0 {
		t.Errorf("ask = %+v, want the second message alone", a)
	}
	open, all := askNodes(h)
	if all != 1 || len(open) != 1 || !strings.Contains(open[0].Details, "second ask") ||
		strings.Contains(open[0].Details, "first ask") || open[0].Status != taskgraph.Doing {
		t.Errorf("want one doing node holding only the second ask, got %d (open %d): %+v", all, len(open), open)
	}
}

// A message typed mid-turn is part of the ask: the note carries it, the fold is
// told it, and a summary that omits it is not rejected for that.
func TestFold_MidTurnCorrectionCarried(t *testing.T) {
	h := newNoteHarness(t)
	ctx := context.Background()
	h.store.Append(ctx, "dun", agent.Entry{ID: "u1", Kind: agent.KindUser, Content: repoAsk, CreatedAt: 1})
	h.store.appendSilent(agent.Entry{ID: "d1", Kind: kindDelivered, Content: "actually only pkg/widget, not the callers", CreatedAt: 5})
	h.store.Append(ctx, "dun", agent.Entry{ID: "n1", Kind: agent.KindNotification, Content: "[session] an aside", CreatedAt: 6})
	h.noteAsk(ctx)

	for _, got := range []string{h.foldTask(), h.foldRestoreNote()} {
		if !strings.Contains(got, repoAsk+"\n\nthen, mid-turn:\nactually only pkg/widget, not the callers") {
			t.Errorf("ask and correction not carried in order:\n%s", got)
		}
		if strings.Contains(got, "an aside") {
			t.Errorf("an aside is not the ask:\n%s", got)
		}
	}
	if p := foldProblem(foldGoodSummary(repoAsk), h.askNow().text); p != "" {
		t.Errorf("a summary without the correction was rejected: %s", p)
	}
	if note := h.foldRestoreNote(); strings.Count(note, repoAsk) != 1 {
		t.Errorf("the ask is printed %d times in the note, want 1:\n%s", strings.Count(note, repoAsk), note)
	}
}

// Reset forgets the ask and drops its node.
func TestFold_ResetClearsAsk(t *testing.T) {
	h := newNoteHarness(t)
	ctx := context.Background()
	h.store.Append(ctx, "dun", agent.Entry{ID: "u1", Kind: agent.KindUser, Content: repoAsk, CreatedAt: 1})
	h.noteAsk(ctx)
	h.Reset()
	if a := h.askNow(); a.text != "" {
		t.Errorf("ask survived Reset: %+v", a)
	}
	if open, _ := askNodes(h); len(open) != 0 {
		t.Errorf("ask node still open after Reset: %+v", open)
	}
	if h.foldTask() != "" || h.foldRestoreNote() != "" {
		t.Error("a cleared session still tells the fold an ask")
	}
}

// A huge ask is capped in the note, and the 300-character check still matches
// a summary that quotes only its start.
func TestFold_HugeAskCapped(t *testing.T) {
	h := newNoteHarness(t)
	ctx := context.Background()
	huge := "START " + strings.Repeat("x", 9000) + " END"
	h.store.Append(ctx, "dun", agent.Entry{ID: "u1", Kind: agent.KindUser, Content: huge, CreatedAt: 1})
	h.noteAsk(ctx)
	note := h.foldRestoreNote()
	if !strings.Contains(note, "…[truncated]") || strings.Contains(note, " END") {
		t.Errorf("note not capped (len %d)", len(note))
	}
	if len(note) > foldAskCap+1000 {
		t.Errorf("note is %d bytes", len(note))
	}
	if p := foldProblem(foldGoodSummary(huge[:300]), huge); p != "" {
		t.Errorf("summary quoting the first 300 chars rejected: %s", p)
	}
	if open, _ := askNodes(h); len(open) != 1 || !strings.Contains(open[0].Details, " END") {
		t.Error("the plan node must hold the whole ask")
	}
}

// foldRunner finds the fold call by agent.IsFold on its ctx. If the Shaper
// stops marking it, the check goes silent: this fails first.
func TestFoldRunner_SeesTheShapersFold(t *testing.T) {
	rec := &sysFake{}
	shaperFold(t, rec)
	if !rec.fold {
		t.Fatal("the Shaper's fold request is not agent.IsFold")
	}
	if rec.calls != 2 {
		t.Errorf("calls = %d: the check did not see the fold (want rejected then retried)", rec.calls)
	}
	// A request with the worker's system but no fold ctx is not checked.
	h := newNoteHarness(t)
	f := &sysFake{}
	if _, _, _, err := collectText(context.Background(), foldRunner{inner: f, h: h}, foldMsgs(), nil, nil); err != nil || f.calls != 1 {
		t.Errorf("calls = %d (%v): a request outside a fold was checked", f.calls, err)
	}
}

// shaperFold runs one real Shaper fold with rec under foldRunner.
func shaperFold(t *testing.T, rec agent.LLMRunner) {
	t.Helper()
	h := newNoteHarness(t)
	sh := &agent.Shaper{Store: h.store, Runner: rec, Estimate: &h.meter,
		Policy: agent.ShaperPolicy{BudgetTokens: 100000, PreserveLastMessages: 6, PreserveLastToolCalls: 4}}
	h.foldWire(sh)
	toolHeavyTurn(t, h, 8, 0)
	if _, did, err := sh.Compact(context.Background(), "dun"); err != nil || !did {
		t.Fatalf("compact: %v %v", did, err)
	}
}

// foldCtx is the ctx of a real fold request: agent.IsFold is true on it, as
// on every request the Shaper sends to fold. agentkit keeps the key
// unexported, so a test that calls foldRunner directly takes it from a fold.
func foldCtx(t *testing.T) context.Context {
	t.Helper()
	rec := &sysFake{}
	shaperFold(t, rec)
	if rec.ctx == nil || !agent.IsFold(rec.ctx) {
		t.Fatal("no fold ctx")
	}
	return context.WithoutCancel(rec.ctx) // the fold is over; its values stay
}

type sysFake struct {
	calls int
	fold  bool
	ctx   context.Context
}

func (f *sysFake) ChatStream(ctx context.Context, msgs []llm.Message, _ []llm.ToolDef, _ *llm.ChatOpts) (<-chan llm.StreamChunk, error) {
	f.calls++
	f.fold = agent.IsFold(ctx)
	f.ctx = ctx
	return replay("no sections", ""), nil
}

// The retry replaces the first reply only when it is better.
func TestFoldRunner_RetryKeptOnlyIfBetter(t *testing.T) {
	for _, tc := range []struct {
		name    string
		replies []string
		want    string
	}{
		{"retry passes", []string{"bad", foldGoodSummary(repoAsk)}, foldGoodSummary(repoAsk)},
		{"retry worse", []string{"## Ask\n" + repoAsk + "\n## Done\nx", ""}, "## Ask\n" + repoAsk + "\n## Done\nx"},
		{"retry fewer problems", []string{"bad", "## Ask\n" + repoAsk + "\n## Done\nx"}, "## Ask\n" + repoAsk + "\n## Done\nx"},
		{"retry same or more", []string{"## Ask\n" + repoAsk + "\n## Done\nx", "## Done\nx"}, "## Ask\n" + repoAsk + "\n## Done\nx"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newNoteHarness(t)
			h.ask = foldAsk{id: "u", text: repoAsk, at: 1}
			f := &foldFake{replies: tc.replies}
			r := foldRunner{inner: f, h: h}
			text, _, _, err := collectText(foldCtx(t), r, []llm.Message{
				{Role: "system", Content: agent.DefaultSummarySystem}, {Role: "user", Content: "events"}}, nil, nil)
			if err != nil || text != tc.want {
				t.Errorf("got %q (%v), want %q", text, err, tc.want)
			}
		})
	}
}

// usageFake replies once, then sends the usage in its own chunk after the
// finish, as an OpenAI-style stream with include_usage does.
type usageFake struct{ reply string }

func (f usageFake) ChatStream(context.Context, []llm.Message, []llm.ToolDef, *llm.ChatOpts) (<-chan llm.StreamChunk, error) {
	ch := make(chan llm.StreamChunk, 2)
	ch <- llm.StreamChunk{Content: f.reply, Done: true}
	ch <- llm.StreamChunk{Usage: &llm.Usage{PromptTokens: 1000, CompletionTokens: 200}}
	close(ch)
	return ch, nil
}

// Each summarizer call of a shaper fold is a side call of kind "fold", with
// the usage that came after the finish; a call that is not a fold is not.
func TestFoldRunner_RecordsSideCall(t *testing.T) {
	for _, tc := range []struct {
		name, reply string
		calls       int
	}{
		{"passes first time", foldGoodSummary(repoAsk), 1},
		{"rejected, retried", "bad", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newNoteHarness(t)
			h.sideCalls = &sideCallStats{}
			h.ask = foldAsk{id: "u", text: repoAsk, at: 1}
			r := foldRunner{inner: usageFake{reply: tc.reply}, h: h}
			if _, _, _, err := collectText(foldCtx(t), r, []llm.Message{
				{Role: "system", Content: agent.DefaultSummarySystem}, {Role: "user", Content: "events"}}, nil, nil); err != nil {
				t.Fatal(err)
			}
			got := h.SideCalls()["fold"]
			if got == nil || got["calls"].(int) != tc.calls || got["generated"].(int) != 200*tc.calls {
				t.Errorf("fold side calls = %v, want %d calls of 200 generated each", got, tc.calls)
			}
		})
	}
	h := newNoteHarness(t)
	h.sideCalls = &sideCallStats{}
	r := foldRunner{inner: usageFake{reply: "hi"}, h: h}
	if _, _, _, err := collectText(context.Background(), r, []llm.Message{{Role: "user", Content: "hi"}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := h.SideCalls()["fold"]; got != nil {
		t.Errorf("a turn call is not a fold: %v", got)
	}
}

// A session resumed after a fold has no user entry for the ask; the ask node
// seeds it, so the next fold still carries it.
func TestFold_AskSurvivesResume(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "s1.jsonl")
	open := func() *Harness {
		s, err := openSessionStore(file)
		if err != nil {
			t.Fatal(err)
		}
		return &Harness{store: s, wake: make(chan struct{}, 1), cfg: Config{SessionFile: file}}
	}
	h1 := open()
	fake := &foldFake{replies: []string{foldGoodSummary(repoAsk)}}
	sh1 := &agent.Shaper{Store: h1.store, Runner: fake, Estimate: &h1.meter,
		Policy: agent.ShaperPolicy{BudgetTokens: 100000, PreserveLastMessages: 6, PreserveLastToolCalls: 4}}
	h1.foldWire(sh1)
	toolHeavyTurn(t, h1, 8, 0)
	h1.store.appendSilent(agent.Entry{ID: "d1", Kind: kindDelivered, Content: "only pkg/widget", CreatedAt: 3})
	h1.compactOnce(t, sh1)

	h2 := open() // a restart: new harness, same files
	if live, _ := h2.store.Context(context.Background(), "dun"); func() bool {
		for _, e := range live {
			if e.Kind == agent.KindUser {
				return true
			}
		}
		return false
	}() {
		t.Fatal("a user entry survived the fold: not the resume case")
	}
	if h2.askNow().text != "" {
		t.Fatal("fresh harness already has an ask")
	}
	toolHeavyTurn(t, h2, 8, 8)
	fake2 := &foldFake{replies: []string{foldGoodSummary(repoAsk)}}
	sh2 := &agent.Shaper{Store: h2.store, Runner: fake2, Estimate: &h2.meter,
		Policy: agent.ShaperPolicy{BudgetTokens: 100000, PreserveLastMessages: 6, PreserveLastToolCalls: 4}}
	h2.foldWire(sh2)
	h2.compactOnce(t, sh2)
	if len(fake2.sent) != 1 || !strings.Contains(fake2.sent[0], repoAsk) {
		t.Errorf("the resumed fold was not told the ask: %v", fake2.sent)
	}
	h2.expectRestoreAsk(t)
	note := h2.foldRestoreNote()
	if !strings.Contains(note, "then, mid-turn:\nonly pkg/widget") || strings.Count(note, "only pkg/widget") != 1 {
		t.Errorf("correction missing or doubled after resume:\n%s", note)
	}
	if open, all := askNodes(h2); len(open) != 1 || all != 1 {
		t.Errorf("ask nodes after resume: %d open, %d in all", len(open), all)
	}
}

func (h *Harness) expectRestoreAsk(t *testing.T) {
	t.Helper()
	h.noteMu.Lock()
	defer h.noteMu.Unlock()
	for _, q := range h.queue {
		if q.key == "fold-restore" && strings.Contains(q.text, "<ask>\n"+repoAsk) {
			return
		}
	}
	t.Error("no queued restore note carries the ask after resume")
}
