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
)

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
	joined := strings.Join(m.convo, "\n")
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
	if !strings.Contains(strings.Join(m.convo, "\n"), "done reading") {
		t.Fatal("final streamed text not finalized")
	}
}

// Tab toggles pane focus; in convo focus ↑/↓ move the message selection.
func TestTUI_FocusToggleAndSelection(t *testing.T) {
	m := newTUIModel(&dunProc{}, "/ws")
	m.convo = []string{"m1", "m2", "m3"}

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
	if !strings.Contains(strings.Join(m.convo, "\n"), "B — fast") {
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
	if !strings.Contains(strings.Join(m.convo, "\n"), "let's chat about X") {
		t.Fatalf("custom answer not echoed: %v", m.convo)
	}
}

func TestTUI_ErrorEventClearsBusy(t *testing.T) {
	m := newTUIModel(&dunProc{}, "/ws")
	m.busy = true
	m = m.handleEvent(evMsg{"type": "error", "error": "boom"})
	if m.busy {
		t.Fatal("error should clear busy")
	}
	if !strings.Contains(strings.Join(m.convo, "\n"), "boom") {
		t.Fatal("error text not shown")
	}
}
