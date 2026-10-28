package dun

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func worklogHarness(t *testing.T, runner *cannedRephraseRunner) *Harness {
	t.Helper()
	h := rephraseHarness(t, runner)
	h.cfg.SessionFile = filepath.Join(t.TempDir(), "s1.jsonl")
	h.cfg.Endpoint.Model = "m1"
	h.cfg.Version = "v9.9.9"
	h.sideCalls = &sideCallStats{}
	return h
}

func mustWork(t *testing.T, h *Harness, action string) string {
	t.Helper()
	msg, err := h.WorklogCmd(action)
	if err != nil {
		t.Fatalf("%q: %v", action, err)
	}
	return msg
}

// start, fork, end and return to the parent; the outcome and why are kept.
func TestWorklog_StartForkEnd(t *testing.T) {
	h := worklogHarness(t, &cannedRephraseRunner{})
	mustWork(t, h, "start Fix the login bug")
	if msg := mustWork(t, h, "start Find the cause"); !strings.Contains(msg, "subtask of #1") {
		t.Errorf("fork: %q", msg)
	}
	w := h.worklogLocked()
	if w.Open != 2 || w.task(2).Parent != 1 {
		t.Fatalf("open %d, parent %d", w.Open, w.task(2).Parent)
	}
	if msg := mustWork(t, h, "end blocked the log is gone"); !strings.Contains(msg, "back to #1") {
		t.Errorf("end: %q", msg)
	}
	if w.Open != 1 {
		t.Fatalf("after end the parent is open: %d", w.Open)
	}
	if c := w.task(2); c.Outcome != WorkBlocked || c.Why != "the log is gone" || c.End == nil {
		t.Errorf("recorded %+v", c)
	}
	// No outcome word: done, and the whole line is the why.
	mustWork(t, h, "end fixed it in auth.go")
	if p := w.task(1); p.Outcome != WorkDone || p.Why != "fixed it in auth.go" || w.Open != 0 {
		t.Errorf("recorded %+v, open %d", p, w.Open)
	}
	tree := mustWork(t, h, "")
	for _, want := range []string{"#1 Fix the login bug", "\n    #2 Find the cause", "blocked: the log is gone", "done: fixed it in auth.go"} {
		if !strings.Contains(tree, want) {
			t.Errorf("tree lacks %q:\n%s", want, tree)
		}
	}
	for _, bad := range []string{"end nothing open", "start", "end", "view 9", "frob"} {
		if _, err := h.WorklogCmd(bad); err == nil {
			t.Errorf("%q: want an error", bad)
		}
	}
	mustWork(t, h, "start x")
	if _, err := h.WorklogCmd("end done"); err == nil {
		t.Error("an end with no why must be refused")
	}
}

