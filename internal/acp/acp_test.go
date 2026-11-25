package acp

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	sdk "github.com/coder/acp-go-sdk"

	"github.com/iodesystems/dun/proto"
)

// A turn end to end: session/new creates an in-place session, the prompt
// is a user message, and the turn's events are its updates — thinking,
// streamed text (not repeated by the final message), a tool call with its
// kind and file, its result with a diff, the plan — until done answers
// end_turn.
func TestTurn(t *testing.T) {
	r := newRig(t)
	r.d.state = []any{&proto.Commands{Items: []proto.CommandSpec{{Name: "plan", Description: "the plan", Usage: "[view|approve]"}}}}
	sid := r.newSession()
	cr := <-r.d.creates
	if cr.Workspace != r.cwd || slices.Contains(cr.Args, "--worktree") || cr.Resume != "" {
		t.Fatalf("create %+v: want the cwd, in place", cr)
	}
	if u := r.ed.take(); len(u) != 1 || u[0].AvailableCommandsUpdate == nil || u[0].AvailableCommandsUpdate.AvailableCommands[0].Input.Unstructured.Hint != "[view|approve]" {
		t.Fatalf("want the commands from the snapshot: %+v", u)
	}

	ch := r.prompt(sid, "fix it")
	if c := r.d.next(); c.Type != "user" || c.Payload.(*proto.User).Content != "fix it" || c.Session != "s1" {
		t.Fatalf("sent %s %+v", c.Type, c.Payload)
	}
	file := filepath.Join(r.cwd, "a.go")
	r.d.emit(&proto.UserEcho{ID: "m1", Content: "fix it"}, &proto.Turn{Kind: "user"},
		&proto.Reasoning{Text: "think"}, &proto.Token{Text: "Hel"}, &proto.Token{Text: "lo"},
		&proto.ToolCall{CallID: "c1", Tool: "file_edit", Args: map[string]any{"path": "a.go"}})
	pending(t, ch, "mid-turn")
	r.d.emit(&proto.ToolResult{CallID: "c1", Tool: "file_edit", Result: "+1 -1", Edits: []proto.FileEdit{
		{Path: file, OldText: "a\n", NewText: "b\n"}, {Path: file + ".bin", Skipped: "binary"}}},
		&proto.Plan{Rows: []proto.PlanNode{{ID: "1", Title: "edit", Status: "doing"}, {ID: "2", Title: "test", Status: "todo", Ready: true}, {ID: "3", Title: "old", Status: "dropped"}}},
		&proto.Message{Role: "assistant", Content: "Hello"}, &proto.Done{})
	if res := waitPrompt(t, ch); res.err != nil || res.stop != sdk.StopReasonEndTurn {
		t.Fatalf("prompt: %+v", res)
	}

	var text, thought []string
	var call *sdk.SessionUpdateToolCall
	var end *sdk.SessionToolCallUpdate
	var plan *sdk.SessionUpdatePlan
	for _, u := range r.ed.take() {
		switch {
		case u.AgentMessageChunk != nil:
			text = append(text, u.AgentMessageChunk.Content.Text.Text)
		case u.AgentThoughtChunk != nil:
			thought = append(thought, u.AgentThoughtChunk.Content.Text.Text)
		case u.ToolCall != nil:
			call = u.ToolCall
		case u.ToolCallUpdate != nil:
			end = u.ToolCallUpdate
		case u.Plan != nil:
			plan = u.Plan
		case u.UserMessageChunk != nil:
			t.Errorf("the bridge's own message echoed back: %+v", u.UserMessageChunk)
		}
	}
	if strings.Join(text, "") != "Hello" || strings.Join(thought, "") != "think" {
		t.Errorf("text %q thought %q", text, thought)
	}
	if call == nil || call.Kind != sdk.ToolKindEdit || call.Status != sdk.ToolCallStatusInProgress || call.Title != "file_edit: a.go" ||
		len(call.Locations) != 1 || call.Locations[0].Path != file {
		t.Fatalf("tool_call %+v", call)
	}
	content, _ := end.Content.Value()
	if st, _ := end.Status.Value(); st != sdk.ToolCallStatusCompleted || len(content) != 3 {
		t.Fatalf("tool_call_update status %v content %+v", st, content)
	}
	if d := content[1].Diff; d == nil || d.Path != file || d.NewText != "b\n" || d.OldText == nil || *d.OldText != "a\n" {
		t.Errorf("diff %+v", content[1])
	}
	if c := content[2].Content; c == nil || !strings.Contains(c.Content.Text.Text, "binary") {
		t.Errorf("a skipped edit is a note: %+v", content[2])
	}
	if plan == nil || len(plan.Entries) != 2 || plan.Entries[0].Status != sdk.PlanEntryStatusInProgress || plan.Entries[1].Priority != sdk.PlanEntryPriorityHigh {
		t.Errorf("plan %+v", plan)
	}
}

