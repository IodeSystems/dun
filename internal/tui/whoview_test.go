package tui

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/iodesystems/dun/proto"
)

// /who opens a screen, not a block: this session by dund's name, its
// sub-agents under it (live from agents events), the other sessions and the
// listeners; typing filters, esc clears the filter and then closes.
func TestWhoScreen(t *testing.T) {
	asked := 0
	old := whoFetch
	t.Cleanup(func() { whoFetch = old })
	whoFetch = func() (*proto.MsgWhoList, error) {
		asked++
		return &proto.MsgWhoList{
			Sessions: []proto.PeerSession{
				{ID: "20261106-100000", Name: "dun", Workspace: "/src/dun", State: "busy", Attached: 1, Task: "build /who", Branch: "main"},
				{ID: "20261106-090000", Name: "raglit", Workspace: "/src/raglit", State: "idle", Task: "rebuild the index"},
			},
			Listeners: []string{"ci"},
		}, nil
	}
	m := newTUIModel(&dunProc{stdin: &recWC{}}, "/ws")
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = nm.(tuiModel)
	m.sessionID = "20261106-100000"
	nm, _ = m.Update(evMsg{"type": "agents", "agents": []any{
		map[string]any{"id": 1.0, "state": "running", "status": "reading the log", "prompt": "find the bug"},
		map[string]any{"id": 2.0, "state": "dismissed", "prompt": "write the tests"},
	}})
	m = nm.(tuiModel)
	before := len(m.convo)

	cmd := m.runSlash("/who")
	if m.activeScreen() == nil || m.whoPaging() == nil {
		t.Fatal("/who: no screen")
	}
	if len(m.convo) != before {
		t.Errorf("/who added %d blocks to the conversation", len(m.convo)-before)
	}
	view := func() string { return ansi.Strip(m.drawView()) }
	if v := view(); !strings.Contains(v, "asking dund") || !strings.Contains(v, "#1 running") {
		t.Fatalf("before dund answers:\n%s", v)
	}
	if cmd == nil {
		t.Fatal("/who did not ask dund")
	}
	nm, _ = m.Update(cmd())
	m = nm.(tuiModel)
	v := view()
	for _, want := range []string{"you are dun", "dun  (you)  busy", "├─ #1 running", "reading the log", "└─ #2 dismissed",
		"raglit  idle", "rebuild the index", "ci  listener", "2 sub-agents (1 live) · 1 other sessions · 1 listeners"} {
		if !strings.Contains(v, want) {
			t.Errorf("missing %q:\n%s", want, v)
		}
	}
	if lines := strings.Split(v, "\n"); len(lines) > 30 {
		t.Errorf("%d rows on a 30-row terminal", len(lines))
	}
	if strings.Index(v, "(you)") > strings.Index(v, "#1 running") || strings.Index(v, "#2 dismissed") > strings.Index(v, "raglit") {
		t.Errorf("not this session, its tree, then the others:\n%s", v)
	}

	// Live: an agents event redraws the tree.
	nm, _ = m.Update(evMsg{"type": "agents", "agents": []any{
		map[string]any{"id": 1.0, "state": "idle", "prompt": "find the bug"},
		map[string]any{"id": 2.0, "state": "dismissed", "prompt": "write the tests"},
		map[string]any{"id": 3.0, "state": "running", "prompt": "check the docs"},
	}})
	m = nm.(tuiModel)
	if v := view(); !strings.Contains(v, "├─ #1 idle") || !strings.Contains(v, "└─ #3 running") {
		t.Errorf("not redrawn after an agents event:\n%s", v)
	}

	key := func(k tea.KeyPressMsg) tea.Cmd {
		nm, cmd := m.Update(k)
		m = nm.(tuiModel)
		return cmd
	}
	typeText := func(s string) {
		for _, r := range s {
			key(tea.KeyPressMsg{Code: r, Text: string(r)})
		}
	}
	// A filter: q and j are letters here, not the pager's keys.
	typeText("rag")
	if v := view(); !strings.Contains(v, "raglit  idle") || strings.Contains(v, "#1 idle") || strings.Contains(v, "ci  listener") {
		t.Errorf("filter rag:\n%s", v)
	}
	if m.whoPaging() == nil {
		t.Fatal("typing closed the screen")
	}
	// A sub-agent that matches keeps this session above it.
	key(tea.KeyPressMsg{Code: tea.KeyEscape})
	typeText("docs")
	if v := view(); !strings.Contains(v, "(you)") || !strings.Contains(v, "#3 running") || strings.Contains(v, "#1 idle") || strings.Contains(v, "raglit") {
		t.Errorf("filter docs:\n%s", v)
	}
	key(tea.KeyPressMsg{Code: tea.KeyEscape})
	// Space adds a second word: both must be in the row.
	typeText("find")
	key(tea.KeyPressMsg{Code: tea.KeySpace, Text: " "})
	typeText("idle")
	if v := view(); !strings.Contains(v, "#1 idle") || strings.Contains(v, "#3 running") {
		t.Errorf("filter %q:\n%s", m.whoPaging().query, v)
	}
	key(tea.KeyPressMsg{Code: tea.KeyEscape})
	if v := m.whoPaging(); v == nil || v.query != "" {
		t.Fatal("esc with a filter: want it cleared and the screen up")
	}

	// ctrl+r asks dund again.
	if c := key(tea.KeyPressMsg{Code: 'r', Mod: tea.ModCtrl}); c == nil {
		t.Error("ctrl+r did not ask dund again")
	} else {
		nm, _ = m.Update(c())
		m = nm.(tuiModel)
	}
	if asked != 2 {
		t.Errorf("dund asked %d times, want 2", asked)
	}

	key(tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.activeScreen() != nil {
		t.Error("esc left the screen up")
	}
}

// With no dund, /who still names this session (by its id) and its
// sub-agents, and says why the others are missing.
func TestWhoNoDund(t *testing.T) {
	old := whoFetch
	t.Cleanup(func() { whoFetch = old })
	whoFetch = func() (*proto.MsgWhoList, error) { return nil, errors.New("connection refused") }
	m := newTUIModel(&dunProc{stdin: &recWC{}}, "/ws")
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 60, Height: 20})
	m = nm.(tuiModel)
	m.sessionID = "20261106-100000"
	cmd := m.runSlash("/who")
	nm, _ = m.Update(cmd())
	m = nm.(tuiModel)
	v := ansi.Strip(m.drawView())
	for _, want := range []string{"you are 20261106-100000", "connection refused", "not listed"} {
		if !strings.Contains(v, want) {
			t.Errorf("missing %q:\n%s", want, v)
		}
	}
}
