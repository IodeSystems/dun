package main

import (
	"strings"
	"testing"
)

func TestRenderers_Registry(t *testing.T) {
	// Unknown tool → generic (raw body).
	pv, full := renderToolResult(renderCtx{tool: "mystery", result: "just text"})
	if !strings.Contains(pv, "just text") || !strings.Contains(full, "just text") {
		t.Fatalf("generic renderer lost the text: %q / %q", pv, full)
	}

	// node_edit → diff stat preview + colorized body.
	diff := "--- a\n+++ b\n+added line\n-removed line\n"
	pv, full = renderToolResult(renderCtx{tool: "node_edit", result: diff})
	if !strings.Contains(pv, "+1 -1") {
		t.Fatalf("node_edit preview should be a diff stat, got %q", pv)
	}
	if !strings.Contains(full, "added line") {
		t.Fatalf("node_edit body should include the diff, got %q", full)
	}

	// search → JSON summary preview + pretty body.
	pv, full = renderToolResult(renderCtx{tool: "search", result: `[{"id":1},{"id":2},{"id":3}]`})
	if !strings.Contains(pv, "3 item(s)") {
		t.Fatalf("search preview should summarize the JSON array, got %q", pv)
	}
	if !strings.Contains(full, "\"id\"") {
		t.Fatalf("search body should be pretty JSON, got %q", full)
	}

	// search with non-JSON → falls back to generic.
	pv, _ = renderToolResult(renderCtx{tool: "search", result: "no results found"})
	if !strings.Contains(pv, "no results found") {
		t.Fatalf("non-JSON search should fall back to generic, got %q", pv)
	}

	// list_documents → JSON summary.
	pv, _ = renderToolResult(renderCtx{tool: "list_documents", result: `[{"path":"a.go","fragments":3},{"path":"b.go","fragments":1}]`})
	if !strings.Contains(pv, "2 item(s)") {
		t.Fatalf("list_documents should count items, got %q", pv)
	}

	// list_indexes → JSON summary.
	pv, _ = renderToolResult(renderCtx{tool: "list_indexes", result: `[{"name":"default","documents":5}]`})
	if !strings.Contains(pv, "1 item(s)") {
		t.Fatalf("list_indexes should count items, got %q", pv)
	}

	// search_figures → JSON summary.
	pv, _ = renderToolResult(renderCtx{tool: "search_figures", result: `[{"path":"x.pdf","page":1}]`})
	if !strings.Contains(pv, "1 item(s)") {
		t.Fatalf("search_figures should count items, got %q", pv)
	}
}

// ── dun's own tools ─────────────────────────────────────────────────

// The verdict has to survive collapsing. A preview that clips a failure into
// looking like output is worse than no preview at all.
func TestExecRender_CarriesTheVerdict(t *testing.T) {
	ok, _ := renderToolResult(renderCtx{tool: "exec", result: "hello\nworld\n"})
	if !strings.Contains(ok, "✓") || !strings.Contains(ok, "2 lines") {
		t.Errorf("a passing command should read as passing: %q", ok)
	}

	bad, body := renderToolResult(renderCtx{tool: "exec",
		result: "go: parse error\nundefined: x\n[exit: status 2]"})
	if !strings.Contains(bad, "✗") || !strings.Contains(bad, "status 2") {
		t.Errorf("a failure must show the exit status: %q", bad)
	}
	// The error is at the BOTTOM of a build log, not the top.
	if !strings.Contains(bad, "undefined: x") {
		t.Errorf("the preview should surface the last thing it said: %q", bad)
	}
	if !strings.Contains(body, "go: parse error") {
		t.Errorf("the body must stay verbatim: %q", body)
	}

	to, _ := renderToolResult(renderCtx{tool: "exec",
		result: "waiting…\n[exit: TIMED OUT after 5m0s and was killed. …]"})
	if !strings.Contains(to, "TIMED OUT") {
		t.Errorf("a timeout is not an ordinary failure: %q", to)
	}
}