// session/cancel interrupts the turn, and the prompt is answered
// cancelled when the turn ends.
func TestCancel(t *testing.T) {
	r := newRig(t)
	sid := r.newSession()
	ch := r.prompt(sid, "long job")
	r.d.next()
	r.d.emit(&proto.UserEcho{ID: "m1", Content: "long job"}, &proto.Turn{Kind: "user"}, &proto.Token{Text: "working"})
	if err := r.cc.Cancel(context.Background(), sdk.CancelNotification{SessionId: sid}); err != nil {
		t.Fatal(err)
	}
	if c := r.d.next(); c.Type != "interrupt" {
		t.Fatalf("want an interrupt, got %s", c.Type)
	}
	pending(t, ch, "before done")
	r.d.emit(&proto.Done{Reason: "interrupted"})
	if res := waitPrompt(t, ch); res.stop != sdk.StopReasonCancelled {
		t.Fatalf("prompt: %+v", res)
	}
}

// A prompt sent while a turn runs is queued by dun. Delivered into the
// running turn, that turn's end answers both prompts; still queued when the
// turn ends, it is answered by the end of the next turn, which takes it.
func TestQueuedPrompt(t *testing.T) {
	r := newRig(t)
	sid := r.newSession()
	first := r.prompt(sid, "one")
	r.d.next()
	r.d.emit(&proto.UserEcho{ID: "m1", Content: "one"}, &proto.Turn{Kind: "user"})
	second := r.prompt(sid, "two")
	if c := r.d.next(); c.Payload.(*proto.User).Content != "two" {
		t.Fatalf("sent %+v", c.Payload)
	}
	r.d.emit(&proto.UserEcho{ID: "m2", Content: "two", Queued: true},
		&proto.ToolCall{CallID: "c1", Tool: "exec"}, &proto.ToolResult{CallID: "c1", Tool: "exec"},
		&proto.Delivered{IDs: []string{"m2"}, CallID: "c1"})
	third := r.prompt(sid, "three")
	r.d.next()
	r.d.emit(&proto.UserEcho{ID: "m3", Content: "three", Queued: true}, &proto.Done{})
	for _, ch := range []chan promptResult{first, second} {
		if res := waitPrompt(t, ch); res.stop != sdk.StopReasonEndTurn {
			t.Fatalf("prompt: %+v", res)
		}
	}
	pending(t, third, "queued past the turn's end")
	r.d.emit(&proto.Delivered{IDs: []string{"m3"}}, &proto.Turn{Kind: "user"}, &proto.Done{})
	if res := waitPrompt(t, third); res.stop != sdk.StopReasonEndTurn {
		t.Fatalf("third: %+v", res)
	}
}

// session/load streams the conversation before it answers.
func TestLoadReplay(t *testing.T) {
	r := newRig(t)
	file := filepath.Join(r.cwd, "new.go")
	r.d.items = []proto.HistoryItem{
		{Kind: "user", Content: "hi"},
		{Kind: "reasoning", Content: "hmm"},
		{Kind: "tool_call", Tool: "file_write", CallID: "c1", Args: map[string]any{"path": file}},
		{Kind: "tool_result", Tool: "file_write", CallID: "c1", Content: "wrote", Edits: []proto.FileEdit{{Path: file, NewText: "package x\n", Created: true}}},
		{Kind: "assistant", Content: "hello"},
		{Kind: "notification", Content: "[dun] note"},
	}
	if _, err := r.cc.LoadSession(context.Background(), sdk.LoadSessionRequest{SessionId: "s1", Cwd: r.cwd, McpServers: []sdk.McpServer{}}); err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, u := range r.ed.take() {
		switch {
		case u.UserMessageChunk != nil:
			kinds = append(kinds, "user:"+u.UserMessageChunk.Content.Text.Text)
		case u.AgentThoughtChunk != nil:
			kinds = append(kinds, "thought")
		case u.AgentMessageChunk != nil:
			kinds = append(kinds, "agent:"+u.AgentMessageChunk.Content.Text.Text)
		case u.ToolCall != nil:
			kinds = append(kinds, "call:"+string(u.ToolCall.Kind))
		case u.ToolCallUpdate != nil:
			c, _ := u.ToolCallUpdate.Content.Value()
			if len(c) != 2 || c[1].Diff == nil || c[1].Diff.OldText != nil {
				t.Errorf("a created file's diff has no old text: %+v", c)
			}
			kinds = append(kinds, "result")
		}
	}
	if got := strings.Join(kinds, " "); got != "user:hi thought call:edit result agent:hello" {
		t.Errorf("replay %q", got)
	}
	select {
	case c := <-r.d.creates:
		t.Errorf("a session dund has is not created again: %+v", c)
	default:
	}
}

