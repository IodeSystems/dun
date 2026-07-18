package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// discardWC is a stand-in engine stdin: answers/sends go nowhere.
type discardWC struct{}

func (discardWC) Write(p []byte) (int, error) { return len(p), nil }
func (discardWC) Close() error                { return nil }

func key(m tuiModel, k tea.KeyMsg) tuiModel {
	nm, _ := m.Update(k)
	return nm.(tuiModel)
}

var (
	kTab   = tea.KeyMsg{Type: tea.KeyTab}
	kUp    = tea.KeyMsg{Type: tea.KeyUp}
	kDown  = tea.KeyMsg{Type: tea.KeyDown}
	kEnter = tea.KeyMsg{Type: tea.KeyEnter}
	kN     = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")}
	kSlash = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")}
	kEsc   = tea.KeyMsg{Type: tea.KeyEsc}
)

func typeStr(m tuiModel, s string) tuiModel {
	for _, r := range s {
		m = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	return m
}

// TestTUI_EventHandling drives the model's event logic directly (no terminal):
// the ready→token→tool_call→done sequence must build the conversation and clear
// the busy/starting flags correctly.
func TestTUI_EventHandling(t *testing.T) {
	m := newTUIModel(&dunProc{}, "/ws")
	if !m.starting {
		t.Fatal("model should start in the 'starting' state")
	}

	m = m.handleEvent(evMsg{"type": "ready", "tools": []any{"node_query", "eval", "search"}})
	if m.starting {
		t.Fatal("ready should clear starting")
	}
	if len(m.tools) != 3 {
		t.Fatalf("tools not captured: %v", m.tools)
	}

	// A turn: streamed tokens accumulate; a tool call flushes the streamed text.
	m.busy = true
	m = m.handleEvent(evMsg{"type": "token", "text": "look"})
	m = m.handleEvent(evMsg{"type": "token", "text": "ing…"})
	if m.cur != "looking…" {
		t.Fatalf("tokens should accumulate into cur, got %q", m.cur)
	}
	m = m.handleEvent(evMsg{"type": "tool_call", "tool": "node_query", "args": map[string]any{"selector": "x"}})
	if m.cur != "" {
		t.Fatal("tool_call should flush the streamed text")
	}
	joined := m.convoText()
	if !strings.Contains(joined, "looking…") || !strings.Contains(joined, "node_query") {
		t.Fatalf("conversation missing streamed text or tool line: %q", joined)
	}

	m = m.handleEvent(evMsg{"type": "token", "text": "done reading"})
	m = m.handleEvent(evMsg{"type": "done"})
	if m.busy {
		t.Fatal("done should clear busy")
	}
	if m.cur != "" {
		t.Fatal("done should flush cur")
	}
	if !strings.Contains(m.convoText(), "done reading") {
		t.Fatal("final streamed text not finalized")
	}
}

// Tab toggles pane focus; in convo focus ↑/↓ move the message selection.
func TestTUI_FocusToggleAndSelection(t *testing.T) {
	m := newTUIModel(&dunProc{}, "/ws")
	m.convo = []convoEntry{{collapsed: "m1"}, {collapsed: "m2"}, {collapsed: "m3"}}

	m = key(m, kTab)
	if m.focus != focusConvo {
		t.Fatal("tab should focus the conversation")
	}
	if m.sel != 2 {
		t.Fatalf("focusing convo should select the last message, got %d", m.sel)
	}
	m = key(m, kUp)
	m = key(m, kUp)
	if m.sel != 0 {
		t.Fatalf("↑ should move selection up, got %d", m.sel)
	}
	m = key(m, kUp) // clamps at 0
	if m.sel != 0 {
		t.Fatalf("selection should clamp at 0, got %d", m.sel)
	}
	m = key(m, kDown)
	if m.sel != 1 {
		t.Fatalf("↓ should move selection down, got %d", m.sel)
	}
	m = key(m, kTab)
	if m.focus != focusInput {
		t.Fatal("tab should return focus to the input")
	}
}

// The ask picker: ↑/↓ choose an option, `n` attaches a detail, enter sends
// "<option> — <detail>".
func TestTUI_AskPickerOptionWithNote(t *testing.T) {
	proc := &dunProc{stdin: discardWC{}}
	m := newTUIModel(proc, "/ws")
	m = m.handleEvent(evMsg{"type": "ask", "question": "Which?", "options": []any{"A", "B"}})
	if !m.asking || len(m.askOptions) != 2 || m.askSel != 0 {
		t.Fatalf("ask not set up: asking=%v opts=%v sel=%d", m.asking, m.askOptions, m.askSel)
	}
	m = key(m, kDown) // select "B"
	if m.askSel != 1 {
		t.Fatalf("↓ should select option 1, got %d", m.askSel)
	}
	m = key(m, kN) // start a detail
	if !m.noting {
		t.Fatal("n should start a detail")
	}
	m.input.SetValue("fast")
	m = key(m, kEnter) // confirm the detail
	if m.noting || m.askNote != "fast" {
		t.Fatalf("detail not captured: noting=%v note=%q", m.noting, m.askNote)
	}
	m = key(m, kEnter) // send the option
	if m.asking {
		t.Fatal("selecting an option should end asking")
	}
	if !strings.Contains(m.convoText(), "B — fast") {
		t.Fatalf("answer not echoed with detail: %v", m.convo)
	}
}

// The custom/chat row lets you type a free-text answer.
func TestTUI_AskPickerCustomAnswer(t *testing.T) {
	proc := &dunProc{stdin: discardWC{}}
	m := newTUIModel(proc, "/ws")
	m = m.handleEvent(evMsg{"type": "ask", "question": "Which?", "options": []any{"A", "B"}})
	m = key(m, kDown)
	m = key(m, kDown) // move onto the custom row (index == len(options))
	if m.askSel != 2 {
		t.Fatalf("expected custom row selected, got %d", m.askSel)
	}
	m = key(m, kEnter) // open free-text entry
	if !m.customAnswer {
		t.Fatal("enter on the custom row should open free-text entry")
	}
	m.input.SetValue("let's chat about X")
	m = key(m, kEnter)
	if m.asking {
		t.Fatal("sending a custom answer should end asking")
	}
	if !strings.Contains(m.convoText(), "let's chat about X") {
		t.Fatalf("custom answer not echoed: %v", m.convo)
	}
}

// A tool call folds its result into one collapsible block; focusing it and
// pressing enter toggles the full output.
func TestTUI_ToolCallExpandCollapse(t *testing.T) {
	m := newTUIModel(&dunProc{}, "/ws")
	m = m.handleEvent(evMsg{"type": "tool_call", "tool": "node_read", "args": map[string]any{"sel": "F"}})
	full := "line one\nline two\nline three that is quite long and would be clipped in the preview form"
	m = m.handleEvent(evMsg{"type": "tool_result", "result": full})

	if len(m.convo) != 1 {
		t.Fatalf("call+result should be one block, got %d", len(m.convo))
	}
	e := m.convo[0]
	if !e.expandable() {
		t.Fatal("a tool block should be expandable")
	}
	// Collapsed = call line + one preview line; open = call + full body.
	if got := strings.Count(e.view(), "\n"); got != 1 {
		t.Fatalf("collapsed view should be 2 lines (call + preview), got %d newlines", got)
	}
	// The call line shows the INPUT value, not just the arg key.
	if !strings.Contains(e.view(), "sel=F") {
		t.Fatalf("collapsed call line should show the arg value, got %q", e.view())
	}

	// Focus it and open.
	m = key(m, kTab)
	if m.sel != 0 {
		t.Fatalf("focus should land on the block, sel=%d", m.sel)
	}
	m = key(m, kEnter)
	if !m.convo[0].open {
		t.Fatal("enter should open the block")
	}
	if !strings.Contains(m.convo[0].view(), "line three") {
		t.Fatal("open view should show the full output")
	}
	m = key(m, kEnter)
	if m.convo[0].open {
		t.Fatal("enter again should close the block")
	}
}

// vim-style "/" search: type a query, matches drive the selection, ↑/↓ step
// between them, esc exits match mode.
func TestTUI_SlashSearch(t *testing.T) {
	m := newTUIModel(&dunProc{}, "/ws")
	m.convo = []convoEntry{
		{collapsed: "apple pie"},
		{collapsed: "banana split"},
		{collapsed: "apple tart"},
		{collapsed: "cherry"},
	}
	m = key(m, kTab) // convo focus
	m = key(m, kSlash)
	if !m.searching {
		t.Fatal("/ should start search")
	}
	m = typeStr(m, "apple")
	if len(m.matches) != 2 || m.matches[0] != 0 || m.matches[1] != 2 {
		t.Fatalf("apple should match blocks 0 and 2, got %v", m.matches)
	}
	if m.sel != 0 {
		t.Fatalf("live search should preview the first match, sel=%d", m.sel)
	}
	m = key(m, kEnter) // commit → navigate mode
	if m.searching || !m.searchActive {
		t.Fatalf("enter should commit to match-scroll: searching=%v active=%v", m.searching, m.searchActive)
	}
	m = key(m, kDown) // next match
	if m.matchPos != 1 || m.sel != 2 {
		t.Fatalf("↓ should step to match 2 (block 2), pos=%d sel=%d", m.matchPos, m.sel)
	}
	m = key(m, kDown) // clamp at last match
	if m.matchPos != 1 {
		t.Fatalf("should clamp at last match, pos=%d", m.matchPos)
	}
	m = key(m, kUp)
	if m.matchPos != 0 || m.sel != 0 {
		t.Fatalf("↑ should step back to match 0, pos=%d sel=%d", m.matchPos, m.sel)
	}
	m = key(m, kEsc) // exit match mode
	if m.searchActive || m.matches != nil {
		t.Fatal("esc should exit match-scroll mode")
	}
	if m.focus != focusConvo {
		t.Fatal("esc from search should stay in convo focus, not quit")
	}
}

// A relevant-docs notification renders as a collapsible summary; →/↑/↓ navigate
// the nested doc list.
func TestTUI_DocsNotificationNav(t *testing.T) {
	m := newTUIModel(&dunProc{}, "/ws")
	m = m.handleEvent(evMsg{
		"type": "notification", "kind": "docs", "found": float64(5), "surfaced": float64(2),
		"docs": []any{
			map[string]any{"title": "README", "line": "intro", "score": float64(1.2)},
			map[string]any{"title": "ARCH", "line": "layout", "score": float64(0.8)},
		},
	})
	if len(m.convo) != 1 || m.convo[0].docs == nil {
		t.Fatalf("docs notification should be one docsBlock entry")
	}
	d := m.convo[0].docs
	if d.found != 5 || d.surfaced != 2 || len(d.docs) != 2 {
		t.Fatalf("docs block counts wrong: found=%d surfaced=%d n=%d", d.found, d.surfaced, len(d.docs))
	}
	if strings.Contains(m.convo[0].view(), "README") {
		t.Fatal("collapsed docs summary should not list docs")
	}

	m = key(m, kTab) // focus the summary
	kRight := tea.KeyMsg{Type: tea.KeyRight}
	kLeft := tea.KeyMsg{Type: tea.KeyLeft}

	m = key(m, kRight) // can't descend until opened
	if m.convo[0].docs.descended {
		t.Fatal("→ should not descend a collapsed summary")
	}
	m = key(m, kEnter) // open
	if !m.convo[0].open || !strings.Contains(m.convo[0].view(), "README") {
		t.Fatal("enter should open the summary and list docs")
	}
	m = key(m, kRight) // descend
	if !m.convo[0].docs.descended || m.convo[0].docs.cur != 0 {
		t.Fatalf("→ should descend to doc 0, descended=%v cur=%d", m.convo[0].docs.descended, m.convo[0].docs.cur)
	}
	m = key(m, kDown) // next doc
	if m.convo[0].docs.cur != 1 {
		t.Fatalf("↓ should move to doc 1, got %d", m.convo[0].docs.cur)
	}
	m = key(m, kEnter) // expand doc 1's snippet
	if !m.convo[0].docs.docs[1].open || !strings.Contains(m.convo[0].view(), "layout") {
		t.Fatal("enter should expand the current doc's snippet")
	}
	m = key(m, kLeft) // ascend
	if m.convo[0].docs.descended {
		t.Fatal("← should ascend out of the doc list")
	}
}

func TestTUI_ErrorEventClearsBusy(t *testing.T) {
	m := newTUIModel(&dunProc{}, "/ws")
	m.busy = true
	m = m.handleEvent(evMsg{"type": "error", "error": "boom"})
	if m.busy {
		t.Fatal("error should clear busy")
	}
	if !strings.Contains(m.convoText(), "boom") {
		t.Fatal("error text not shown")
	}
}