// The marker is identified by POSITION, not by Contains — a command that prints
// it is not a failing command. Same trap the exec code itself was rewritten to
// avoid; here it would only be a cosmetic lie, which is still a lie.
func TestExecRender_OutputCannotFakeAFailure(t *testing.T) {
	preview, _ := renderToolResult(renderCtx{tool: "exec",
		result: "[exit: 1] <- this is just text we printed\nall good\n"})
	if strings.Contains(preview, "✗") {
		t.Errorf("a command that PRINTS the marker exited 0: %q", preview)
	}
	if !strings.Contains(preview, "✓") {
		t.Errorf("want a pass: %q", preview)
	}
}

func TestExecRender_BackgroundIsAReceipt(t *testing.T) {
	preview, _ := renderToolResult(renderCtx{tool: "exec",
		result: "Started background job #7: `go test ./...`. It has no time limit…"})
	if !strings.Contains(preview, "#7") || !strings.Contains(preview, "background") {
		t.Errorf("a background start must carry the job id — it is the only handle: %q", preview)
	}
	if strings.Contains(preview, "✓") {
		t.Errorf("a receipt is not a result: %q", preview)
	}
}

func TestAgentRender_ShowsStateAndSpend(t *testing.T) {
	started, _ := renderToolResult(renderCtx{tool: "agent",
		result: "Started agent #1: `count the callers`.\nTranscript: /s/1.sub1.jsonl"})
	if !strings.Contains(started, "#1") {
		t.Errorf("a spawn must name the agent: %q", started)
	}

	// The spend is the argument for delegating at all — it must survive
	// collapsing, or the feature's whole case is invisible.
	rep, _ := renderToolResult(renderCtx{tool: "agent",
		result: "agent #2 IDLE after 3m40s — spent 41.2k tokens on small\ntask: read the log\n137"})
	if !strings.Contains(rep, "#2") || !strings.Contains(rep, "idle") {
		t.Errorf("want id and state: %q", rep)
	}
	if !strings.Contains(rep, "41.2k tokens") {
		t.Errorf("the spend must show: %q", rep)
	}

	failed, _ := renderToolResult(renderCtx{tool: "agent",
		result: "agent #3 FAILED after 2s — spent 100 tokens\ntask: x\nFAILED: boom"})
	if !strings.Contains(failed, "failed") {
		t.Errorf("a failed child must read as failed: %q", failed)
	}
}

func TestAgentMonitorRender_ListsAndActs(t *testing.T) {
	empty, _ := renderToolResult(renderCtx{tool: "agent_monitor", result: "No sub-agents. Start one with agent({prompt:…})."})
	if !strings.Contains(empty, "no sub-agents") {
		t.Errorf("an empty list should say so plainly: %q", empty)
	}
	list, _ := renderToolResult(renderCtx{tool: "agent_monitor",
		result: "#1 RUNNING 12s — `a`\n#2 IDLE 4s — `b`"})
	if !strings.Contains(list, "2 agents") {
		t.Errorf("a listing should count: %q", list)
	}
	told, _ := renderToolResult(renderCtx{tool: "agent_monitor", result: "Answered agent #1 (\"which file?\") — it is running again."})
	if !strings.Contains(told, "Answered agent #1") {
		t.Errorf("an action should read as the action: %q", told)
	}
}

// The interesting content is in the ARGS. "status set." tells you nothing.
func TestTellParentRender_ReadsTheArgs(t *testing.T) {
	got, _ := renderToolResult(renderCtx{tool: "tell_parent",
		args: map[string]any{"status": "reading 12 of 40"}, result: "status set."})
	if !strings.Contains(got, "reading 12 of 40") {
		t.Errorf("show what the child said: %q", got)
	}
	// final beats status: it is the answer, and the answer is the point.
	both, _ := renderToolResult(renderCtx{tool: "tell_parent",
		args:   map[string]any{"status": "done", "final": "there are 3 callers"},
		result: "status set; answer recorded — you can stop now."})
	if !strings.Contains(both, "there are 3 callers") {
		t.Errorf("the final answer must win: %q", both)
	}
}

