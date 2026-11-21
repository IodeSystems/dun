package harness

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
)

func addEntry(t *testing.T, h *Harness, kind agent.EntryKind, text string, at time.Time) {
	t.Helper()
	if err := h.store.Append(context.Background(), "dun", agent.Entry{ID: text, Kind: kind, Content: text, CreatedAt: at.UnixNano()}); err != nil {
		t.Fatal(err)
	}
}

// ── /plan summary ───────────────────────────────────────────────────────

const threeParts = "## Done\n- 1 parsed\n## Doing\n- 2 the encoder\n## Next\n- 3 the README"

// One side call over the plan and the recent session; the three parts are
// kept with when they were made, and the call is counted.
func TestPlanSummary_OneSideCallThreeSections(t *testing.T) {
	r := &cannedRephraseRunner{reply: threeParts}
	h := journalHarness(t, r)
	var events atomic.Int32
	var sawRunning atomic.Bool
	h.cfg.OnPlan = func() {
		events.Add(1)
		if h.PlanSummaryNow().Running {
			sawRunning.Store(true)
		}
	}
	planCall(t, h, "plan_add", `{"title":"Parse the file","done_when":"tests pass"}`)
	addEntry(t, h, agent.KindUser, "please parse notes.txt", time.Now())
	s, err := h.SummarizePlan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r.called != 1 {
		t.Fatalf("%d side calls, want 1", r.called)
	}
	p := r.seen[0].Content
	for _, want := range []string{planSummaryInstruction, "Parse the file", "please parse notes.txt"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt lacks %q:\n%s", want, p)
		}
	}
	for _, want := range []string{"## Done", "## Doing", "## Next"} {
		if !strings.Contains(s.Text, want) {
			t.Errorf("summary lacks %q: %q", want, s.Text)
		}
	}
	if s.At.IsZero() || !strings.HasPrefix(s.Made(), "made ") || s.Running {
		t.Errorf("summary %+v, made %q", s, s.Made())
	}
	if !sawRunning.Load() || events.Load() < 2 {
		t.Errorf("plan events %d, running seen %v: want one at the start (summarizing…) and one at the end", events.Load(), sawRunning.Load())
	}
	if h.SideCalls()["plan_summary"] == nil {
		t.Error("the side call is not counted")
	}
	// The summary is the person's; it never enters the conversation.
	for _, it := range h.History() {
		if strings.Contains(it.Content, "## Doing") {
			t.Error("the summary entered the session")
		}
	}
}

