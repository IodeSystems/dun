package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// Descending into a sub-agent from the activity strip: its conversation,
// live, read in the root's pane, and back out to the root as it was left.

// scopeModel is a TUI with one running and one idle child, whose engine
// stdin is recorded in buf.
func scopeModel(t *testing.T, w int) (tuiModel, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	m := newTUIModel(&dunProc{stdin: bufCloser{&buf}}, "/ws")
	nm, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: 24})
	m = nm.(tuiModel)
	m = m.handleEvent(evMsg{"type": "ready", "tools": []any{"eval"}})
	m = m.handleEvent(evMsg{"type": "agents", "agents": []any{
		map[string]any{"id": 1.0, "state": "running", "prompt": "read the build log", "tokens": 6500.0, "seconds": 122.0},
		map[string]any{"id": 2.0, "state": "idle", "prompt": "count matches", "tokens": 900.0, "seconds": 41.0},
	}})
	return m, &buf
}

// sentAgentCmds is every `agent` command the TUI wrote, as "action id content".
func sentAgentCmds(buf *bytes.Buffer) []string {
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(buf.Bytes()))
	for sc.Scan() {
		var c map[string]string
		if json.Unmarshal(sc.Bytes(), &c) != nil || c["type"] != "agent" {
			continue
		}
		out = append(out, strings.TrimSpace(c["action"]+" "+c["id"]+" "+c["content"]))
	}
	return out
}

// Enter on the strip's agent row opens it; the header names it, its task and
// the way back, and shortens on a phone with the way back intact.
func TestAgentScope_HeaderNamesTheChildAndTheWayBack(t *testing.T) {
	m, buf := scopeModel(t, 100)
	m = key(m, kTab) // input → convo
	m = key(m, kTab) // convo → activity
	m = key(m, kEnter)
	if m.scopeAgent != 1 {
		t.Fatalf("enter on the first agent row: scope %d", m.scopeAgent)
	}
	if got := sentAgentCmds(buf); len(got) != 1 || got[0] != "view 1" {
		t.Errorf("opening must ask the engine for the child's view: %q", got)
	}
	if v := ansi.Strip(m.activityView()); v != "↰ agent #1 · read the build log · ← esc back" {
		t.Errorf("header: %q", v)
	}
	if !strings.Contains(m.input.placeholder, "tell agent #1") {
		t.Errorf("the placeholder must say typing tells the child: %q", m.input.placeholder)
	}

	m.agents[0].prompt = "read the whole build log and say which package failed first, and why"
	m = resized(m, 48, 24)
	v := ansi.Strip(m.activityView())
	if !strings.HasPrefix(v, "↰ #1 · read the") || !strings.HasSuffix(v, "… · ← esc") {
		t.Errorf("phone header: %q", v)
	}
	if w := lipgloss.Width(v); w > 47 {
		t.Errorf("phone header is %d columns wide, over 47: %q", w, v)
	}
}

// The child's view is its history, then its entries live: a call and its
// result that arrive apart fold into one run, like the root's — also when
// the call came in the history and the result live.
func TestAgentScope_ShowsHistoryThenLiveEntries(t *testing.T) {
	m, _ := scopeModel(t, 100)
	m.enterAgentScope(1)
	m = m.handleEvent(evMsg{"type": "agent_history", "agent": 1.0, "prompt": "read the build log", "items": []any{
		map[string]any{"kind": "user", "content": "read the build log"},
		map[string]any{"kind": "tool_call", "tool": "exec", "call_id": "c1", "args": map[string]any{"command": "tail build.log"}},
	}})
	if !strings.Contains(convoText(m), "read the build log") || strings.Contains(convoText(m), "resumed") {
		t.Fatalf("the child's history, without a resume marker: %s", convoText(m))
	}
	m.suggestions = []suggestion{{text: "the root's prediction"}}
	if m.suggestActive() {
		t.Error("the root's suggestions must not show in a child's view")
	}
	m = m.handleEvent(evMsg{"type": "agent_entry", "agent": 2.0, "items": []any{
		map[string]any{"kind": "assistant", "content": "another child's words"},
	}})
	m = m.handleEvent(evMsg{"type": "agent_entry", "agent": 1.0, "items": []any{
		map[string]any{"kind": "tool_result", "tool": "exec", "call_id": "c1", "content": "FAIL pkg/x"},
	}})
	m = m.handleEvent(evMsg{"type": "agent_entry", "agent": 1.0, "items": []any{
		map[string]any{"kind": "assistant", "content": "pkg/x fails first"},
	}})
	runs := 0
	for _, e := range m.convo {
		if e.run != nil {
			runs++
			if it := e.run.items[len(e.run.items)-1]; !it.done || it.result != "FAIL pkg/x" {
				t.Errorf("the result must land on its call: %+v", it)
			}
		}
	}
	if runs != 1 {
		t.Errorf("one call, one run: %d", runs)
	}
	text := convoText(m)
	if !strings.Contains(text, "fails") {
		t.Errorf("the live reply must show: %s", text)
	}
	if strings.Contains(text, "another") {
		t.Errorf("another child's entry pasted in: %s", text)
	}
}

