package dun

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/llm"
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
	// A bare tool NAME keeps only that tool's MOST RECENT call. Keeping every
	// call of it preserved the 255,720-character `cat` the recap existed to
	// remove — measured on the first run where a model recapped unprompted, and
	// the model reaches for the name it knows rather than a phrase.
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
	if kept != 2 { // the latest call and its result, not every exec ever run
		t.Errorf("a bare tool name should keep only the most recent call, got %d entries", kept)
	}
	// …and the big early result is NOT rescued by naming the tool.
	rescued := false
	for _, e := range byName.Kept {
		if e.ToolCallID == "c1" {
			rescued = true
		}
	}
	if rescued {
		t.Error("naming a tool must not preserve its oldest, largest result")
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

// The two shapes worth catching, named by the USER: a big result just landed,
// or the same tool is being hammered. Both are churn at the moment it is made,
// which is when a suggestion is worth anything.
func TestRecapCue_FiresOnTheShapeOfTheChurn(t *testing.T) {
	call := func(tool string) agent.Entry {
		return agent.Entry{Kind: agent.KindToolCall, ToolName: tool, Content: "{}"}
	}
	result := func(tool string, n int) agent.Entry {
		return agent.Entry{ID: "r" + tool + fmt.Sprint(n), Kind: agent.KindToolResult,
			ToolName: tool, Content: strings.Repeat("x", n)}
	}

	// Flailing: three exec calls in a row (two in history plus the one that
	// just ran) is not a plan.
	cue, ok := recapCueFor([]agent.Entry{call("exec"), result("exec", 10), call("exec"), result("exec", 10)}, "exec", 10)
	if !ok || !strings.Contains(cue.detail, "3 times in a row") {
		t.Fatalf("repetition should be caught: %+v ok=%v", cue, ok)
	}

	// Two in a row is still a plan.
	if _, ok := recapCueFor([]agent.Entry{call("exec"), result("exec", 10)}, "exec", 10); ok {
		t.Error("two calls of the same tool must not read as flailing")
	}

	// A large result the model has since moved past — moving past it is what
	// the current call proves.
	big := []agent.Entry{call("exec"), result("exec", 300000), call("eval"), result("eval", 20)}
	cue, ok = recapCueFor(big, "eval", 20)
	if !ok || !strings.Contains(cue.detail, "300000-character exec result") {
		t.Fatalf("a superseded large result should be named: %+v ok=%v", cue, ok)
	}

	// Nothing large, nothing repeated: say nothing.
	if _, ok := recapCueFor([]agent.Entry{call("exec"), result("exec", 50)}, "eval", 20); ok {
		t.Error("an ordinary exchange must not be nudged")
	}
}

// A suggestion is made once. Repeating it every call is how a useful signal
// becomes something the model learns to skip.
func TestRecapCue_NeverRepeatsItself(t *testing.T) {
	st, _ := openSessionStore("")
	h := &Harness{store: st, wake: make(chan struct{}, 1)}
	st.appendSilent(agent.Entry{Kind: agent.KindUser, Content: "count the errors in big.log please"})
	st.appendSilent(agent.Entry{ID: "big1", Kind: agent.KindToolResult, ToolName: "exec",
		Content: strings.Repeat("x", 300000)})

	h.watchRecap("eval", 20)
	first := h.notes()
	if len(first) != 1 {
		t.Fatalf("the first superseded large result should be raised, got %d", len(first))
	}
	if !strings.Contains(first[0], "recap({from:") || !strings.Contains(first[0], "count the errors in big.log") {
		t.Errorf("the suggestion must carry the call and an anchor: %q", first[0])
	}
	h.watchRecap("eval", 20)
	if n := h.notes(); len(n) != 0 {
		t.Errorf("the same cue must not be raised twice: %q", n)
	}
}

// The window-size fallback, for churn with no shape.
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
	if !strings.Contains(msg, "~40000 tokens") {
		t.Errorf("the fallback is about SIZE, so it must say the size: %q", msg)
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

// The cap is universal (USER): nothing about the harm is specific to exec. A
// node_read of a big file, a search with many hits and an eval returning a big
// structure all cost the same window, so the cap lives around EVERY tool.
func TestRecapWatch_CapsEveryTool(t *testing.T) {
	dir := t.TempDir()
	st, _ := openSessionStore(filepath.Join(dir, "s.jsonl"))
	h := &Harness{store: st, wake: make(chan struct{}, 1), cfg: Config{SessionFile: filepath.Join(dir, "s.jsonl")}}

	huge := strings.Repeat("{\"hit\": \"result line\"},\n", 20000)
	inner := func(context.Context, llm.ToolCall) (string, error) { return huge, nil }
	d := withRecapWatch(inner, h)

	var tc llm.ToolCall
	tc.Function.Name, tc.Function.Arguments = "search", "{}"
	out, err := d(context.Background(), tc)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > execInlineMax+400 {
		t.Fatalf("a non-exec tool went uncapped: %d characters", len(out))
	}
	// Structured output is clipped too, knowingly: an unbounded result would
	// only have to start with "{" to reopen the hole.
	if !strings.Contains(out, "characters elided") {
		t.Errorf("the gap must say what happened: %q", out[:200])
	}
	// A REF, not a path — under --docker a host path is not openable by the
	// model at all.
	if !strings.Contains(out, `recap({ref:`) {
		t.Errorf("the gap must say how to read the rest: %q", out[len(out)-300:])
	}

	// And that ref really reads back, dun-side: no shell, no container.
	ref := ""
	h.recapMu.Lock()
	for k := range h.spills {
		ref = k
	}
	h.recapMu.Unlock()
	if ref == "" {
		t.Fatal("no spill was registered")
	}
	got := h.readSpill(ref, "hit", 0, 0, 0, false)
	if !strings.Contains(got, "lines match") {
		t.Errorf("grep should work on a spilled result: %q", got[:120])
	}
	if head := h.readSpill(ref, "", 3, 0, 0, false); !strings.Contains(head, "first 3 of") {
		t.Errorf("head should work: %q", head[:120])
	}
	// full is BOUNDED: an unbounded read hands back exactly what the cap took
	// out, through a door marked convenience.
	full := h.readSpill(ref, "", 0, 0, 0, true)
	if len(full) > execInlineMax+500 {
		t.Errorf("a full read must stay bounded, got %d characters", len(full))
	}
	if h.readSpill("nope", "", 0, 0, 0, true) == "" || !strings.Contains(h.readSpill("nope", "", 0, 0, 0, true), "Known refs") {
		t.Error("an unknown ref should say which ones exist")
	}
}

// Output with no line structure — a minified document, a base64 payload, one
// enormous generated line — defeats grep, head and tail alike: every one of
// them is line-based, so they all hand back the single line that was the
// problem. Paging is the only handle on it.
func TestReadSpill_PagesOutputWithNoLines(t *testing.T) {
	dir := t.TempDir()
	st, _ := openSessionStore(filepath.Join(dir, "s.jsonl"))
	h := &Harness{store: st, cfg: Config{SessionFile: filepath.Join(dir, "s.jsonl")}}

	blob := "{" + strings.Repeat("\"k\":\"v\",", 12000) + "}" // one line, ~96k chars
	ref := h.spillRef("eval {}", blob)
	if ref == "" {
		t.Fatal("no ref")
	}

	// Asked about, it says the line-based tools cannot help and how to page.
	desc := h.readSpill(ref, "", 0, 0, 0, false)
	if !strings.Contains(desc, "no line structure") || !strings.Contains(desc, "at:0") {
		t.Fatalf("it must say paging is the way in: %q", desc)
	}

	first := h.readSpill(ref, "", 0, 0, 0, true)
	if !strings.Contains(first, "characters 0–") {
		t.Fatalf("full on an unlined blob should page from the start: %q", first[:120])
	}
	if !strings.Contains(first, "next page: recap({ref:") {
		t.Fatalf("a page must say where the next one starts: %q", first[:200])
	}
	if len(first) > execInlineMax+400 {
		t.Errorf("a page must stay bounded, got %d", len(first))
	}

	second := h.readSpill(ref, "", 0, 0, execInlineMax, false)
	if !strings.Contains(second, fmt.Sprintf("characters %d–", execInlineMax)) {
		t.Errorf("at: should page from the offset given: %q", second[:120])
	}
	// Past the end of the FILE, which is larger than the blob: the saved copy
	// carries a "$ command" header so the offsets are the file's, not the
	// output's.
	if past := h.readSpill(ref, "", 0, 0, len(blob)+1000, false); !strings.Contains(past, "past the end") {
		t.Errorf("an offset past the end must say so: %q", past)
	}
}

// A byte-boundary cut on multibyte text produces invalid UTF-8 — corruption
// that can break the transport, not merely read badly. The line-boundary trim
// does not help when there are no lines.
func TestCapExecOutput_NeverSplitsARune(t *testing.T) {
	out := strings.Repeat("héllo wörld ☃ ", 4000) // no newlines at all
	got := capExecOutput(out, "eval", nil)
	if utf8.ValidString(got) != true {
		t.Fatal("clipping produced invalid UTF-8")
	}
	if len(got) > execInlineMax+400 {
		t.Errorf("unlined output must still be clipped, got %d", len(got))
	}
}

// The -p stream used to carry the RAW result while the model got the clipped
// one, so the TUI rendered a quarter-megabyte the model had never read. Both
// sides must see the same text — and it must spill ONCE, or the human is told
// to read a copy the model has never heard of.
func TestCappedReporter_ShowsTheHumanWhatTheModelSaw(t *testing.T) {
	dir := t.TempDir()
	st, _ := openSessionStore(filepath.Join(dir, "s.jsonl"))
	h := &Harness{store: st, wake: make(chan struct{}, 1), cfg: Config{SessionFile: filepath.Join(dir, "s.jsonl")}}

	var reported string
	report := h.cappedReporter(func(_ string, _ map[string]any, result string) { reported = result })

	huge := strings.Repeat("output line\n", 30000)
	inner := func(_ context.Context, _ llm.ToolCall) (string, error) {
		report("exec", map[string]any{"command": "cat big"}, huge) // as each wrapper does
		return huge, nil
	}
	var tc llm.ToolCall
	tc.Function.Name, tc.Function.Arguments = "exec", `{"command":"cat big"}`
	toModel, err := withRecapWatch(inner, h)(context.Background(), tc)
	if err != nil {
		t.Fatal(err)
	}
	if reported != toModel {
		t.Fatalf("the human and the model must see the same text (%d vs %d chars)", len(reported), len(toModel))
	}
	h.recapMu.Lock()
	n := len(h.spills)
	h.recapMu.Unlock()
	if n != 1 {
		t.Fatalf("capping twice must not spill twice, got %d refs", n)
	}
}