// A second summary replaces the first: one more call, the new text.
func TestPlanSummary_SecondRefreshes(t *testing.T) {
	r := &cannedRephraseRunner{reply: threeParts}
	h := journalHarness(t, r)
	if _, err := h.SummarizePlan(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.reply = "## Done\n- all\n## Doing\n- nothing\n## Next\n- nothing"
	s, err := h.SummarizePlan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r.called != 2 || !strings.Contains(s.Text, "- all") || h.PlanSummaryNow().Text != s.Text {
		t.Errorf("calls %d, summary %q", r.called, s.Text)
	}
}

// blockingRunner holds its reply until release is closed.
type blockingRunner struct {
	started chan struct{}
	release chan struct{}
}

func (b *blockingRunner) ChatStream(ctx context.Context, _ []llm.Message, _ []llm.ToolDef, _ *llm.ChatOpts) (<-chan llm.StreamChunk, error) {
	ch := make(chan llm.StreamChunk, 2)
	go func() {
		defer close(ch)
		close(b.started)
		select {
		case <-b.release:
		case <-ctx.Done():
			return
		}
		ch <- llm.StreamChunk{Content: threeParts}
		ch <- llm.StreamChunk{Done: true}
	}()
	return ch, nil
}

// A turn in flight (turnMu held) neither delays a summary nor is
// interrupted by one; a second press while one runs is refused, not queued.
func TestPlanSummary_BesideARunningTurn(t *testing.T) {
	b := &blockingRunner{started: make(chan struct{}), release: make(chan struct{})}
	h := rephraseHarness(t, b)
	h.sideCalls = &sideCallStats{}
	h.turnMu.Lock() // the turn
	defer h.turnMu.Unlock()
	turnCtx, endTurn := h.interruptible(context.Background())
	defer endTurn()

	done := make(chan error, 1)
	go func() { _, err := h.SummarizePlan(context.Background()); done <- err }()
	select {
	case <-b.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the summary waited for the turn")
	}
	if !h.PlanSummaryNow().Running {
		t.Error("not running while the call is out")
	}
	if _, err := h.SummarizePlan(context.Background()); err == nil {
		t.Error("a second summary while one runs: want a refusal")
	}
	close(b.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the summary did not finish")
	}
	if turnCtx.Err() != nil {
		t.Error("the turn was interrupted")
	}
	if s := h.PlanSummaryNow(); s.Running || !strings.Contains(s.Text, "## Next") {
		t.Errorf("after: %+v", s)
	}
}

func TestTailLines(t *testing.T) {
	var lines []string
	for i := 0; i < 50; i++ {
		lines = append(lines, strings.Repeat("a", 99))
	}
	got := tailLines(lines, 1000)
	if len(got) != 11 || !strings.Contains(got[0], "40 earlier entries left out") || got[len(got)-1] != lines[49] {
		t.Errorf("got %d: %q", len(got), got[0])
	}
	if got := tailLines(lines[:3], 1000); len(got) != 3 {
		t.Errorf("under the bound, all kept: %d", len(got))
	}
}

// ── /journal ────────────────────────────────────────────────────────────

// A fold is an entry, pointing at the span file the fold wrote; its body is
// the fold's summary, not the span.
func TestJournal_FoldIsAnEntry(t *testing.T) {
	h, _, _ := sessHarness(t, &fakeRag{})
	putFold(t, h, "fold-1", "port 7420 is the walrus service")
	all := h.Journal()
	if len(all) != 1 {
		t.Fatalf("entries: %+v", all)
	}
	e := all[0]
	if e.Kind != JournalFold || e.Title != "find the port" || e.Path != filepath.Join(foldsDir(h.cfg.SessionFile), "fold-1.md") || e.Text != "" {
		t.Errorf("entry %+v", e)
	}
	if e.Body() != "Found it." {
		t.Errorf("body %q", e.Body())
	}
	// A recap is not a fold: no entry.
	if err := agent.PutSpan(context.Background(), lateSessionIndex{h}, agent.SessionDocRecap, "recap-1", "", "r",
		[]agent.Entry{{Kind: agent.KindUser, Content: "x"}}); err != nil {
		t.Fatal(err)
	}
	if n := len(h.Journal()); n != 1 {
		t.Errorf("a recap added an entry: %d", n)
	}
}

// Without a dund link the fold writes no file: the entry keeps the summary.
func TestJournal_FoldWithoutAFileKeepsItsSummary(t *testing.T) {
	h := journalHarness(t, &cannedRephraseRunner{})
	putFold(t, h, "fold-1", "x")
	all := h.Journal()
	if len(all) != 1 || all[0].Path != "" || all[0].Body() != "Found it." {
		t.Fatalf("entries: %+v", all)
	}
}

// /journal now covers what happened since the last entry, not before it.
func TestJournal_NowSinceTheLastEntry(t *testing.T) {
	r := &cannedRephraseRunner{reply: "Fixed the parser\n- changed parse.go"}
	h := journalHarness(t, r)
	t0 := time.Now().Add(-time.Hour)
	addEntry(t, h, agent.KindUser, "OLD ASK", t0)
	if err := h.journalAdd(JournalEntry{At: t0.Add(time.Minute), Kind: JournalFold, Title: "earlier", Text: "s"}); err != nil {
		t.Fatal(err)
	}
	addEntry(t, h, agent.KindUser, "NEW ASK", t0.Add(2*time.Minute))
	addEntry(t, h, agent.KindAssistant, "NEW REPLY", t0.Add(3*time.Minute))
	e, n, err := h.JournalNow(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p := r.seen[0].Content
	if strings.Contains(p, "OLD ASK") || !strings.Contains(p, "NEW ASK") || !strings.Contains(p, "NEW REPLY") {
		t.Errorf("prompt is not bounded by the last entry:\n%s", p)
	}
	if n != 2 || e.Kind != JournalNow || e.Title != "Fixed the parser" || e.Path != filepath.Join(JournalDir(h.cfg.SessionFile), "2.md") {
		t.Errorf("entry #%d %+v", n, e)
	}
	if !strings.Contains(e.Body(), "changed parse.go") {
		t.Errorf("body %q", e.Body())
	}
	// Nothing since: refused, no call.
	if _, _, err := h.JournalNow(context.Background()); err == nil || r.called != 1 {
		t.Errorf("nothing new: err %v, calls %d", err, r.called)
	}
	if h.SideCalls()["journal"] == nil {
		t.Error("the side call is not counted")
	}
	// The prompt holds at most journalPromptMax of the record.
	for i := 0; i < 200; i++ {
		addEntry(t, h, agent.KindToolResult, strings.Repeat("y", 2000)+string(rune('a'+i%26)), time.Now().Add(time.Duration(i+1)*time.Millisecond))
	}
	if _, _, err := h.JournalNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := len(r.seen[0].Content); got > journalPromptMax+len(journalInstruction)+4000 {
		t.Errorf("prompt is %d chars: not bounded", got)
	}
}

// The journal survives a resume: a new harness on the same session file
// lists the entries, and /journal now counts from the last of them.
func TestJournal_SurvivesResume(t *testing.T) {
	h := journalHarness(t, &cannedRephraseRunner{})
	putFold(t, h, "fold-1", "x")
	if err := h.journalAdd(JournalEntry{Kind: JournalNow, Title: "second", Text: "did things"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(JournalFile(h.cfg.SessionFile)); err != nil {
		t.Fatal(err)
	}
	h2 := journalHarness(t, &cannedRephraseRunner{})
	h2.cfg.SessionFile = h.cfg.SessionFile
	out, err := h2.JournalCmd("")
	if err != nil {
		t.Fatal(err)
	}
	// Newest first.
	if i, j := strings.Index(out, "#2"), strings.Index(out, "#1"); i < 0 || j < 0 || i > j || !strings.Contains(out, "second") || !strings.Contains(out, "fold") {
		t.Errorf("list:\n%s", out)
	}
	v, err := h2.JournalCmd("view 2")
	if err != nil || !strings.Contains(v, "did things") {
		t.Errorf("view: %q %v", v, err)
	}
	if _, err := h2.JournalCmd("view 9"); err == nil {
		t.Error("view 9: want an error")
	}
}

func journalHarness(t *testing.T, runner *cannedRephraseRunner) *Harness {
	t.Helper()
	h := rephraseHarness(t, runner)
	h.cfg.SessionFile = filepath.Join(t.TempDir(), "s1.jsonl")
	h.cfg.Endpoint.Model = "m1"
	h.cfg.Version = "v9.9.9"
	h.sideCalls = &sideCallStats{}
	return h
}