func TestAskRender_ShowsTheAnswerAndKeepsTheQuestion(t *testing.T) {
	preview, body := renderToolResult(renderCtx{tool: "ask_user",
		args: map[string]any{"question": "which config file?"}, result: "dun.json"})
	if !strings.Contains(preview, "dun.json") {
		t.Errorf("the preview is the ANSWER: %q", preview)
	}
	if !strings.Contains(body, "which config file?") {
		t.Errorf("the body must keep the question, or the answer is context-free: %q", body)
	}
	errp, _ := renderToolResult(renderCtx{tool: "ask_parent",
		args: map[string]any{"question": "q"}, result: "ERROR: no answer came back…"})
	if !strings.Contains(errp, "ERROR") {
		t.Errorf("a failed ask must not read like an answer: %q", errp)
	}
}

// A failing build's LAST line is a verdict, not a reason — `go test` ends with a
// bare "FAIL". A preview that says "FAIL" has spent its space saying nothing.
func TestExecTail_SkipsBareVerdicts(t *testing.T) {
	got := execTail("--- FAIL: TestFoo\n    foo_test.go:12: want 3 got 4\nFAIL\nexit status 1\n[exit: status 1]")
	if !strings.Contains(got, "foo_test.go:12") {
		t.Errorf("want the informative line, got %q", got)
	}
	// All-noise output still says something rather than nothing.
	if got := execTail("FAIL\n[exit: status 1]"); got != "FAIL" {
		t.Errorf("with nothing better, fall back to the last line: %q", got)
	}
	if got := execTail("[exit: status 1]"); got != "(no output)" {
		t.Errorf("no output should say so: %q", got)
	}
	// Bounded lookback: a long tail of noise must not scan the whole log.
	long := strings.Repeat("FAIL\n", 50) + "[exit: status 1]"
	if got := execTail(long); got != "FAIL" {
		t.Errorf("lookback should stay bounded: %q", got)
	}
}

// ── new renderers ─────────────────────────────────────────────────

func TestNodeReadRender(t *testing.T) {
	pv, body := renderToolResult(renderCtx{tool: "node_read", result: "func foo() {}\n\treturn x\n"})
	if !strings.Contains(pv, "✓") {
		t.Errorf("node_read should show success: %q", pv)
	}
	if !strings.Contains(pv, "2 lines") {
		t.Errorf("node_read should count lines: %q", pv)
	}
	if !strings.Contains(body, "func foo()") {
		t.Errorf("body should be verbatim: %q", body)
	}

	// Empty result.
	pv, _ = renderToolResult(renderCtx{tool: "node_read", result: ""})
	if !strings.Contains(pv, "empty") {
		t.Errorf("empty node_read should say so: %q", pv)
	}
}

func TestEvalRender(t *testing.T) {
	// Successful result.
	pv, _ := renderToolResult(renderCtx{tool: "eval", result: "42"})
	if !strings.Contains(pv, "✓") {
		t.Errorf("successful eval should show check: %q", pv)
	}
	if !strings.Contains(pv, "42") {
		t.Errorf("eval should show the result: %q", pv)
	}

	// Error result.
	pv, _ = renderToolResult(renderCtx{tool: "eval", result: "ERROR: mcp tool error: ERROR: Unknown command 'foo'"})
	if !strings.Contains(pv, "✗") {
		t.Errorf("failed eval should show error: %q", pv)
	}

	// No output.
	pv, _ = renderToolResult(renderCtx{tool: "eval", result: "   "})
	if !strings.Contains(pv, "no output") {
		t.Errorf("empty eval should say so: %q", pv)
	}
}

func TestGetDocRender(t *testing.T) {
	pv, _ := renderToolResult(renderCtx{tool: "get_document", result: "page 1 text\npage 2 text\npage 3 text\n"})
	if !strings.Contains(pv, "✓") {
		t.Errorf("get_document should show success: %q", pv)
	}
	if !strings.Contains(pv, "3 lines") {
		t.Errorf("get_document should count lines: %q", pv)
	}

	// Error.
	pv, _ = renderToolResult(renderCtx{tool: "get_document", result: "error: document not found"})
	if !strings.Contains(pv, "✗") {
		t.Errorf("failed get_document should show error: %q", pv)
	}
}

