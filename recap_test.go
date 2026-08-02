package dun

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/agentkit/agent"
)

// convo builds a conversation: a user message, then churn, then the live recap
// call the model is inside when it asks for this.
func convo() []agent.Entry {
	at := int64(0)
	e := func(k agent.EntryKind, content, callID, tool string) agent.Entry {
		at++
		return agent.Entry{ID: "e" + string(rune('a'+at)), Kind: k, Content: content,
			ToolCallID: callID, ToolName: tool, CreatedAt: at}
	}
	return []agent.Entry{
		e(agent.KindUser, "make the parser handle nested quotes", "", ""),
		e(agent.KindAssistant, "let me try regex", "", ""),
		e(agent.KindToolCall, `{"command":"go test"}`, "c1", "exec"),
		e(agent.KindToolResult, strings.Repeat("FAIL output ", 500), "c1", "exec"),
		e(agent.KindAssistant, "that was wrong, trying a stack", "", ""),
		e(agent.KindToolCall, `{"command":"go test"}`, "c2", "exec"),
		e(agent.KindToolResult, "ok", "c2", "exec"),
		e(agent.KindAssistant, "recapping now", "", ""),
		e(agent.KindToolCall, `{"from":"nested quotes"}`, "c9", "recap"),
	}
}

// The span is everything after the anchor, and the churn is what goes.
func TestPlanRecap_ReplacesTheSpanAfterTheAnchor(t *testing.T) {
	sp, err := planRecap(convo(), "nested quotes", nil)
	if err != nil {
		t.Fatal(err)
	}
	if sp.Anchor == nil || !strings.Contains(sp.Anchor.Content, "nested quotes") {
		t.Fatalf("anchor should be the user message, got %+v", sp.Anchor)
	}
	if sp.Chars < 5000 {
		t.Errorf("the big tool result should dominate the saving, got %d chars", sp.Chars)
	}
	for _, e := range sp.Subsumes {
		if e.ToolCallID == "c9" {
			t.Fatal("the live recap call must never be subsumed — its own result would be orphaned")
		}
	}
}

// The one shape a provider rejects outright is a call without its result. A
// kept id keeps BOTH halves; an unkept one loses both.
func TestPlanRecap_KeepsToolPairsWhole(t *testing.T) {
	sp, err := planRecap(convo(), "nested quotes", []string{"c2"})
	if err != nil {
		t.Fatal(err)
	}
	kept := map[string]int{}
	for _, e := range sp.Kept {
		kept[e.ToolCallID]++
	}
	if kept["c2"] != 2 {
		t.Errorf("a kept call must keep its call AND its result, got %d entries", kept["c2"])
	}
	dropped := map[string]int{}
	for _, e := range sp.Subsumes {
		if e.ToolCallID != "" {
			dropped[e.ToolCallID]++
		}
	}
	if dropped["c1"] != 2 {
		t.Errorf("an unkept call must lose both halves, got %d", dropped["c1"])
	}
	if dropped["c2"] != 0 {
		t.Error("a kept call must not also appear in the removed set")
	}
}

func TestPlanRecap_RefusesWhatItCannotAnchor(t *testing.T) {
	if _, err := planRecap(convo(), "a phrase nobody said", nil); err == nil {
		t.Error("an unmatched anchor must be an error, not a silent whole-conversation wipe")
	}
	if _, err := planRecap(convo(), "", nil); err == nil {
		t.Error("an empty anchor must be refused")
	}
	// The anchor is matched from the END backwards, so the most recent wins.
	sp, err := planRecap(convo(), "go test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if sp.Anchor.ToolCallID != "c2" {
		t.Errorf("the LAST match should anchor, got %q", sp.Anchor.ToolCallID)
	}
}

