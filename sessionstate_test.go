package dun

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/agentkit/llm"
)

// The sidecar path mirrors the .meta.json convention: same directory, same id
// stem, a different suffix. An in-memory session (no file) has no sidecar.
func TestStateFile_DerivesFromSessionFile(t *testing.T) {
	if got := StateFile("/ws/sess/20260102-150405.jsonl"); got != filepath.Join("/ws/sess", "20260102-150405.state.json") {
		t.Fatalf("StateFile = %q", got)
	}
	if got := StateFile(""); got != "" {
		t.Fatalf("an in-memory session has no sidecar, got %q", got)
	}
}

// A round trip: save, read back, and the values — plus a set timestamp —
// survive. This is the acceptance criterion for the persisted state file.
func TestSessionState_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "20260102-150405.jsonl")
	st := SessionState{
		Goal:   "Fix bug",
		Plan:   []string{"Step 1"},
		Todo:   []string{"Test"},
		Status: "in_progress",
	}
	if err := SaveSessionState(file, st); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(StateFile(file)); err != nil {
		t.Fatalf("state file not created: %v", err)
	}
	got := LoadSessionState(file)
	if got.Goal != "Fix bug" || got.Status != "in_progress" ||
		len(got.Plan) != 1 || got.Plan[0] != "Step 1" ||
		len(got.Todo) != 1 || got.Todo[0] != "Test" {
		t.Fatalf("round trip lost data: %+v", got)
	}
	if got.UpdatedAt.IsZero() {
		t.Error("save should stamp UpdatedAt")
	}
}

// Idempotence: saving the same values again updates the timestamp and does not
// error or corrupt the state.
func TestSessionState_Idempotent(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "s.jsonl")
	st := SessionState{Goal: "Fix bug", Status: "in_progress"}
	if err := SaveSessionState(file, st); err != nil {
		t.Fatal(err)
	}
	first := LoadSessionState(file).UpdatedAt
	time.Sleep(2 * time.Millisecond)
	if err := SaveSessionState(file, st); err != nil {
		t.Fatalf("re-saving the same values must not error: %v", err)
	}
	again := LoadSessionState(file)
	if again.UpdatedAt.Before(first) {
		t.Errorf("timestamp did not advance: %v -> %v", first, again.UpdatedAt)
	}
	if again.Goal != "Fix bug" || again.Status != "in_progress" {
		t.Errorf("re-save corrupted the state: %+v", again)
	}
}

// A merge touches only the fields it was given. {status} must leave the goal,
// plan, and todo intact — this is what makes a partial update safe.
func TestSessionState_MergeLeavesUntouchedFields(t *testing.T) {
	h := newNoteHarness(t)
	h.state = SessionState{Goal: "Fix bug", Plan: []string{"Step 1", "Step 2"}, Todo: []string{"Test"}, Status: "in_progress"}

	done := "done"
	got := h.stateMerge(nil, nil, nil, &done)
	if got.Goal != "Fix bug" || len(got.Plan) != 2 || len(got.Todo) != 1 {
		t.Fatalf("a status-only merge touched other fields: %+v", got)
	}
	if got.Status != "done" {
		t.Errorf("status not updated: %q", got.Status)
	}

	// A new todo replaces the list; the goal survives.
	got2 := h.stateMerge(nil, nil, []string{"Just test"}, nil)
	if got2.Goal != "Fix bug" || got2.Status != "in_progress" {
		t.Errorf("todo merge lost the goal or reset status: %+v", got2)
	}
	if len(got2.Todo) != 1 || got2.Todo[0] != "Just test" {
		t.Errorf("todo not replaced: %+v", got2.Todo)
	}
}