func TestOcrRender(t *testing.T) {
	// With page references.
	pv, _ := renderToolResult(renderCtx{tool: "ocr", result: "page 1: hello\npage 2: world\n"})
	if !strings.Contains(pv, "✓") {
		t.Errorf("ocr should show success: %q", pv)
	}
	if !strings.Contains(pv, "2 page") {
		t.Errorf("ocr should count pages: %q", pv)
	}

	// Without page references — falls back to line count.
	pv, _ = renderToolResult(renderCtx{tool: "ocr", result: "line one\nline two\nline three\n"})
	if !strings.Contains(pv, "3 lines") {
		t.Errorf("ocr without pages should count lines: %q", pv)
	}
}

func TestIngestRender(t *testing.T) {
	pv, _ := renderToolResult(renderCtx{tool: "ingest", result: "queued job #abc123 for indexing"})
	if !strings.Contains(pv, "queued") {
		t.Errorf("ingest should show queued: %q", pv)
	}

	// Error.
	pv, _ = renderToolResult(renderCtx{tool: "ingest", result: "error: invalid URL"})
	if !strings.Contains(pv, "✗") {
		t.Errorf("failed ingest should show error: %q", pv)
	}
}

func TestRecapRender(t *testing.T) {
	pv, _ := renderToolResult(renderCtx{tool: "recap", result: "replaced 15 entries (120k chars) → 3.2k chars saved"})
	if !strings.Contains(pv, "recap") {
		t.Errorf("recap should show recap marker: %q", pv)
	}

	// Error.
	pv, _ = renderToolResult(renderCtx{tool: "recap", result: "error: nothing to recap"})
	if !strings.Contains(pv, "✗") {
		t.Errorf("failed recap should show error: %q", pv)
	}
}

// ── view cycling ─────────────────────────────────────────────

func TestViewState_Cycles(t *testing.T) {
	if viewMinimized.Next() != viewExpanded || viewExpanded.Next() != viewMinimized {
		t.Errorf("minimized ↔ expanded: %v %v", viewMinimized.Next(), viewExpanded.Next())
	}
}

func TestConvoEntry_ViewStates(t *testing.T) {
	e := convoEntry{collapsed: "collapsed text", full: "full styled text"}
	if e.view() != "collapsed text" {
		t.Errorf("minimized view = %q, want collapsed", e.view())
	}
	e.state = viewExpanded
	if e.view() != "full styled text" {
		t.Errorf("expanded view = %q, want full", e.view())
	}
	if (convoEntry{collapsed: "c"}).expandable() {
		t.Error("entry with only collapsed should not be expandable")
	}
}

// execExitMarker, informative, backgroundJobID: the pure helpers behind
// execRender.

func TestExecExitMarker(t *testing.T) {
	// No marker.
	if m, ok := execExitMarker("hello world"); ok {
		t.Errorf("no marker: got %q ok=%v", m, ok)
	}

	// Simple marker.
	m, ok := execExitMarker("output\n[exit: 1]")
	if !ok || m != "1" {
		t.Errorf("exit 1: got %q ok=%v", m, ok)
	}

	// Marker with a reason.
	m, ok = execExitMarker("output\n[exit: signal: killed]")
	if !ok || m != "signal: killed" {
		t.Errorf("signal: got %q ok=%v", m, ok)
	}

	// Multi-digit.
	m, ok = execExitMarker("[exit: 255]")
	if !ok || m != "255" {
		t.Errorf("255: got %q ok=%v", m, ok)
	}
}