// End to end against a real store: the churn leaves the context, the summary
// takes its place, the removed entries are on disk, and the citation never
// reaches the model.
func TestRecap_MovesChurnToDiskAndOutOfContext(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	st, err := openSessionStore(path)
	if err != nil {
		t.Fatal(err)
	}
	h := &Harness{store: st, cfg: Config{SessionFile: path}}
	for _, e := range convo() {
		st.appendSilent(e)
	}
	before, _ := st.Context(context.Background(), "dun")

	sp, err := planRecap(before, "nested quotes", []string{"c2"})
	if err != nil {
		t.Fatal(err)
	}
	note, err := h.applyRecap(sp, "The parser needed a stack, not a regex. Done and tested.", "")
	if err != nil {
		t.Fatal(err)
	}

	after, _ := st.Context(context.Background(), "dun")
	text := ""
	for _, e := range after {
		text += e.Content
		if e.Kind == kindRecap {
			t.Error("the citation must be filtered out of the model's context")
		}
	}
	if strings.Contains(text, "FAIL output") {
		t.Error("the churn is still in context")
	}
	if !strings.Contains(text, "a stack, not a regex") {
		t.Error("the summary did not take its place")
	}
	if !strings.Contains(text, "make the parser handle nested quotes") {
		t.Error("the anchor itself must survive — the span starts AFTER it")
	}
	// The kept pair survives whole, and the live call is still there to receive
	// its result.
	var c2, c9 int
	for _, e := range after {
		switch e.ToolCallID {
		case "c2":
			c2++
		case "c9":
			c9++
		}
	}
	if c2 != 2 || c9 != 1 {
		t.Errorf("kept pair=%d live call=%d — both must survive", c2, c9)
	}

	// Nothing was destroyed: the sidecar holds what left.
	side := filepath.Join(dir, "s.recap1.jsonl")
	data, err := os.ReadFile(side)
	if err != nil {
		t.Fatalf("the removed entries must be on disk: %v", err)
	}
	if !strings.Contains(string(data), "FAIL output") {
		t.Error("the sidecar should hold the churn — it is the evidence for fixing the tooling")
	}
	if !strings.Contains(note, "s.recap1.jsonl") {
		t.Errorf("the citation must name where the churn went: %q", note)
	}
}

// Recapping the user's own message is the point of `user`: a request that was
// ambiguous is rewritten to what it turned out to mean.
func TestRecap_RewritesTheAnchoringUserMessage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	st, _ := openSessionStore(path)
	h := &Harness{store: st, cfg: Config{SessionFile: path}}
	for _, e := range convo() {
		st.appendSilent(e)
	}
	before, _ := st.Context(context.Background(), "dun")
	sp, _ := planRecap(before, "nested quotes", nil)

	if _, err := h.applyRecap(sp, "done", "make the parser handle nested quotes INSIDE attribute values"); err != nil {
		t.Fatal(err)
	}
	after, _ := st.Context(context.Background(), "dun")
	if !strings.Contains(after[0].Content, "INSIDE attribute values") {
		t.Fatalf("the user message should be clarified, got %q", after[0].Content)
	}
}

// The confirmation is not decoration: a decline must leave the conversation
// exactly as it was.
func TestRecap_DeclinedLeavesEverythingAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	st, _ := openSessionStore(path)
	asked := ""
	h := &Harness{store: st, cfg: Config{
		SessionFile: path,
		Ask: func(_ context.Context, q string, _ []string, _ bool) (string, error) {
			asked = q
			return "leave it alone", nil
		},
	}}
	for _, e := range convo() {
		st.appendSilent(e)
	}
	before, _ := st.Context(context.Background(), "dun")

	out := h.runRecap(context.Background(), "nested quotes", "a summary", nil, "")
	if !strings.Contains(out, "declined") {
		t.Errorf("a decline must be reported to the model: %q", out)
	}
	if !strings.Contains(asked, "Recap would remove") || !strings.Contains(asked, "a summary") {
		t.Errorf("the human must see the count AND the replacement: %q", asked)
	}
	after, _ := st.Context(context.Background(), "dun")
	if len(after) != len(before) {
		t.Fatalf("a declined recap changed the conversation: %d → %d", len(before), len(after))
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), "s.recap1.jsonl")); err == nil {
		t.Error("a declined recap must not write a sidecar")
	}
}

// A sub-agent has no human to ask, and its context is exactly what recap is
// for, so it applies without confirmation.
func TestRecap_ChildRecapsWithoutAsking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	st, _ := openSessionStore(path)
	h := &Harness{store: st, cfg: Config{SessionFile: path}} // no Ask: a child
	for _, e := range convo() {
		st.appendSilent(e)
	}
	out := h.runRecap(context.Background(), "nested quotes", "the answer is a stack", nil, "")
	if strings.Contains(out, "ERROR") || strings.Contains(out, "declined") {
		t.Fatalf("a child should recap freely: %q", out)
	}
	after, _ := st.Context(context.Background(), "dun")
	text := ""
	for _, e := range after {
		text += e.Content
	}
	if strings.Contains(text, "FAIL output") {
		t.Error("the child's churn should be gone")
	}
	_ = time.Now
}