// renderState reads like a handoff: goal first, then status, plan, open todo.
// A zero state renders to nothing — an empty block must not enter the prompt.
func TestRenderState(t *testing.T) {
	if got := renderState(SessionState{}); got != "" {
		t.Fatalf("an empty state must render nothing, got %q", got)
	}
	st := SessionState{
		Goal: "Fix bug", Status: "in_progress",
		Plan: []string{"Find it", "Fix it"}, Todo: []string{"Test"},
	}
	out := renderState(st)
	// The goal must come before the plan, the plan before the todo.
	if i, j := strings.Index(out, "Fix bug"), strings.Index(out, "Find it"); i < 0 || j < 0 || i > j {
		t.Fatalf("goal must precede plan:\n%s", out)
	}
	for _, want := range []string{"# Session state", "goal: Fix bug", "status: in_progress",
		"- [ ] Test"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

// Load is tolerant: a missing file and a corrupt file both yield a zero state,
// never an error. A resume must not fail because its annotation is unreadable.
func TestLoadSessionState_Tolerant(t *testing.T) {
	if got := LoadSessionState(filepath.Join(t.TempDir(), "nope.jsonl")); !got.empty() {
		t.Fatalf("missing file must load as empty, got %+v", got)
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "s.jsonl")
	if err := os.WriteFile(StateFile(file), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := LoadSessionState(file); !got.empty() {
		t.Fatalf("corrupt file must load as empty, got %+v", got)
	}
}

// End-to-end: the acceptance criterion. Set state on a session, kill it,
// resume the SAME session file, and the new harness has the state — in memory
// and in its system prompt. No state means no error and no injected block.
func TestSessionState_ResumeRehydrates(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "20260102-150405.jsonl")

	// Session one: set the state through the same path the tool uses.
	h1, err := Start(context.Background(), Config{
		Workspace: dir, SessionFile: file,
	})
	if err != nil {
		t.Fatal(err)
	}
	st := SessionState{Goal: "Fix bug", Plan: []string{"Step 1"}, Todo: []string{"Test"}, Status: "in_progress"}
	if err := SaveSessionState(file, st); err != nil {
		t.Fatal(err)
	}
	h1.Close()

	// Session two: a fresh harness on the same file. Rehydration happens at
	// Start, before the tools are built, so the system prompt already carries it.
	h2, err := Start(context.Background(), Config{
		Workspace: dir, SessionFile: file,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h2.Close()

	if h2.state.Goal != "Fix bug" || h2.state.Status != "in_progress" ||
		len(h2.state.Plan) != 1 || len(h2.state.Todo) != 1 {
		t.Fatalf("resume did not rehydrate state: %+v", h2.state)
	}
	if !strings.Contains(h2.Session.System, "Fix bug") {
		t.Errorf("the resumed system prompt is missing the goal:\n%s", h2.Session.System)
	}
	if !strings.Contains(h2.Session.System, "# Session state") {
		t.Errorf("the resumed system prompt is missing the state block:\n%s", h2.Session.System)
	}
}

// No state is not an error: a fresh session with no sidecar starts clean, and
// its system prompt carries no state block.
func TestSessionState_NoStateIsClean(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "20260102-150405.jsonl")
	h, err := Start(context.Background(), Config{
		Workspace: dir, SessionFile: file,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if !h.state.empty() {
		t.Fatalf("a fresh session must start with no state: %+v", h.state)
	}
	if strings.Contains(h.Session.System, "# Session state") {
		t.Errorf("no state means no block:\n%s", h.Session.System)
	}
}

// The tool is registered on a real session and routes by name: a session_state
// call is handled, anything else falls through to the inner dispatcher.
func TestSessionState_ToolRegistered(t *testing.T) {
	dir := t.TempDir()
	h, err := Start(context.Background(), Config{
		Workspace: dir, SessionFile: filepath.Join(dir, "s.jsonl"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	found := false
	for _, td := range h.Session.Tools {
		if td.Function.Name == "session_state" {
			found = true
		}
	}
	if !found {
		t.Fatal("session_state is not registered as a tool")
	}

	// A call for a different tool must fall through to the inner dispatcher.
	inner := func(ctx context.Context, tc llm.ToolCall) (string, error) {
		return "inner:" + tc.Function.Name, nil
	}
	d := withSessionState(inner, h, nil)
	var tc llm.ToolCall
	tc.Function.Name = "some_other_tool"
	tc.Function.Arguments = "{}"
	out, err := d(context.Background(), tc)
	if err != nil {
		t.Fatal(err)
	}
	if out != "inner:some_other_tool" {
		t.Fatalf("a non-session_state call must fall through, got %q", out)
	}
}
