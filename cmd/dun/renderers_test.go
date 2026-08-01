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

func TestShipRender_NamesTheVerdictAndTheChecks(t *testing.T) {
	cases := []struct{ result, want string }{
		{"Shipped. dun/1 pushed to origin.\npassed: compile, lint.", "✓ shipped"},
		{"Verified. Rebased onto origin/main and passed: compile, vet.\nNothing pushed (mode: verify).", "✓ verified"},
		{"Verified, but NOT pushed: you are on main…", "⊘ refused"},
		{"Nothing to ship — dun/1 already matches origin/dun/1.", "nothing to ship"},
	}
	for _, c := range cases {
		got, _ := renderToolResult(renderCtx{tool: "ship", result: c.result})
		if !strings.Contains(got, c.want) {
			t.Errorf("ship %q → %q, want %q", firstLine(c.result), got, c.want)
		}
	}

	// "verified" without saying what ran is a claim ship has not earned, and
	// the collapsed line is exactly where that claim gets read.
	v, _ := renderToolResult(renderCtx{tool: "ship",
		result: "Verified. Rebased onto origin/main and passed: compile, lint.\nNothing pushed (mode: verify)."})
	if !strings.Contains(v, "compile, lint") {
		t.Errorf("the preview must name the checks: %q", v)
	}

	f, _ := renderToolResult(renderCtx{tool: "ship",
		result: "Checks failed at stage 1 of 3. Fix these, commit, then ship again.\n\nFAILED compile: go build ./...\nboom\n\nFAILED lint: golangci-lint run\nmeh"})
	if !strings.Contains(f, "1/3") {
		t.Errorf("say which stage failed: %q", f)
	}
	if !strings.Contains(f, "compile") || !strings.Contains(f, "lint") {
		t.Errorf("name every failing check — they are one turn's work, not two: %q", f)
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