// The model cannot see tool_call ids — they are protocol-level and never appear
// in its context. Live run: it invented "exec_2", matched nothing, and silently
// lost the one result it had asked to keep. So keep takes a tool NAME or a
// phrase from the call's arguments, and anything that matches nothing is SAID.
func TestPlanRecap_KeepMatchesWhatTheModelCanActuallySee(t *testing.T) {
	byName, err := planRecap(convo(), "nested quotes", []string{"exec"})
	if err != nil {
		t.Fatal(err)
	}
	kept := 0
	for _, e := range byName.Kept {
		if e.ToolName == "exec" {
			kept++
		}
	}
	if kept != 4 { // two calls, two results
		t.Errorf("a tool NAME should keep every call of it and its results, got %d", kept)
	}

	byArgs, err := planRecap(convo(), "nested quotes", []string{"go test"})
	if err != nil {
		t.Fatal(err)
	}
	if len(byArgs.Kept) < 2 {
		t.Errorf("a phrase from the arguments should keep that call, got %d", len(byArgs.Kept))
	}

	// The failure that actually happened: an id the model made up.
	invented, err := planRecap(convo(), "nested quotes", []string{"exec_2"})
	if err != nil {
		t.Fatal(err)
	}
	if len(invented.Unmatched) != 1 || invented.Unmatched[0] != "exec_2" {
		t.Fatalf("an unmatched keep term must be reported, got %v", invented.Unmatched)
	}
	if !strings.Contains(invented.preview("s", ""), "matched nothing") {
		t.Error("the human must be told a keep term will not be honoured")
	}
}

// The recap call's own arguments quote the keep terms, so it matched itself and
// the confirmation read "keeping the exec, recap calls" — true, and confusing.
// It is kept unconditionally anyway.
func TestPlanRecap_KeepDoesNotMatchTheRecapCallItself(t *testing.T) {
	sp, err := planRecap(convo(), "nested quotes", []string{"from"})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range sp.KeptCalls {
		if name == "recap" {
			t.Fatalf("the live call must not appear in the kept list: %v", sp.KeptCalls)
		}
	}
}

// A prompt line was not enough. Live, with recap fully described in the system
// prompt: cat (255,720 chars), a failed eval, two help lookups, 902,875 tokens
// billed, 65,127 active — and no recap. The model ends its turn the moment it
// has the answer, and nothing in that moment is about its context. So the
// reminder arrives when it is expensive and NAMES what is costing the most.
func TestRecapNudge_ArrivesWhenItIsExpensiveAndNamesTheOffender(t *testing.T) {
	st, _ := openSessionStore("")
	h := &Harness{store: st, wake: make(chan struct{}, 1)}
	st.appendSilent(agent.Entry{Kind: agent.KindUser, Content: "how many lines in big.log contain ERROR and why"})
	st.appendSilent(agent.Entry{Kind: agent.KindToolResult, ToolName: "exec", Content: strings.Repeat("log line ", 30000)})

	// Below the threshold: a small session is never nagged.
	h.maybeNudgeRecap(1000)
	if n := h.notes(); len(n) != 0 {
		t.Fatalf("a small window must not be nudged: %q", n)
	}

	h.maybeNudgeRecap(40000)
	notes := h.notes()
	if len(notes) != 1 {
		t.Fatalf("a large window should earn exactly one reminder, got %d", len(notes))
	}
	msg := notes[0]
	if !strings.Contains(msg, "from exec") {
		t.Errorf("the reminder must name what is filling the window: %q", msg)
	}
	if !strings.Contains(msg, "how many lines in big.log") {
		t.Errorf("it must hand over an anchor the model can quote back: %q", msg)
	}
	if !strings.Contains(msg, "recap({from:") {
		t.Errorf("it must show the call, not just mention the idea: %q", msg)
	}

	// Repeating it every chat round past the threshold would be nagging.
	h.maybeNudgeRecap(41000)
	if n := h.notes(); len(n) != 0 {
		t.Errorf("the same reminder must not repeat without real growth: %q", n)
	}
	h.maybeNudgeRecap(60000)
	if n := h.notes(); len(n) != 1 {
		t.Errorf("real growth should earn a fresh reminder, got %d", len(n))
	}
}

// A big window with no single offender is compaction's problem, not recap's:
// there is nothing specific to point at, and vague advice is what already
// failed.
func TestRecapNudge_SilentWithNoSingleOffender(t *testing.T) {
	st, _ := openSessionStore("")
	h := &Harness{store: st, wake: make(chan struct{}, 1)}
	for i := 0; i < 50; i++ {
		st.appendSilent(agent.Entry{Kind: agent.KindAssistant, Content: strings.Repeat("x", 500)})
	}
	h.maybeNudgeRecap(40000)
	if n := h.notes(); len(n) != 0 {
		t.Errorf("no single large entry means nothing to name: %q", n)
	}
}