// The root's events while a child is on screen go to the root's
// conversation, not the child's, and are there on the way back.
func TestAgentScope_RootEventsStayWithTheRoot(t *testing.T) {
	m, _ := scopeModel(t, 100)
	m = m.handleEvent(evMsg{"type": "user", "content": "the root task"})
	m.enterAgentScope(2)
	m = m.handleEvent(evMsg{"type": "agent_history", "agent": 2.0, "items": []any{
		map[string]any{"kind": "user", "content": "count matches"},
	}})
	m = m.handleEvent(evMsg{"type": "turn"})
	m = m.handleEvent(evMsg{"type": "token", "text": "rootreply arrives"})
	m = m.handleEvent(evMsg{"type": "done"})
	if strings.Contains(fullText(m), "rootreply") {
		t.Errorf("the root's reply landed in the child's view: %s", fullText(m))
	}
	m = key(m, kEsc)
	if m.scopeAgent != 0 {
		t.Fatal("esc must leave the scope")
	}
	if text := fullText(m); !strings.Contains(text, "rootreply") || !strings.Contains(text, "the root task") {
		t.Errorf("the root's conversation must hold what arrived meanwhile: %s", text)
	}
}

// esc puts the root back at the row the reader had scrolled to.
func TestAgentScope_EscRestoresTheRootScroll(t *testing.T) {
	m, buf := scopeModel(t, 100)
	for i := 0; i < 80; i++ {
		m.append(fmt.Sprintf("root line %d", i))
	}
	m = resized(m, 100, 24) // an update lays the strip out, as the app does
	m.scrollPinned = false
	m.vp.SetYOffset(17)
	rows := len(m.vp.lines)

	m.enterAgentScope(1)
	m = m.handleEvent(evMsg{"type": "agent_history", "agent": 1.0, "items": []any{
		map[string]any{"kind": "user", "content": "read the build log"},
		map[string]any{"kind": "assistant", "content": "137 failures"},
	}})
	if m.vp.YOffset == 17 && len(m.vp.lines) == rows {
		t.Fatal("the child's view is not on screen")
	}
	m = key(m, kEsc)
	if m.scopeAgent != 0 {
		t.Fatal("esc must leave the scope")
	}
	if m.vp.YOffset != 17 || len(m.vp.lines) != rows {
		t.Errorf("root view: offset %d (want 17), %d rows (want %d)", m.vp.YOffset, len(m.vp.lines), rows)
	}
	if got := sentAgentCmds(buf); len(got) != 2 || got[1] != "leave 1" {
		t.Errorf("leaving must end the watch: %q", got)
	}
}

// Typing while descended is the child's next message (a jobs tell), shown
// when its transcript takes it. A running child takes none: the draft stays.
func TestAgentScope_TypingTellsTheChild(t *testing.T) {
	m, buf := scopeModel(t, 100)
	m.enterAgentScope(2) // idle
	before := len(m.history)
	m = typeStr(m, "look at the last 40 lines instead")
	m = key(m, kEnter)
	if got := sentAgentCmds(buf); len(got) != 2 || got[1] != "tell 2 look at the last 40 lines instead" {
		t.Errorf("enter must tell the child: %q", got)
	}
	if len(m.history) != before+1 {
		t.Error("a message to a child should still be recallable")
	}
	if m.busy {
		t.Error("telling a child is not a turn of the session's own")
	}
	if strings.Contains(convoText(m), "last 40") {
		t.Errorf("echoed before the child took it: %s", convoText(m))
	}
	m = m.handleEvent(evMsg{"type": "agent_entry", "agent": 2.0, "items": []any{
		map[string]any{"kind": "user", "content": "look at the last 40 lines instead"},
	}})
	if !strings.Contains(convoText(m), "last 40") {
		t.Errorf("the child's entry must show the message: %s", convoText(m))
	}

	m.enterAgentScope(1) // running
	buf.Reset()
	m = typeStr(m, "stop that")
	m = key(m, kEnter)
	if got := sentAgentCmds(buf); len(got) != 0 {
		t.Errorf("a running child takes no tell: %q", got)
	}
	if m.input.Value() != "stop that" || !strings.Contains(m.flashText, "is working") {
		t.Errorf("the draft must stay, with why: %q / %q", m.input.Value(), m.flashText)
	}
}