// Events land in the open task only, and view shows them.
func TestWorklog_EventsWhileOpen(t *testing.T) {
	h := worklogHarness(t, &cannedRephraseRunner{})
	h.workInput("before any task")
	mustWork(t, h, "start Parent")
	h.workInput("what does auth.go do?")
	mustWork(t, h, "start Child")
	report := h.cappedReporter(func(string, map[string]any, string) {})
	report("exec", map[string]any{"command": "go test ./..."}, "ok  \tdun\t1.2s\nsecond line")
	h.workRecap("read the tests; they pass")
	h.Notify("background job #1 finished")
	mustWork(t, h, "end done tested")
	h.workInput("back in the parent")

	w := h.worklogLocked()
	if n := len(w.task(1).Events); n != 2 {
		t.Errorf("parent has %d events, want 2: %+v", n, w.task(1).Events)
	}
	ev := w.task(2).Events
	if len(ev) != 3 || ev[0].Kind != WorkTool || ev[1].Kind != WorkRecap || ev[2].Kind != WorkNote {
		t.Fatalf("child events: %+v", ev)
	}
	if ev[0].Result != "ok dun 1.2s" || !strings.Contains(ev[0].Text, `exec {"command":"go test ./..."}`) {
		t.Errorf("tool event: %+v", ev[0])
	}
	view := mustWork(t, h, "view 2")
	for _, want := range []string{"#2 Child", "under #1 Parent", "tool exec", "→ ok dun 1.2s", "recap: read the tests", "note: background job #1"} {
		if !strings.Contains(view, want) {
			t.Errorf("view lacks %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "second line") {
		t.Error("a tool's result is its first line only")
	}
	if v := mustWork(t, h, "view"); !strings.Contains(v, "you: what does auth.go do?") {
		t.Errorf("bare view is the open task's:\n%s", v)
	}
}

// The prompt: the instruction, the harness header, the events, bounded.
func TestWorklog_AccountPrompt(t *testing.T) {
	w := &Worklog{}
	now := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	w.Begin("Big task", now)
	w.Record(WorkEvent{At: now, Kind: WorkInput, Text: "FIRST ASK"})
	for i := 0; i < 1500; i++ {
		w.Record(WorkEvent{At: now, Kind: WorkTool, Text: "exec", Result: strings.Repeat("x", 100)})
	}
	w.Record(WorkEvent{At: now, Kind: WorkRecap, Text: "LAST RECAP"})
	info := HarnessInfo{Name: "dun", Harness: "dun", Version: "v1.2.3", Model: "chat", Provider: "openai", Effort: "medium", SessionID: "abc"}
	if _, err := w.AccountPrompt(1, info); err == nil {
		t.Fatal("an open task has no account yet")
	}
	w.Finish(WorkDone, "shipped", now.Add(time.Hour))
	p, err := w.AccountPrompt(1, info)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{worklogInstruction, "# Big task", "Model: chat (openai) · effort: medium", "Harness: dun v1.2.3",
		"Outcome: done — shipped", "INPUT: FIRST ASK", "RECAP: LAST RECAP", "events left out here"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
	if len(p) > workPromptMax+len(worklogInstruction)+2000 {
		t.Errorf("prompt is %d chars: not bounded", len(p))
	}
}

// write: one side call, the header dun's, the file beside the session.
func TestWorklog_Write(t *testing.T) {
	r := &cannedRephraseRunner{reply: "## What was asked\nA fix."}
	h := worklogHarness(t, r)
	if _, err := h.WriteWorklog(context.Background(), ""); err == nil {
		t.Error("nothing closed: want an error")
	}
	mustWork(t, h, "start Fix: the login bug!")
	h.workInput("fix login")
	if _, err := h.WriteWorklog(context.Background(), "1"); err == nil {
		t.Error("an open task: want an error")
	}
	mustWork(t, h, "end done fixed")
	before := h.store.Loaded()
	path, err := h.WriteWorklog(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(WorklogDir(h.cfg.SessionFile), "1-fix-the-login-bug.md"); path != want {
		t.Errorf("path %q, want %q", path, want)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# Fix: the login bug!", "Model: m1", "Harness: dun v9.9.9", "## What was asked\nA fix."} {
		if !strings.Contains(string(b), want) {
			t.Errorf("account lacks %q:\n%s", want, b)
		}
	}
	if r.called != 1 || len(r.seen) != 1 || !strings.Contains(r.seen[0].Content, "INPUT: fix login") {
		t.Errorf("side call: %d calls, %+v", r.called, r.seen)
	}
	if h.store.Loaded() != before || len(h.History()) != 0 {
		t.Error("the account must not enter the session")
	}
	if got := h.worklogLocked().task(1).Account; got != path {
		t.Errorf("account path kept as %q", got)
	}
	if h.SideCalls()["worklog"] == nil {
		t.Error("the side call is not counted")
	}
}

// A resumed session keeps its worklog: a new harness on the same file.
func TestWorklog_SurvivesResume(t *testing.T) {
	h := worklogHarness(t, &cannedRephraseRunner{})
	mustWork(t, h, "start Outer")
	mustWork(t, h, "start Inner")
	h.workInput("hello")
	mustWork(t, h, "end dropped not needed")

	h2 := worklogHarness(t, &cannedRephraseRunner{})
	h2.cfg.SessionFile = h.cfg.SessionFile
	w := h2.worklogLocked()
	if len(w.Tasks) != 2 || w.Open != 1 || w.task(2).Outcome != WorkDropped || len(w.task(2).Events) != 1 {
		t.Fatalf("resumed: %+v", w)
	}
	h2.workInput("after resume")
	if n := len(h2.worklogLocked().task(1).Events); n != 1 {
		t.Errorf("resumed open task records: %d", n)
	}
}

func TestBoundLines(t *testing.T) {
	var lines []string
	for i := 0; i < 100; i++ {
		lines = append(lines, strings.Repeat("a", 99))
	}
	got := boundLines(lines, 1000)
	if len(got) < 3 || got[0] != lines[0] || got[len(got)-1] != lines[99] || !strings.Contains(strings.Join(got, "\n"), "events left out here") {
		t.Errorf("got %d lines: %q", len(got), got)
	}
	if got := boundLines(lines[:3], 1000); len(got) != 3 {
		t.Errorf("under the bound, all kept: %d", len(got))
	}
}

// A turn's final reply is an event of the open task, so the account can say
// what was answered.
func TestWorklog_RecordsTheReply(t *testing.T) {
	h := worklogHarness(t, &cannedRephraseRunner{})
	mustWork(t, h, "start Count lines")
	h.workReply("notes.txt has 1 line.")
	h.workReply("   ") // an empty reply is not an event
	ev := h.worklogLocked().task(1).Events
	if len(ev) != 1 || ev[0].Kind != WorkReply || ev[0].Text != "notes.txt has 1 line." {
		t.Fatalf("events: %+v", ev)
	}
	if v := mustWork(t, h, "view"); !strings.Contains(v, "dun: notes.txt has 1 line.") {
		t.Errorf("view:\n%s", v)
	}
	if got := ev[0].eventText(); !strings.Contains(got, "REPLY: notes.txt has 1 line.") {
		t.Errorf("prompt text: %q", got)
	}
}