func TestInformative(t *testing.T) {
	// Bare verdicts: not informative.
	for _, s := range []string{"FAIL", "FAILED", "ERROR", "ERRORS", "OK", "PASS", "FAIL.", "PASS!"} {
		if informative(s) {
			t.Errorf("%q should not be informative", s)
		}
	}

	// Shell epilogue.
	if informative("exit status 1") {
		t.Error("exit status should not be informative")
	}

	// A "where: what" line.
	if !informative("harness.go:123: undefined: foo") {
		t.Error("file:line: msg should be informative")
	}

	// A long sentence.
	if !informative("this is a sentence that is long enough to count") {
		t.Error("long sentence should be informative")
	}

	// A short line with no colon: not informative.
	if informative("short") {
		t.Error("short line without colon should not be informative")
	}
}

func TestBackgroundJobID(t *testing.T) {
	// No prefix.
	if _, ok := backgroundJobID("no job here"); ok {
		t.Error("no prefix should not be ok")
	}

	// With id.
	id, ok := backgroundJobID("Started background job #42: running\noutput")
	if !ok || id != "42" {
		t.Errorf("job 42: got %q ok=%v", id, ok)
	}

	// Id followed by a dot.
	id, ok = backgroundJobID("Started background job #7.done")
	if !ok || id != "7" {
		t.Errorf("job 7: got %q ok=%v", id, ok)
	}
}

// jobs answers for both kinds: a list counts each, a command's report and an
// agent's report read as they did under exec_monitor and agent_monitor.
func TestJobsRender_BothKinds(t *testing.T) {
	list, _ := renderToolResult(renderCtx{tool: "jobs",
		result: "job #1 RUNNING for 3s (0 bytes out) — `sleep 9`\nagent #2 idle for 4s — task: x"})
	if !strings.Contains(list, "1 job") || !strings.Contains(list, "1 agent") {
		t.Errorf("a list counts each kind: %q", list)
	}
	empty, _ := renderToolResult(renderCtx{tool: "jobs", result: "No background jobs or sub-agents."})
	if !strings.Contains(empty, "no background jobs") {
		t.Errorf("empty: %q", empty)
	}
	agent, _ := renderToolResult(renderCtx{tool: "jobs", args: map[string]any{"agent": 2},
		result: "agent #2 is idle after 3m40s (41.2k tokens)\ntask: read the log"})
	if !strings.Contains(agent, "#2 idle") || !strings.Contains(agent, "41.2k tokens") {
		t.Errorf("an agent's report: %q", agent)
	}
	job, _ := renderToolResult(renderCtx{tool: "jobs", args: map[string]any{"job": 1},
		result: "#1 succeeded in 2s — `make`\nmonitor: completion notice on, no progress reports\nok"})
	if !strings.Contains(job, "#1 succeeded") {
		t.Errorf("a command's report: %q", job)
	}
	bad, _ := renderToolResult(renderCtx{tool: "jobs", result: "ERROR: give job or agent, not both."})
	if !strings.Contains(bad, "not both") {
		t.Errorf("an error: %q", bad)
	}
}

// exec's handoff names the job and the call that looks at it.
func TestExecRender_HandoffNamesTheJob(t *testing.T) {
	got, _ := renderToolResult(renderCtx{tool: "exec",
		result: "`make` is STILL RUNNING after 30s — it was NOT killed and has NOT failed. It is now background job #4 and it runs"})
	if !strings.Contains(got, "#4") || !strings.Contains(got, "jobs(job:4)") {
		t.Errorf("handoff: %q", got)
	}
}

// session: a read shows what runs; a merge shows the state set.
func TestSessionRender(t *testing.T) {
	const out = `{"harness":{"name":"dun","version":"v1.2","model":"m"},"state":{"goal":"ship","status":"done"}}`
	read, _ := renderToolResult(renderCtx{tool: "session", result: out})
	if !strings.Contains(read, "dun v1.2 · m") {
		t.Errorf("read: %q", read)
	}
	set, _ := renderToolResult(renderCtx{tool: "session", args: map[string]any{"status": "done"}, result: out})
	if !strings.Contains(set, "state: done · ship") {
		t.Errorf("merge: %q", set)
	}
}