// The file-access question is a permission request; the option picked is
// sent back as the answer.
func TestPermission(t *testing.T) {
	r := newRig(t)
	r.ed.asked = make(chan sdk.RequestPermissionRequest, 1)
	r.ed.perm = func(q sdk.RequestPermissionRequest) sdk.RequestPermissionResponse {
		return sdk.RequestPermissionResponse{Outcome: sdk.NewRequestPermissionOutcomeSelected("1")}
	}
	sid := r.newSession()
	ch := r.prompt(sid, "read /etc/hosts")
	r.d.next()
	opts := []string{"yes, for this session", "yes, for this project", "yes, everywhere", "no"}
	ask := &proto.Ask{AskID: "a1", CallID: "c1", Question: "file_read wants to read /etc/hosts?", Options: opts}
	r.d.emit(&proto.UserEcho{ID: "m1", Content: "read /etc/hosts"}, &proto.Turn{Kind: "user"},
		&proto.ToolCall{CallID: "c1", Tool: "file_read", Args: map[string]any{"path": "/etc/hosts"}},
		ask, &proto.TurnState{Phase: proto.PhaseWaitingAnswer, Ask: ask})
	q := <-r.ed.asked
	var kinds []sdk.PermissionOptionKind
	for _, o := range q.Options {
		kinds = append(kinds, o.Kind)
	}
	if q.ToolCall.ToolCallId != "c1" || !slices.Equal(kinds, []sdk.PermissionOptionKind{
		sdk.PermissionOptionKindAllowOnce, sdk.PermissionOptionKindAllowAlways, sdk.PermissionOptionKindAllowAlways, sdk.PermissionOptionKindRejectOnce}) {
		t.Fatalf("request %+v kinds %v", q, kinds)
	}
	c := r.d.next()
	if a, ok := c.Payload.(*proto.Answer); !ok || a.Value != "yes, for this project" || a.AskID != "a1" {
		t.Fatalf("answer %s %+v", c.Type, c.Payload)
	}
	select {
	case q := <-r.ed.asked:
		t.Fatalf("asked twice (ask and turn_state carry one question): %+v", q)
	default:
	}
	r.d.emit(&proto.Answered{AskID: "a1", Value: "yes, for this project"}, &proto.Done{})
	if res := waitPrompt(t, ch); res.stop != sdk.StopReasonEndTurn {
		t.Fatalf("prompt: %+v", res)
	}
}

// A multiple-choice question is the agent's text, and the next prompt is
// its answer: sent as an answer, answered with the turn it continues.
func TestTextQuestion(t *testing.T) {
	r := newRig(t)
	sid := r.newSession()
	first := r.prompt(sid, "pick")
	r.d.next()
	r.d.emit(&proto.UserEcho{ID: "m1", Content: "pick"}, &proto.Turn{Kind: "user"},
		&proto.Ask{AskID: "a1", Question: "which ones?", Options: []string{"x", "y"}, Multi: true})
	said := r.ed.waitSaid(t, "Your next message is the answer.")
	second := r.prompt(sid, "x and y")
	c := r.d.next()
	if a, ok := c.Payload.(*proto.Answer); !ok || a.Value != "x and y" || a.AskID != "a1" {
		t.Fatalf("want an answer, got %s %+v", c.Type, c.Payload)
	}
	r.d.emit(&proto.Answered{AskID: "a1"}, &proto.Done{})
	for _, ch := range []chan promptResult{first, second} {
		if res := waitPrompt(t, ch); res.stop != sdk.StopReasonEndTurn {
			t.Fatalf("prompt: %+v", res)
		}
	}
	if !strings.Contains(said, "which ones?") || !strings.Contains(said, "- y") {
		t.Errorf("question text %q", said)
	}
}

// A prompt that names one of dun's commands runs it; its reply is the
// agent's text.
func TestSlashCommand(t *testing.T) {
	r := newRig(t)
	r.d.state = []any{&proto.Commands{Items: []proto.CommandSpec{{Name: "jobs", Description: "jobs"}}}}
	sid := r.newSession()
	ch := r.prompt(sid, "/jobs list")
	c := r.d.next()
	if cc, ok := c.Payload.(*proto.ControlCmd); !ok || cc.ID != "slash" || cc.Action != "jobs list" {
		t.Fatalf("sent %s %+v", c.Type, c.Payload)
	}
	r.d.emit(&proto.ControlEvent{ID: "jobs", Message: "no jobs"})
	if res := waitPrompt(t, ch); res.stop != sdk.StopReasonEndTurn {
		t.Fatalf("prompt: %+v", res)
	}
}

func TestToolKind(t *testing.T) {
	for name, want := range map[string]sdk.ToolKind{
		"file_read": sdk.ToolKindRead, "node_edit": sdk.ToolKindEdit, "file_delete": sdk.ToolKindDelete,
		"file_move": sdk.ToolKindMove, "file_grep": sdk.ToolKindSearch, "exec": sdk.ToolKindExecute,
		"plan_add": sdk.ToolKindThink, "chrome__fetch": sdk.ToolKindFetch, "agent": sdk.ToolKindOther,
	} {
		if got := toolKind(name); got != want {
			t.Errorf("%s: %s, want %s", name, got, want)
		}
	}
}

func TestPromptText(t *testing.T) {
	got := promptText([]sdk.ContentBlock{
		sdk.TextBlock("look at"),
		sdk.ResourceLinkBlock("a.go", "file:///w/a.go"),
		sdk.ResourceBlock(sdk.EmbeddedResourceResource{TextResourceContents: &sdk.TextResourceContents{Uri: "file:///w/b.go", Text: "package b\n"}}),
	})
	want := "look at\n\n[file: /w/a.go]\n\n[/w/b.go]\n```\npackage b\n```"
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}
