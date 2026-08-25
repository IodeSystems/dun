package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/cellbuf"
)

// discardWC is a stand-in engine stdin: answers/sends go nowhere.
type discardWC struct{}

func (discardWC) Write(p []byte) (int, error) { return len(p), nil }
func (discardWC) Close() error                { return nil }

// resized puts the model through a real WindowSizeMsg. Setting m.w/m.h by hand
// leaves the conversation pane at width 0, which wraps every block to a single
// column — a state the app never reaches (test helper).
func resized(m tuiModel, w, h int) tuiModel {
	nm, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	return nm.(tuiModel)
}

// convoText joins the visible text of every block (test helper).
func convoText(m tuiModel) string {
	parts := make([]string, len(m.convo))
	for i, e := range m.convo {
		parts[i] = e.view()
	}
	return strings.Join(parts, "\n")
}

// fullText returns all conversation blocks plus the streaming cursor text
// (markdown-rendered), joined by newlines (test helper).
func fullText(m tuiModel) string {
	blocks := make([]string, 0, len(m.convo)+1)
	for _, e := range m.convo {
		blocks = append(blocks, e.view())
	}
	if m.cur != "" {
		blocks = append(blocks, renderMarkdown(m.md, m.cur))
	}
	return strings.Join(blocks, "\n")
}

// bufCloser wraps an io.Writer as an io.WriteCloser (Close is a no-op).
type bufCloser struct{ io.Writer }

func (bufCloser) Close() error { return nil }

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
	kRight = tea.KeyMsg{Type: tea.KeyRight}
)

func typeStr(m tuiModel, s string) tuiModel {
	for _, r := range s {
		m = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	return m
}

// vtui is a virtual TUI for testing rendering without a real terminal.
// It lets you set dimensions (e.g. phone size 40x15), feed events and keys,
// and read back the rendered output as plain text (ANSI stripped).
type vtui struct {
	m    tuiModel
	w, h int
}

// newVtui creates a virtual TUI at the given dimensions.
// Pass w=0, h=0 for defaults (80x24).
func newVtui(w, h int) *vtui {
	if w == 0 {
		w = 80
	}
	if h == 0 {
		h = 24
	}
	m := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	v := &vtui{m: m, w: w, h: h}
	v.resize(w, h)
	return v
}

// resize sets the terminal dimensions and triggers a layout refresh.
func (v *vtui) resize(w, h int) {
	v.w, v.h = w, h
	nm, _ := v.m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	v.m = nm.(tuiModel)
}

// event feeds a JSON event (as map) into the model through Update — the same
// door the engine's events come in by. Calling handleEvent directly skips the
// per-message layout sync that Update does, which is how a frame that overdrew
// the terminal by one row stayed invisible to these tests.
func (v *vtui) event(ev map[string]any) {
	nm, _ := v.m.Update(evMsg(ev))
	v.m = nm.(tuiModel)
}

// send simulates the user typing a message and pressing enter.
func (v *vtui) send(msg string) {
	v.m = typeStr(v.m, msg)
	v.m = key(v.m, kEnter)
}

// view returns the full rendered output with ANSI codes stripped.
func (v *vtui) view() string {
	return stripANSI(v.m.View())
}

// convo returns the conversation text (block views joined by newlines, ANSI stripped).
func (v *vtui) convo() string {
	return stripANSI(convoText(v.m))
}

// model returns the underlying tuiModel for direct inspection.
func (v *vtui) model() tuiModel {
	return v.m
}

// setScrollPin directly controls the scroll pin state (test helper).
func (v *vtui) setScrollPin(pinned bool) {
	v.m.scrollPinned = pinned
}

// setYOffset directly sets the viewport scroll offset (test helper).
func (v *vtui) setYOffset(y int) {
	v.m.vp.SetYOffset(y)
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
	joined := convoText(m)
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
	if !strings.Contains(convoText(m), "done reading") {
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
	if !strings.Contains(convoText(m), "B — fast") {
		t.Fatalf("answer not echoed with detail: %v", m.convo)
	}
}

// A no-options ask (free-text question) drops STRAIGHT into text entry: the
// user can type immediately without first pressing enter on a "custom" row.
// Regression for a session where the model asked with no options and typing an
// answer was inert.
func TestTUI_AskNoOptionsFreeText(t *testing.T) {
	proc := &dunProc{stdin: discardWC{}}
	m := newTUIModel(proc, "/ws")
	m = m.handleEvent(evMsg{"type": "ask", "question": "What is your favorite color?", "options": nil})
	if !m.asking || len(m.askOptions) != 0 {
		t.Fatalf("expected a no-options ask, got opts=%v", m.askOptions)
	}
	if !m.customAnswer {
		t.Fatal("no-options ask should enter free-text mode immediately")
	}
	// Type without pressing enter first — this was the bug (inert typing).
	m = typeStr(m, "blue")
	if m.input.Value() != "blue" {
		t.Fatalf("typing should reach the input, got %q", m.input.Value())
	}
	m = key(m, kEnter)
	if m.asking {
		t.Fatal("enter should send the free-text answer")
	}
	if !strings.Contains(convoText(m), "blue") {
		t.Fatalf("answer not echoed: %v", m.convo)
	}
}

// multi:true → enter toggles the highlighted option (space is a typed char, not
// a toggle), and a trailing "✓ done" row submits the joined set.
func TestTUI_AskMultiSelect(t *testing.T) {
	proc := &dunProc{stdin: discardWC{}}
	m := newTUIModel(proc, "/ws")
	m = m.handleEvent(evMsg{"type": "ask", "question": "Which?", "options": []any{"A", "B", "C"}, "multi": true})
	if !m.askMulti || len(m.askChecked) != 3 {
		t.Fatalf("multi ask not set up: multi=%v checked=%v", m.askMulti, m.askChecked)
	}
	m = key(m, kEnter) // toggle A (row 0)
	if !m.askChecked[0] {
		t.Fatal("enter should toggle the highlighted option in multi mode")
	}
	m = key(m, kDown)
	m = key(m, kDown)
	m = key(m, kEnter) // toggle C (row 2)
	if !m.askChecked[2] || m.askChecked[1] {
		t.Fatalf("expected A+C checked, got %v", m.askChecked)
	}
	// Navigate past the custom row to the "✓ done" row (custom+1 = 4).
	m = key(m, kDown) // custom row (3)
	m = key(m, kDown) // done row (4)
	if m.askSel != 4 {
		t.Fatalf("should be on the done row, sel=%d", m.askSel)
	}
	m = key(m, kEnter) // submit
	if m.asking {
		t.Fatal("enter on the done row should submit")
	}
	if !strings.Contains(convoText(m), "A, C") {
		t.Fatalf("joined answer missing: %v", m.convo)
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
	if !strings.Contains(convoText(m), "let's chat about X") {
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

	// Focus it and open → the tool inspector overlay (not inline expansion).
	m = key(m, kTab)
	if m.sel != 0 {
		t.Fatalf("focus should land on the block, sel=%d", m.sel)
	}
	m = key(m, kEnter)
	if !m.inspecting {
		t.Fatal("enter on a tool block should open the inspector")
	}
	if !strings.Contains(m.insp.panes[inspOutput].src, "line three") {
		t.Fatalf("inspector output should hold the full result, got %q", m.insp.panes[inspOutput].src)
	}
	m = key(m, kEsc)
	if m.inspecting {
		t.Fatal("esc should close the inspector")
	}
}

// A `history` event (a resumed session) replays the loaded conversation as
// scrollback: user echo, assistant markdown, a FOLDED tool call+result block,
// and a notification — plus a trailing "resumed N entries" marker.
func TestTUI_HistoryReplay(t *testing.T) {
	m := newTUIModel(&dunProc{}, "/ws")
	m = m.handleEvent(evMsg{"type": "history", "items": []any{
		map[string]any{"kind": "user", "content": "fix the bug"},
		map[string]any{"kind": "assistant", "content": "on it"},
		map[string]any{"kind": "tool_call", "tool": "node_read", "call_id": "c1", "args": map[string]any{"sel": "F"}},
		map[string]any{"kind": "tool_result", "tool": "node_read", "call_id": "c1", "content": "func F() {}\nreturn"},
		map[string]any{"kind": "notification", "content": "background job #1 finished"},
	}})

	// 4 rendered items (call+result fold to one) + the resumed marker = 5 blocks.
	if len(m.convo) != 5 {
		t.Fatalf("expected 5 blocks (4 items folded + marker), got %d", len(m.convo))
	}
	txt := convoText(m)
	for _, want := range []string{"fix the bug", "on it", "node_read", "sel=F", "background job #1", "resumed 4 entries"} {
		if !strings.Contains(txt, want) {
			t.Fatalf("replayed scrollback missing %q in:\n%s", want, txt)
		}
	}
	// The tool call+result is one expandable, inspector-backed block.
	tb := m.convo[2]
	if !tb.expandable() || tb.tool == nil {
		t.Fatal("replayed tool block should be expandable with an inspector toolBlock")
	}
	if !strings.Contains(tb.tool.output, "func F()") {
		t.Fatalf("replayed tool block should hold the full result, got %q", tb.tool.output)
	}
	// A stray tool_result with no matching call renders standalone (no panic, no args).
	m2 := newTUIModel(&dunProc{}, "/ws")
	m2 = m2.handleEvent(evMsg{"type": "history", "items": []any{
		map[string]any{"kind": "tool_result", "tool": "eval", "call_id": "x", "content": "42"},
	}})
	if len(m2.convo) != 2 { // the block + marker
		t.Fatalf("unmatched tool_result should still render one block + marker, got %d", len(m2.convo))
	}
}

// Typing "/" opens the command palette: it lists/filters commands, tab
// completes the highlighted one, and /help enumerates them.
func TestTUI_CommandPalette(t *testing.T) {
	m := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = nm.(tuiModel)

	m = typeStr(m, "/")
	if !m.paletteActive() {
		t.Fatal("typing / should open the command palette")
	}
	if len(m.paletteMatches()) != len(slashCommands) {
		t.Fatalf("bare / should list all %d commands, got %d", len(slashCommands), len(m.paletteMatches()))
	}
	// Filter down to /config.
	m = typeStr(m, "conf")
	if ms := m.paletteMatches(); len(ms) != 1 || ms[0].name != "config" {
		t.Fatalf("/conf should match only config, got %v", ms)
	}
	// Tab completes to the highlighted command.
	m = key(m, kTab)
	if m.input.Value() != "/config " {
		t.Fatalf("tab should complete to %q, got %q", "/config ", m.input.Value())
	}
	// esc dismisses the palette without quitting.
	m = key(m, kEsc)
	if m.paletteActive() || m.input.Value() != "" {
		t.Fatalf("esc should clear the palette, value=%q", m.input.Value())
	}

	// /help enumerates the commands into the conversation.
	m = typeStr(m, "/help")
	m = key(m, kEnter)
	txt := convoText(m)
	if !strings.Contains(txt, "commands") || !strings.Contains(txt, "/config") || !strings.Contains(txt, "/exit") {
		t.Fatalf("/help should list the commands, got: %s", txt)
	}
}

// --suggest: a suggestions event shows the idle picker; a digit sends that
// suggestion; a new turn (token) clears it.
func TestTUI_Suggestions(t *testing.T) {
	m := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = nm.(tuiModel)
	m = m.handleEvent(evMsg{"type": "ready", "tools": []any{"eval"}})
	m = m.handleEvent(evMsg{"type": "suggestions", "items": []any{
		map[string]any{"text": "run the tests", "prob": 0.7},
		map[string]any{"text": "commit it", "prob": 0.2},
	}})
	if len(m.suggestions) != 2 {
		t.Fatalf("suggestions not stored: %+v", m.suggestions)
	}
	if !m.suggestActive() {
		t.Fatal("picker should be active when idle + empty input")
	}
	// digit "1" fills the input with the first suggestion (does not send).
	m = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("1")})
	if m.input.Value() != "run the tests" {
		t.Fatalf("digit 1 should fill input with first suggestion, got: %q", m.input.Value())
	}
	if len(m.suggestions) != 0 {
		t.Fatal("picking a suggestion should clear the list")
	}

	// A new turn's token clears any showing suggestions.
	m = m.handleEvent(evMsg{"type": "suggestions", "items": []any{map[string]any{"text": "x", "prob": 0.5}}})
	m = m.handleEvent(evMsg{"type": "token", "text": "thinking"})
	if len(m.suggestions) != 0 {
		t.Fatal("a new turn should clear suggestions")
	}
}

// Horizontal arrow axis: left at input-front → convo; right from a plain convo
// message → input; right from an empty input → suggestion selector (left closes).
func TestTUI_ArrowNav(t *testing.T) {
	kLeft := tea.KeyMsg{Type: tea.KeyLeft}
	kRight := tea.KeyMsg{Type: tea.KeyRight}
	m := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = nm.(tuiModel)
	m = m.handleEvent(evMsg{"type": "ready", "tools": []any{"eval"}})
	m.append("a reply") // a plain convo message to land on

	// left at the front of an (empty) input hops to the conversation.
	m = key(m, kLeft)
	if m.focus != focusConvo {
		t.Fatal("left at input front should focus the conversation")
	}
	// right from a plain message (no sub-selection) hops back to the input.
	m = key(m, kRight)
	if m.focus != focusInput {
		t.Fatal("right from a plain convo message should focus the input")
	}
	// right from an empty input accepts the ghost-text suggestion (fills the buffer).
	m = m.handleEvent(evMsg{"type": "suggestions", "items": []any{
		map[string]any{"text": "alpha", "prob": 0.6},
		map[string]any{"text": "bravo", "prob": 0.4},
	}})
	// up/down cycle through suggestions (ghost text changes).
	m = key(m, kDown)
	if m.suggestSel != 1 {
		t.Fatalf("↓ should move the selection, got %d", m.suggestSel)
	}
	// right accepts the ghost text (fills the input buffer).
	m = key(m, kRight)
	if m.input.Value() != "bravo" {
		t.Fatalf("right should accept ghost text, got %q", m.input.Value())
	}
	// enter sends the highlighted suggestion (when input is empty).
	m = m.handleEvent(evMsg{"type": "suggestions", "items": []any{
		map[string]any{"text": "alpha", "prob": 0.6},
		map[string]any{"text": "bravo", "prob": 0.4},
	}})
	m = key(m, kDown) // select "bravo"
	m = key(m, kEnter)
	if !strings.Contains(convoText(m), "bravo") {
		t.Fatalf("enter should send the selected suggestion, convo: %s", convoText(m))
	}
}

// --disable-exit: ctrl+c and esc don't quit, but /quit still does.
func TestTUI_DisableExit(t *testing.T) {
	kCtrlC := tea.KeyMsg{Type: tea.KeyCtrlC}
	m := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	m.disableExit = true
	if _, cmd := m.Update(kCtrlC); cmd != nil {
		t.Fatal("ctrl+c should be ignored when disableExit")
	}
	if _, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc}); cmd != nil {
		t.Fatal("esc should be ignored when disableExit")
	}
	// /exit is a deliberate exit — still works.
	q := typeStr(m, "/exit")
	if _, cmd := q.Update(kEnter); cmd == nil {
		t.Fatal("/exit should exit even with disableExit")
	}
	// Bare "exit" (no slash) also exits silently.
	e := typeStr(m, "exit")
	if _, cmd := e.Update(kEnter); cmd == nil {
		t.Fatal("bare exit should exit even with disableExit")
	}
	// Control: exit enabled → ctrl+c quits.
	m2 := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	if _, cmd := m2.Update(kCtrlC); cmd == nil {
		t.Fatal("ctrl+c should quit when exit is enabled")
	}
}

// An unknown slash command is reported, not sent to the engine.
func TestTUI_UnknownSlash(t *testing.T) {
	m := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	m = typeStr(m, "/bogus")
	m = key(m, kEnter)
	if !strings.Contains(convoText(m), "unknown command") {
		t.Fatalf("expected unknown-command note, got: %s", convoText(m))
	}
}

// /clear clears the scrollback and sends a reset to the engine.
func TestTUI_Clear(t *testing.T) {
	var buf strings.Builder
	m := newTUIModel(&dunProc{stdin: &bufCloser{Writer: &buf}}, "/ws")
	// Seed some conversation content.
	m.convo = append(m.convo, convoEntry{collapsed: "some tool call"})
	m.cur = "partial assistant reply"
	m.busy = true
	m.pendingTool = 0

	m = typeStr(m, "/clear")
	m = key(m, kEnter)

	// Scrollback should be cleared.
	if len(m.convo) > 1 { // only the "session cleared" note remains
		t.Fatalf("convo not cleared: %d entries", len(m.convo))
	}
	if m.cur != "" {
		t.Fatal("cur not cleared")
	}
	if m.busy {
		t.Fatal("busy not cleared")
	}
	if m.pendingTool != -1 {
		t.Fatal("pendingTool not cleared")
	}
	if !strings.Contains(convoText(m), "session cleared") {
		t.Fatalf("expected clear confirmation, got: %s", convoText(m))
	}

	// Engine should have received a reset message.
	if !strings.Contains(buf.String(), `"type":"reset"`) {
		t.Fatalf("engine did not receive reset, got: %s", buf.String())
	}
}

// SIGUSR1 → dumpMsg writes the rendered screen + state header to $DUN_DUMP_FILE.
func TestTUI_ScreenDump(t *testing.T) {
	path := t.TempDir() + "/dump.txt"
	t.Setenv("DUN_DUMP_FILE", path)

	m := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = nm.(tuiModel)
	m = m.handleEvent(evMsg{"type": "ready", "tools": []any{"eval"}})
	m.busy = true

	nm, _ = m.Update(dumpMsg{}) // what SIGUSR1 delivers
	m = nm.(tuiModel)

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("dump file not written: %v", err)
	}
	dump := string(b)
	if !strings.Contains(dump, "busy=true") || !strings.Contains(dump, "w=80 h=24") {
		t.Fatalf("state header missing/incomplete:\n%s", dump)
	}
	if !strings.Contains(dump, "1 tools") {
		t.Fatalf("rendered screen not captured:\n%s", dump)
	}
	// A second dump appends (history of snapshots).
	m.Update(dumpMsg{})
	b2, _ := os.ReadFile(path)
	if strings.Count(string(b2), "dun screen @") < 2 {
		t.Fatal("second dump should append, not overwrite")
	}
}

// The inspector overlay: enter opens it, tab switches the focused frame, "/"
// search finds and n cycles matches, esc closes.
func TestTUI_Inspector(t *testing.T) {
	m := newTUIModel(&dunProc{}, "/ws")
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = nm.(tuiModel)
	m = m.handleEvent(evMsg{"type": "ready", "tools": []any{"eval"}})
	m.busy = true
	m = m.handleEvent(evMsg{"type": "tool_call", "tool": "eval", "args": map[string]any{"code": "print(x)"}})
	m = m.handleEvent(evMsg{"type": "tool_result", "tool": "eval", "result": "alpha\nbravo\ncharlie\nbravo again"})

	m = key(m, kTab)   // focus the block
	m = key(m, kEnter) // open inspector
	if !m.inspecting {
		t.Fatal("inspector should be open")
	}
	if m.insp.focus != inspOutput {
		t.Fatalf("output frame should start focused, got %d", m.insp.focus)
	}
	// The overlay renders both frames without panic.
	if v := stripANSI(m.View()); !strings.Contains(v, "eval") || !strings.Contains(v, "output") || !strings.Contains(v, "charlie") {
		t.Fatalf("inspector view missing tool/frame/content: %q", v)
	}
	// tab switches to the input frame; its source is the call's args.
	m = key(m, kTab)
	if m.insp.focus != inspInput {
		t.Fatalf("tab should focus the input frame, got %d", m.insp.focus)
	}
	if !strings.Contains(m.insp.panes[inspInput].src, "print(x)") {
		t.Fatalf("input frame should show the args, got %q", m.insp.panes[inspInput].src)
	}
	m = key(m, kTab) // back to output

	// "/bravo" → two matches in the output frame.
	m = key(m, kSlash)
	m = typeStr(m, "bravo")
	m = key(m, kEnter)
	if len(m.insp.matches) != 2 {
		t.Fatalf("expected 2 matches for 'bravo', got %d", len(m.insp.matches))
	}
	if m.insp.at != 0 {
		t.Fatalf("first match should be selected, at=%d", m.insp.at)
	}
	m = key(m, kN) // next match
	if m.insp.at != 1 {
		t.Fatalf("n should advance to the second match, at=%d", m.insp.at)
	}
	m = key(m, kEsc)
	if m.inspecting {
		t.Fatal("esc should close the inspector")
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

	// → is one level per press, all the way down: open the summary, descend into
	// the list, open a document. It used to take enter to get past the first two.
	m = key(m, kRight) // open the summary
	if m.convo[0].state <= viewMinimized || !strings.Contains(m.convo[0].view(), "README") {
		t.Fatal("→ should open the summary and list docs")
	}
	if m.convo[0].docs.descended {
		t.Fatal("→ opens the summary first; descending is the NEXT press")
	}
	m = key(m, kRight) // descend
	if !m.convo[0].docs.descended || m.convo[0].docs.cur != 0 {
		t.Fatalf("→ should descend to doc 0, descended=%v cur=%d", m.convo[0].docs.descended, m.convo[0].docs.cur)
	}
	m = key(m, kDown) // next doc
	if m.convo[0].docs.cur != 1 {
		t.Fatalf("↓ should move to doc 1, got %d", m.convo[0].docs.cur)
	}
	m = key(m, kRight) // open doc 1's snippet
	if !m.convo[0].docs.docs[1].open || !strings.Contains(m.convo[0].view(), "layout") {
		t.Fatal("→ should open the current doc's snippet")
	}
	m = key(m, kRight) // nothing deeper — and the focus must NOT leak to the input
	if m.focus != focusConvo || !m.convo[0].docs.descended {
		t.Fatalf("→ inside the doc list must stay there, focus=%d descended=%v", m.focus, m.convo[0].docs.descended)
	}
	// ← is → run backwards, one level per press.
	m = key(m, kLeft)
	if m.convo[0].docs.docs[1].open {
		t.Fatal("← should close the open document before leaving the list")
	}
	m = key(m, kLeft)
	if m.convo[0].docs.descended {
		t.Fatal("← should ascend out of the doc list")
	}
	m = key(m, kLeft)
	if m.convo[0].state != viewMinimized {
		t.Fatalf("← should close the summary itself, got state %d", m.convo[0].state)
	}
	if m.focus != focusConvo {
		t.Fatalf("closing a block must not also change zone, focus=%d", m.focus)
	}
	// Enter still cycles, unchanged: the two ways in do not have to agree.
	m = key(m, kEnter)
	if m.convo[0].state == viewMinimized {
		t.Fatal("enter should still expand the summary")
	}
}

// The arrow rule for an ordinary folded block: → walks minimized → expanded →
// raw and stops, ← walks it back, and only a block with nothing left to open
// hands → to the input. Before this, → left the conversation immediately and
// the ▸ on every tool line could only be opened with enter.
func TestTUI_RightDescendsAFoldedBlock(t *testing.T) {
	m := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = nm.(tuiModel)
	m.convo = []convoEntry{
		{collapsed: "▸ call", full: "the styled body", raw: "the raw body"},
		{collapsed: "just a message"},
	}
	kRight := tea.KeyMsg{Type: tea.KeyRight}
	kLeft := tea.KeyMsg{Type: tea.KeyLeft}

	m = key(m, kLeft) // input → convo
	m.sel = 0
	if m.focus != focusConvo {
		t.Fatalf("want the conversation, got %d", m.focus)
	}
	for _, want := range []viewState{viewExpanded, viewRaw} {
		m = key(m, kRight)
		if m.convo[0].state != want {
			t.Fatalf("→ should reach state %d, got %d", want, m.convo[0].state)
		}
	}
	// Nothing deeper: → is the horizontal axis again and hands over to the
	// input, WITHOUT wrapping the block shut on the way out.
	m = key(m, kRight)
	if m.focus != focusInput || m.convo[0].state != viewRaw {
		t.Fatalf("→ past the deepest level should go to the input and leave the block open, focus=%d state=%d",
			m.focus, m.convo[0].state)
	}
	m.focus, m.sel = focusConvo, 0
	m.input.Blur()
	for _, want := range []viewState{viewExpanded, viewMinimized} {
		m = key(m, kLeft)
		if m.convo[0].state != want {
			t.Fatalf("← should reach state %d, got %d", want, m.convo[0].state)
		}
	}
	if m.focus != focusConvo {
		t.Fatalf("← should still be closing the block, not leaving, focus=%d", m.focus)
	}
	m = key(m, kLeft) // shut: now ← leaves the zone
	if m.focus == focusConvo {
		t.Fatal("← on a shut block should walk on to the next zone")
	}

	// A block with nothing inside it: → is still the way back to the input.
	m.focus, m.sel = focusConvo, 1
	m.input.Blur()
	m = key(m, kRight)
	if m.focus != focusInput {
		t.Fatalf("→ on a block with nothing to open should return to the input, got %d", m.focus)
	}
}

// A block whose deepest level is `full` must stop there. viewState.Next() wraps
// to raw, and view() falls back to the COLLAPSED line when raw is empty — so a
// naive → showed the one-liner again as if it had closed the block.
func TestTUI_RightStopsWhereTheBlockEnds(t *testing.T) {
	m := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = nm.(tuiModel)
	m.convo = []convoEntry{{collapsed: "▸ recap: 9 entries", full: "▾ recap: 9 entries\nreplaced with: …"}}

	m = key(m, tea.KeyMsg{Type: tea.KeyLeft}) // into the conversation
	m.sel = 0
	m = key(m, tea.KeyMsg{Type: tea.KeyRight})
	m = key(m, tea.KeyMsg{Type: tea.KeyRight})
	if m.convo[0].state != viewExpanded {
		t.Fatalf("a block with no raw view must stay expanded, got state %d", m.convo[0].state)
	}
	if !strings.Contains(m.convo[0].view(), "replaced with") {
		t.Fatalf("the expanded body must still be showing: %q", m.convo[0].view())
	}
}

// Recap no longer asks before it applies, so the event is the only place a
// human learns it happened: one dim line that OPENS onto the replacement text.
func TestTUI_RecapEventIsExpandable(t *testing.T) {
	m := newTUIModel(&dunProc{}, "/ws")
	m = m.handleEvent(evMsg{"type": "recap", "entries": 9.0, "chars": 258256.0,
		"note": "recap: 9 entries (~258256 chars) → s.recap1.jsonl",
		"detail": "Removed 9 entries (~258256 characters) from the model's context.\n\n" +
			"Replaced with:\n  │ the answer is a stack"})
	if len(m.convo) != 1 {
		t.Fatalf("a recap should be one block, got %d", len(m.convo))
	}
	e := m.convo[0]
	if !e.expandable() {
		t.Fatal("the recap line must open onto what replaced the span")
	}
	if !strings.Contains(e.view(), "9 entries") || strings.Contains(e.view(), "the answer is a stack") {
		t.Fatalf("collapsed should be the citation only: %q", e.view())
	}
	e.state = viewExpanded
	if !strings.Contains(e.view(), "the answer is a stack") {
		t.Fatalf("expanded should show the replacement: %q", e.view())
	}
	if m.ctxStats.recaps != 1 || m.ctxStats.entriesRecapped != 9 {
		t.Errorf("/context should still count it: %+v", m.ctxStats)
	}
}

func TestTUI_ErrorEventClearsBusy(t *testing.T) {
	m := newTUIModel(&dunProc{}, "/ws")
	m.busy = true
	m = m.handleEvent(evMsg{"type": "error", "error": "boom"})
	if m.busy {
		t.Fatal("error should clear busy")
	}
	if !strings.Contains(convoText(m), "boom") {
		t.Fatal("error text not shown")
	}
}

// The retry UX. The user's report was "a connection error and dun dies, no retry,
// no info": the retries WERE happening, inside the LLM client, reported only to a
// log the TUI writes to a temp file. These assert the wait is on screen.
func TestTUI_RetryBanner(t *testing.T) {
	m := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	m = resized(m, 100, 30)
	m = m.handleEvent(evMsg{"type": "ready", "tools": []any{"eval"}})
	m.busy = true

	// A rich 429 from a fair-share proxy: the queue numbers are the difference
	// between "429" and "4/4 busy, 2 ahead, come back in 10s".
	m = m.handleEvent(evMsg{"type": "retry", "scope": "request", "kind": "429",
		"attempt": 3.0, "status": 429.0, "delay_ms": 10000.0, "elapsed_ms": 13000.0,
		"capacity": 4.0, "in_flight": 4.0, "waiting": 2.0, "queue": "queue-timeout",
		"server_asked": true,
		"reason":       "provider at capacity — 4/4 slots busy, 2 waiting ahead, queue-timeout (attempt 3)",
		"text":         "provider at capacity — 4/4 slots busy, 2 waiting ahead, queue-timeout (attempt 3) — the provider asked for 10s"})

	if m.retry == "" {
		t.Fatal("no retry banner; the wait is invisible again")
	}
	for _, want := range []string{"4/4 busy", "2 ahead", "queue-timeout"} {
		if !strings.Contains(m.retry, want) {
			t.Errorf("banner = %q; want it to mention %q", m.retry, want)
		}
	}
	if m.retryDue.IsZero() {
		t.Error("no countdown deadline; the banner cannot tick down")
	}
	view := m.View()
	if !strings.Contains(stripANSI(view), "next try in") {
		t.Errorf("status line has no countdown:\n%s", stripANSI(view))
	}
	// The first wait also lands in scrollback, so the record survives the banner.
	if !strings.Contains(stripANSI(convoText(m)), "provider at capacity") {
		t.Errorf("first retry not recorded in scrollback:\n%s", stripANSI(convoText(m)))
	}
	// Subsequent waits update the banner but must NOT spam the conversation.
	before := len(m.convo)
	m = m.handleEvent(evMsg{"type": "retry", "scope": "request", "kind": "429",
		"attempt": 4.0, "delay_ms": 10000.0, "reason": "provider at capacity (attempt 4)", "text": "again"})
	if len(m.convo) != before {
		t.Errorf("scrollback grew by %d on a repeat wait", len(m.convo)-before)
	}

	// Recovery takes the banner down and says so.
	m = m.handleEvent(evMsg{"type": "retry", "kind": "recovered", "attempt": 5.0,
		"text": "provider recovered on attempt 5"})
	if m.retry != "" || !m.retryDue.IsZero() {
		t.Error("recovery left the banner up")
	}
	if !strings.Contains(stripANSI(convoText(m)), "recovered on attempt 5") {
		t.Error("recovery not recorded")
	}
}

// A turn-scope retry means the generation died mid-stream and will be redone, so
// the half-streamed text must go — otherwise the regenerated reply appends to a
// broken sentence.
func TestTUI_TurnRetryDiscardsPartialReply(t *testing.T) {
	m := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	m = resized(m, 100, 30)
	m = m.handleEvent(evMsg{"type": "ready", "tools": []any{"eval"}})
	m = m.handleEvent(evMsg{"type": "token", "text": "I'll start by rea"})
	if m.cur == "" {
		t.Fatal("precondition: partial text should be buffered")
	}
	m = m.handleEvent(evMsg{"type": "retry", "scope": "turn", "kind": "interrupted",
		"attempt": 1.0, "delay_ms": 1000.0, "reason": "the turn was interrupted", "text": "interrupted"})
	if m.cur != "" {
		t.Errorf("partial reply kept: %q", m.cur)
	}
	if !m.busy {
		t.Error("a retry in progress is still a turn in flight")
	}
	if strings.Contains(stripANSI(convoText(m)), "I'll start by rea") {
		t.Error("the discarded partial reply was finalized into the conversation")
	}
}

// Giving up is not the end of the session: the conversation is on disk, so the
// user is told that another message resumes from here.
func TestTUI_GiveUpKeepsSessionUsable(t *testing.T) {
	m := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	m = resized(m, 100, 30)
	m = m.handleEvent(evMsg{"type": "ready", "tools": []any{"eval"}})
	m.busy = true
	m = m.handleEvent(evMsg{"type": "retry", "kind": "giveup", "attempt": 5.0, "text": "gave up after 5m"})
	if m.busy {
		t.Error("giveup should clear busy so the input is usable")
	}
	m = m.handleEvent(evMsg{"type": "error", "error": "agent: chat: stream error"})
	if !strings.Contains(stripANSI(convoText(m)), "send a message to retry from here") {
		t.Errorf("no recovery hint after a failure:\n%s", stripANSI(convoText(m)))
	}
	// And the input accepts one.
	m = typeStr(m, "keep going")
	m = key(m, kEnter)
	if !strings.Contains(stripANSI(convoText(m)), "keep going") {
		t.Error("a message sent after a failure was dropped")
	}
}

// Typing while the agent works is allowed: the engine buffers the message and
// lifts it into the next tool result, so it lands in the RUNNING turn.
// The TUI shows queued messages as provisional entries in the conversation
// (dimmed, with "(queued)" suffix) and clears the marker when the turn ends.
func TestTUI_SendWhileBusyQueues(t *testing.T) {
	m := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	m = resized(m, 100, 30)
	m = m.handleEvent(evMsg{"type": "ready", "tools": []any{"eval"}})
	m.busy = true

	m = typeStr(m, "also update the README")
	m = key(m, kEnter)
	if strings.TrimSpace(m.input.Value()) != "" {
		t.Errorf("input not cleared; the message was refused while busy: %q", m.input.Value())
	}
	// The echo is in the convo right after sendUser.
	echoed := stripANSI(convoText(m))
	if !strings.Contains(echoed, "also update the README") {
		t.Error("mid-turn message not echoed after sendUser")
	}

	// The queued event marks the echo as provisional (dimmed, in-place).
	m = m.handleEvent(evMsg{"type": "queued", "text": "also update the README", "count": 1.0})
	if m.queuedMsgs != 1 {
		t.Errorf("queuedMsgs = %d; want 1", m.queuedMsgs)
	}
	// The message should still be in the convo, now as provisional.
	found := false
	for _, e := range m.convo {
		if e.provisional && e.provisionalText == "also update the README" {
			found = true
			break
		}
	}
	if !found {
		t.Error("message should be provisional in convo after queued event")
	}
	// The view should show it with "(queued)" suffix.
	view := stripANSI(m.View())
	if !strings.Contains(view, "also update the README") {
		t.Errorf("convo does not show queued message:\n%s", view)
	}
	if !strings.Contains(view, "1 message queued") {
		t.Errorf("status line does not report the queued message:\n%s", view)
	}

	// A second one batches with it.
	m = m.handleEvent(evMsg{"type": "queued", "text": "and the changelog", "count": 2.0})
	found2 := false
	for _, e := range m.convo {
		if e.provisional && e.provisionalText == "and the changelog" {
			found2 = true
			break
		}
	}
	if !found2 {
		t.Error("second message should be provisional in convo")
	}
	if !strings.Contains(stripANSI(m.View()), "2 messages queued") {
		t.Errorf("second queued message not counted:\n%s", stripANSI(m.View()))
	}

	// The turn ending clears the provisional markers.
	m = m.handleEvent(evMsg{"type": "done"})
	if m.queuedMsgs != 0 {
		t.Errorf("queuedMsgs = %d after done; want 0", m.queuedMsgs)
	}
	// No provisional entries should remain.
	for i, e := range m.convo {
		if e.provisional {
			t.Fatalf("convo[%d] should not be provisional after done", i)
		}
	}
	// Both messages should now be in the convo as normal user entries.
	convo := stripANSI(convoText(m))
	if !strings.Contains(convo, "also update the README") {
		t.Error("first queued message not in convo after done")
	}
	if !strings.Contains(convo, "and the changelog") {
		t.Error("second queued message not in convo after done")
	}
}

// Queued messages should also move into the convo when the turn errors — the
// engine flushes them on error, so they're part of the conversation history.
func TestTUI_QueuedMessagesOnError(t *testing.T) {
	m := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	m = resized(m, 100, 30)
	m = m.handleEvent(evMsg{"type": "ready", "tools": []any{"eval"}})
	m.busy = true

	m = typeStr(m, "fix this bug")
	m = key(m, kEnter)
	m = m.handleEvent(evMsg{"type": "queued", "text": "fix this bug", "count": 1.0})
	// The message should be provisional in the convo.
	found := false
	for _, e := range m.convo {
		if e.provisional && e.provisionalText == "fix this bug" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("message should be provisional in convo after queued event")
	}

	// Error event should clear provisional markers.
	m = m.handleEvent(evMsg{"type": "error", "error": "provider timeout", "fatal": false})
	if m.busy {
		t.Fatal("error should clear busy")
	}
	for i, e := range m.convo {
		if e.provisional {
			t.Fatalf("convo[%d] should not be provisional after error", i)
		}
	}
	convo := stripANSI(convoText(m))
	if !strings.Contains(convo, "fix this bug") {
		t.Error("queued message not in convo after error")
	}
	if !strings.Contains(convo, "provider timeout") {
		t.Error("error text not in convo")
	}
}

// /rag and /lsp are forwarded to the engine (which owns the harness), and the
// `server` event it sends back is rendered into the conversation. The TUI must
// not try to interpret the action itself.
func TestTUI_ServerSlashCommands(t *testing.T) {
	var sent bytes.Buffer
	m := newTUIModel(&dunProc{stdin: nopWC{&sent}}, "/ws")
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = nm.(tuiModel)

	m = typeStr(m, "/rag auto")
	m = key(m, kEnter)
	var got map[string]string
	if err := json.Unmarshal(sent.Bytes(), &got); err != nil {
		t.Fatalf("did not send a server event: %q (%v)", sent.String(), err)
	}
	if got["type"] != "server" || got["id"] != "rag" || got["action"] != "auto" {
		t.Fatalf("wrong event: %v", got)
	}

	// The engine's reply is what the user sees, verbatim.
	m = m.handleEvent(evMsg{"type": "server", "id": "rag", "action": "auto",
		"message": "rag (docs): autostart on (saved to /ws/.dun/dun.local.json)",
		"tools":   []any{"eval", "search"}})
	if !strings.Contains(convoText(m), "autostart on") {
		t.Fatalf("server reply not shown: %s", convoText(m))
	}
	// A start/stop changes the tool set; the TUI's copy must follow.
	if len(m.tools) != 2 {
		t.Errorf("tool list not refreshed from the server event: %v", m.tools)
	}
}

// A startup hint is the only signal that a tool family is off — the alternative
// symptom is an agent that silently never navigates code.
func TestTUI_ReadyHint(t *testing.T) {
	m := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = nm.(tuiModel)
	m = m.handleEvent(evMsg{"type": "ready", "tools": []any{"eval"},
		"hint": "docs off — /rag on to start it, /rag auto to start it every session"})
	if !strings.Contains(convoText(m), "/rag auto") {
		t.Fatalf("ready hint not shown: %s", convoText(m))
	}
}

// nopWC captures what the TUI writes to the engine.
type nopWC struct{ w io.Writer }

func (n nopWC) Write(p []byte) (int, error) { return n.w.Write(p) }
func (n nopWC) Close() error                { return nil }

// A turn that died is not a session that died. The engine says which, and the
// UI must not offer advice that cannot work — "send a message to retry" to a
// session that can no longer run a turn is how a 30-minute deadline came to
// look like a crash.
func TestTUI_ErrorHintTracksFatality(t *testing.T) {
	m := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = nm.(tuiModel)

	m = m.handleEvent(evMsg{"type": "error", "error": "context deadline exceeded", "fatal": false})
	if !strings.Contains(convoText(m), "session is intact") {
		t.Errorf("a recoverable turn failure should say so: %s", convoText(m))
	}

	m = m.handleEvent(evMsg{"type": "error", "error": "context canceled", "fatal": true})
	txt := convoText(m)
	if !strings.Contains(txt, "dun --continue") {
		t.Errorf("a dead session should point at --continue: %s", txt)
	}
	if strings.Count(txt, "session is intact") != 1 {
		t.Errorf("the fatal error must not repeat the retry advice: %s", txt)
	}
}

// The engine announces why it is leaving, so the TUI does not have to report a
// bare "engine exited" and leave the user guessing.
func TestTUI_ExitEventExplainsItself(t *testing.T) {
	m := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = nm.(tuiModel)
	m = m.handleEvent(evMsg{"type": "exit", "reason": "interrupted"})
	nm, _ = m.Update(eofMsg{})
	m = nm.(tuiModel)
	if !strings.Contains(m.fatalErr, "interrupted") {
		t.Errorf("exit reason lost: %q", m.fatalErr)
	}
}

// The TUI outlives its engine. A crash (stdout closes with no exit event) must
// put a new engine in its place attached to the same session, not leave a dead
// UI behind — the conversation is on disk and the user is still sitting there.
func TestTUI_RespawnsACrashedEngine(t *testing.T) {
	m := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = nm.(tuiModel)
	m = m.handleEvent(evMsg{"type": "session", "id": "20260730-101010"})
	if m.sessionID == "" {
		t.Fatal("session id not captured — a respawn could not reattach")
	}

	nm, cmd := m.Update(eofMsg{})
	m = nm.(tuiModel)
	if cmd == nil {
		t.Fatal("a crashed engine should be restarted")
	}
	if m.fatalErr != "" {
		t.Errorf("a restartable death is not fatal: %q", m.fatalErr)
	}
	if !strings.Contains(convoText(m), "restarting") {
		t.Errorf("the restart should be visible: %s", convoText(m))
	}
	if !m.skipHistory {
		t.Error("the respawned engine's history replay must be skipped (it is already on screen)")
	}
}

// An engine that says it is going was told to go. Restarting it would fight the
// user.
func TestTUI_ExitEventStopsTheRestartLoop(t *testing.T) {
	m := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = nm.(tuiModel)
	m = m.handleEvent(evMsg{"type": "exit", "reason": "interrupted"})
	nm, cmd := m.Update(eofMsg{})
	m = nm.(tuiModel)
	if cmd != nil {
		t.Error("an announced exit must not be restarted")
	}
	if !strings.Contains(m.fatalErr, "interrupted") {
		t.Errorf("exit reason lost: %q", m.fatalErr)
	}
}

// A crash LOOP is not survivable by restarting — after the cap the failure is
// reported instead of hidden behind a flickering UI.
func TestTUI_RestartCapGivesUp(t *testing.T) {
	m := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = nm.(tuiModel)
	for i := 0; i < engineRestartMax; i++ {
		nm, cmd := m.Update(eofMsg{})
		m = nm.(tuiModel)
		if cmd == nil {
			t.Fatalf("restart %d should have been attempted", i+1)
		}
	}
	nm, cmd := m.Update(eofMsg{})
	m = nm.(tuiModel)
	if cmd != nil {
		t.Error("past the cap the TUI should stop restarting")
	}
	if !strings.Contains(m.fatalErr, "gave up") {
		t.Errorf("giving up should be reported: %q", m.fatalErr)
	}
	if !strings.Contains(convoText(m), "dun --continue") {
		t.Errorf("the user should be told the conversation survived: %s", convoText(m))
	}
}

// A server the user turned on by hand is gone from a fresh engine, so the TUI
// puts it back — otherwise a restart silently costs the agent its tools.
func TestTUI_RestartReappliesServers(t *testing.T) {
	var sent bytes.Buffer
	m := newTUIModel(&dunProc{stdin: nopWC{&sent}}, "/ws")
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = nm.(tuiModel)
	// First ready: docs is running (the user turned it on last session).
	m = m.handleEvent(evMsg{"type": "ready", "tools": []any{"search"},
		"servers": []any{map[string]any{"id": "docs", "running": true}}})
	if !m.wantServers["rag"] {
		t.Fatalf("running server not recorded: %v", m.wantServers)
	}
	// Engine dies, comes back with nothing on.
	nm, _ = m.Update(eofMsg{})
	m = nm.(tuiModel)
	sent.Reset()
	m = m.handleEvent(evMsg{"type": "ready", "tools": []any{"eval"},
		"servers": []any{map[string]any{"id": "docs", "running": false}}})
	if !strings.Contains(sent.String(), `"action":"on"`) || !strings.Contains(sent.String(), `"id":"rag"`) {
		t.Errorf("/rag was not turned back on after the restart: %q", sent.String())
	}
}

// After the restart cap the TUI is still a working UI, and /reconnect is the
// way back — the alternative is quitting and losing the terminal state for a
// session that is fine on disk.
func TestTUI_ReconnectAfterGivingUp(t *testing.T) {
	m := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = nm.(tuiModel)
	m.restarts, m.restartStart = engineRestartMax, time.Now()
	m.fatalErr = "dun engine exited — gave up restarting it"

	cmd := m.runSlash("/reconnect")
	if cmd == nil {
		t.Fatal("/reconnect should spawn an engine")
	}
	if m.restarts != 0 {
		t.Errorf("the retry budget should be refilled, got %d", m.restarts)
	}
	if m.fatalErr != "" {
		t.Errorf("reconnecting clears the dead-engine banner, got %q", m.fatalErr)
	}
	if !strings.Contains(convoText(m), "reconnecting") {
		t.Errorf("the attempt should be visible: %s", convoText(m))
	}
}

// Between an engine dying and its replacement there is no engine. Typing then
// must not panic, and must not swallow the message silently either.
func TestTUI_NoEngineIsSurvivable(t *testing.T) {
	m := newTUIModel(nil, "/ws") // never started
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = nm.(tuiModel)
	if cmd := m.Init(); cmd == nil {
		t.Fatal("Init with no engine should still schedule work (a retry)")
	}
	m = m.sendUser("are you there")
	if m.busy {
		t.Error("nothing was sent, so no turn is running")
	}
	if !strings.Contains(convoText(m), "not sent") {
		t.Errorf("a dropped message must be reported: %s", convoText(m))
	}
	// And the slash commands that talk to the engine say so rather than panic.
	m.runSlash("/rag on")
	if !strings.Contains(convoText(m), "no engine") {
		t.Errorf("server command should report the missing engine: %s", convoText(m))
	}
}

// TestTUI_StreamingMatchesFinalized verifies that the streaming cursor
// (m.cur) is rendered through the same markdown pipeline as finalized text.
// This prevents the visual "jump" where streaming text looks different from
// the finalized block.
func TestTUI_StreamingMatchesFinalized(t *testing.T) {
	m := newTUIModel(&dunProc{}, "/ws")
	m.md = newMarkdown(80)
	if m.md == nil {
		t.Skip("glamour renderer unavailable")
	}

	// Simulate streaming tokens that form markdown.
	m.cur = "**bold** and `code`"

	// Capture what fullText() renders for the streaming cursor.
	streaming := fullText(m)

	// Now finalize the same text and capture what convoText() produces.
	m.flushCur()
	finalized := convoText(m)

	// Both should be identical — same renderer, same input.
	if streaming != finalized {
		t.Errorf("streaming != finalized:\n  streaming:  %q\n  finalized: %q", streaming, finalized)
	}
}

// TestTUI_ScrollPin verifies that scrolling up unpins the viewport so new
// messages don't jump the user's position, and scrolling back to the bottom
// re-pins it.
func TestTUI_ScrollPin(t *testing.T) {
	m := newTUIModel(&dunProc{}, "/ws")

	// scrollPinned should default to true (follow bottom).
	if !m.scrollPinned {
		t.Fatal("scrollPinned should be true by default")
	}

	// Initialize the viewport with a window size.
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = nm.(tuiModel)

	// Add enough content to make the viewport scrollable.
	for i := 0; i < 50; i++ {
		m.convo = append(m.convo, convoEntry{collapsed: fmt.Sprintf("message line %d", i)})
	}
	m.vp.SetLines(strings.Split(fullText(m), "\n"))

	// After setup, pin should still be true.
	if !m.scrollPinned {
		t.Fatal("scrollPinned should still be true after setup")
	}

	// Simulate scrolling up via pgup.
	m = key(m, tea.KeyMsg{Type: tea.KeyPgUp})

	// Should be unpinned now.
	if m.scrollPinned {
		t.Fatal("scrollPinned should be false after scrolling up")
	}

	// Simulate scrolling back down to the bottom with multiple pgdowns.
	for i := 0; i < 10; i++ {
		m = key(m, tea.KeyMsg{Type: tea.KeyPgDown})
	}

	// Should be re-pinned at the bottom.
	if !m.scrollPinned {
		t.Fatal("scrollPinned should be true after scrolling back to bottom")
	}
}

// ── multiline input tests ──

// The composer GROWS. An empty box that reserves four rows costs four rows of
// conversation for nothing, which is what it did.
func TestMultilineInput_Basic(t *testing.T) {
	in := newMultilineInput()
	in.width = 80

	for _, r := range "hello world" {
		in = in.HandleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	if in.Value() != "hello world" {
		t.Fatalf("expected 'hello world', got %q", in.Value())
	}
	if n := len(strings.Split(in.View(), "\n")); n != 1 {
		t.Fatalf("one line of text is one row, got %d", n)
	}

	in.Reset()
	if in.Value() != "" || strings.TrimSpace(in.Value()) != "" {
		t.Fatalf("Reset should clear, got %q", in.Value())
	}
	in.SetValue("preloaded text")
	if in.Value() != "preloaded text" {
		t.Fatalf("SetValue failed: got %q", in.Value())
	}
}

// Regression: with the cursor at the end of a row, View wrote the row, then the
// cursor, then the row AGAIN — every keystroke looked like it had been typed
// twice. `after` was initialised to the whole line and never cleared.
func TestMultilineInput_RendersTextOnce(t *testing.T) {
	in := newMultilineInput()
	in.width = 80
	in.SetValue("hello world")
	in.CursorEnd()

	if n := strings.Count(in.View(), "hello world"); n != 1 {
		t.Fatalf("the text must be rendered exactly once, got %d copies: %q", n, in.View())
	}
}

// Regression: wrapping broke at a space by taking chunk[:space] and then
// advancing past the WHOLE window, so everything between the break and the
// window edge silently vanished from the display — and, because the cursor
// mapping was derived from those strings, took the cursor with it.
func TestWrapLine_KeepsEveryCharacter(t *testing.T) {
	const text = "one two three four five sixseven eight"
	buf := []rune(text)
	segs := wrapLine(buf, 0, len(buf), 10)

	var rebuilt strings.Builder
	for _, s := range segs {
		if s.end-s.start > 10 {
			t.Errorf("row wider than the box: %q", string(buf[s.start:s.end]))
		}
		rebuilt.WriteString(string(buf[s.start:s.end]))
		rebuilt.WriteString(string(buf[s.end:s.next])) // the separator wrapping consumed
	}
	if rebuilt.String() != text {
		t.Fatalf("wrapping lost text:\n want %q\n got  %q", text, rebuilt.String())
	}
}

// The cursor mapping has to be exact in both directions, or editing lands in
// the wrong place. Every offset in a wrapped buffer must survive the round trip.
func TestMultilineInput_CursorRoundTrip(t *testing.T) {
	in := newMultilineInput()
	in.width = 10
	in.SetValue("one two three\nfour five sixseven eight")

	for off := 0; off <= len(in.buf); off++ {
		in.cursor = off
		row, col := in.cursorDisplayPos()
		if got := in.displayToOffset(row, col); got != off {
			t.Fatalf("offset %d → (%d,%d) → %d", off, row, col, got)
		}
	}
}

// Arrows MOVE. The first version inserted a newline into the buffer when → or ↓
// landed on a wrap point, so navigating your own text rewrote it.
func TestMultilineInput_ArrowsDoNotEditTheBuffer(t *testing.T) {
	in := newMultilineInput()
	in.width = 10
	in.SetValue("1234567890abcdefghij")

	for _, k := range []tea.KeyMsg{{Type: tea.KeyRight}, {Type: tea.KeyDown}, {Type: tea.KeyUp}, {Type: tea.KeyLeft}} {
		in.cursor = 10 // the wrap point
		in = in.HandleKey(k)
		if in.Value() != "1234567890abcdefghij" {
			t.Fatalf("%v edited the buffer: %q", k, in.Value())
		}
	}
}

// Enter submits, so a newline needs a key of its own.
func TestMultilineInput_AltEnterInsertsNewline(t *testing.T) {
	in := newMultilineInput()
	in.width = 80
	in.SetValue("first")
	in.CursorEnd()

	in = in.HandleKey(tea.KeyMsg{Type: tea.KeyEnter, Alt: true})
	in = in.HandleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("second")})
	if in.Value() != "first\nsecond" {
		t.Fatalf("alt+enter should insert a newline, got %q", in.Value())
	}
	if n := len(strings.Split(in.View(), "\n")); n != 2 {
		t.Fatalf("two logical lines is two rows, got %d", n)
	}
}

// A text box a shell user cannot drive is not finished.
func TestMultilineInput_ReadlineEditing(t *testing.T) {
	mk := func(v string, cur int) multilineInput {
		in := newMultilineInput()
		in.width = 80
		in.SetValue(v)
		in.cursor = cur
		return in
	}
	cases := []struct {
		name string
		in   multilineInput
		key  tea.KeyMsg
		want string
	}{
		{"ctrl+w kills the word behind", mk("hello world", 11), tea.KeyMsg{Type: tea.KeyCtrlW}, "hello "},
		{"ctrl+k kills to end of line", mk("hello world", 5), tea.KeyMsg{Type: tea.KeyCtrlK}, "hello"},
		{"ctrl+u kills to start of line", mk("hello", 3), tea.KeyMsg{Type: tea.KeyCtrlU}, "lo"},
		{"ctrl+d deletes forward", mk("hello", 0), tea.KeyMsg{Type: tea.KeyCtrlD}, "ello"},
	}
	for _, c := range cases {
		got := c.in.HandleKey(c.key)
		if got.Value() != c.want {
			t.Errorf("%s: want %q, got %q", c.name, c.want, got.Value())
		}
	}

	// Motions land on the logical line's bounds, not the buffer's.
	in := mk("first line\nsecond line", 14)
	in = in.HandleKey(tea.KeyMsg{Type: tea.KeyCtrlA})
	if in.cursor != 11 {
		t.Errorf("ctrl+a should go to the start of the LINE, got %d", in.cursor)
	}
	in = in.HandleKey(tea.KeyMsg{Type: tea.KeyCtrlE})
	if in.cursor != len(in.buf) {
		t.Errorf("ctrl+e should go to the end of the line, got %d", in.cursor)
	}
}

// Regression: bindings are matched on Key.String(), and a bracketed paste
// arrives as KeyRunes — so pasting the word "home" was read as the Home key.
func TestMultilineInput_PastedKeyNameIsText(t *testing.T) {
	in := newMultilineInput()
	in.width = 80
	in.SetValue("go ")
	in.CursorEnd()

	in = in.HandleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("home")})
	if in.Value() != "go home" {
		t.Fatalf("a pasted key name must be typed as text, got %q", in.Value())
	}
}

// ↑ moves within the composer and recalls history from the top row — the shell
// behaviour, and what keeps ↑ meaning "what I sent before" in a one-line box.
func TestMultilineInput_UpAtFirstRowRecallsHistory(t *testing.T) {
	proc := &dunProc{stdin: discardWC{}}
	m := newTUIModel(proc, "/ws")
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = nm.(tuiModel)
	m = m.handleEvent(evMsg{"type": "ready", "tools": []any{"eval"}})

	m = typeStr(m, "first message")
	m = key(m, kEnter)

	m = key(m, tea.KeyMsg{Type: tea.KeyUp})
	if m.input.Value() != "first message" {
		t.Fatalf("up from the first row should recall history, got %q", m.input.Value())
	}

	// With the cursor on a LOWER row, up moves the cursor and leaves the text.
	m.input.SetValue("aaa\nbbb")
	m.input.CursorEnd()
	m = key(m, tea.KeyMsg{Type: tea.KeyUp})
	if m.input.Value() != "aaa\nbbb" {
		t.Fatalf("up inside the buffer must not recall history, got %q", m.input.Value())
	}
	if row, _ := m.input.cursorDisplayPos(); row != 0 {
		t.Fatalf("up should have moved to the first row, got %d", row)
	}
}

func TestMultilineInput_HistoryViaCtrl(t *testing.T) {
	proc := &dunProc{stdin: discardWC{}}
	m := newTUIModel(proc, "/ws")
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = nm.(tuiModel)
	m = m.handleEvent(evMsg{"type": "ready", "tools": []any{"eval"}})

	m = typeStr(m, "first message")
	m = key(m, kEnter)
	m = typeStr(m, "second message")
	m = key(m, kEnter)
	if len(m.history) != 2 {
		t.Fatalf("history should have 2 entries, got %d", len(m.history))
	}

	ctrlUp := tea.KeyMsg{Type: tea.KeyCtrlUp}
	m = key(m, ctrlUp)
	if m.input.Value() != "second message" {
		t.Fatalf("ctrl+up should recall the last entry, got %q", m.input.Value())
	}
	m = key(m, ctrlUp)
	if m.input.Value() != "first message" {
		t.Fatalf("ctrl+up should step back, got %q", m.input.Value())
	}
	ctrlDown := tea.KeyMsg{Type: tea.KeyCtrlDown}
	m = key(m, ctrlDown)
	if m.input.Value() != "second message" {
		t.Fatalf("ctrl+down should step forward, got %q", m.input.Value())
	}
	m = key(m, ctrlDown)
	if m.input.Value() != "" {
		t.Fatalf("ctrl+down past the end should clear, got %q", m.input.Value())
	}
}

// The placeholder shows whether or not the box has focus — a hint that
// disappears the moment you focus the field is the wrong way round.
func TestMultilineInput_EmptyInput(t *testing.T) {
	proc := &dunProc{stdin: discardWC{}}
	m := newTUIModel(proc, "/ws")
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = nm.(tuiModel)
	m = m.handleEvent(evMsg{"type": "ready", "tools": []any{"eval"}})

	before := len(m.convo)
	m = key(m, kEnter)
	if len(m.convo) > before {
		t.Fatalf("empty enter should not submit, got %d new entries", len(m.convo)-before)
	}
	if !strings.Contains(m.input.View(), "ask dun") {
		t.Fatalf("a focused empty input should show the placeholder: %q", m.input.View())
	}
	m.input.Blur()
	if !strings.Contains(m.input.View(), "ask dun") {
		t.Fatalf("a blurred empty input should show the placeholder: %q", m.input.View())
	}
}

func TestMultilineInput_LeftAtFrontHopsToConvo(t *testing.T) {
	m := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = nm.(tuiModel)
	m.convo = []convoEntry{{collapsed: "a message"}}

	m = key(m, tea.KeyMsg{Type: tea.KeyLeft})
	if m.focus != focusConvo {
		t.Fatal("left at the input front should focus the conversation")
	}
}

// ── activity zone ──

func withActivity(t *testing.T) tuiModel {
	t.Helper()
	m := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	m = nm.(tuiModel)
	m = m.handleEvent(evMsg{"type": "ready", "tools": []any{"eval"}})
	m = m.handleEvent(evMsg{"type": "agents", "agents": []any{
		map[string]any{"id": 1.0, "state": "running", "prompt": "read the build log", "tokens": 6500.0, "seconds": 122.0},
		map[string]any{"id": 2.0, "state": "idle", "prompt": "count matches", "tokens": 900.0, "seconds": 41.0},
	}})
	m = m.handleEvent(evMsg{"type": "jobs", "jobs": []any{
		map[string]any{"id": 3.0, "command": "go test -race ./...", "state": "running",
			"started": float64(time.Now().Add(-time.Minute).Unix()), "log": "/tmp/job-3.log"},
	}})
	return m
}

// A session that never delegates and never backgrounds a command must lose no
// space to the zone, and tab must keep behaving exactly as it always did.
func TestActivity_EmptyZoneIsSkippedEntirely(t *testing.T) {
	m := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = nm.(tuiModel)

	if v := m.activityView(); v != "" {
		t.Fatalf("an empty zone must render nothing, got %q", v)
	}
	m = key(m, tea.KeyMsg{Type: tea.KeyTab})
	if m.focus != focusConvo {
		t.Fatalf("tab should reach the conversation, got focus %d", m.focus)
	}
	m = key(m, tea.KeyMsg{Type: tea.KeyTab})
	if m.focus != focusInput {
		t.Fatalf("tab should return to the input, skipping an empty zone, got %d", m.focus)
	}
}

// Collapsed it costs ONE line no matter how many children exist — that is what
// makes it affordable to always show.
func TestActivity_CollapsedIsOneLineWithTheAffordance(t *testing.T) {
	m := withActivity(t)
	v := m.activityView()
	if n := len(strings.Split(v, "\n")); n != 1 {
		t.Fatalf("collapsed must be one line, got %d:\n%s", n, v)
	}
	if !strings.Contains(v, "▸") {
		t.Errorf("the collapsed line must carry the descend affordance: %q", v)
	}
	if !strings.Contains(v, "#1") || !strings.Contains(v, "job 3") {
		t.Errorf("every row should appear in the summary: %q", v)
	}
}

// → opens and descends, ← ascends: the same rule as a docs block, which is the
// whole point of using the same glyph.
func TestActivity_RightDescendsLeftAscends(t *testing.T) {
	m := withActivity(t)
	m = key(m, tea.KeyMsg{Type: tea.KeyTab}) // convo
	m = key(m, tea.KeyMsg{Type: tea.KeyTab}) // activity
	if m.focus != focusActivity {
		t.Fatalf("tab should reach the activity zone when it has rows, got %d", m.focus)
	}
	if m.actLevel != actCollapsed {
		t.Fatal("focus alone must not expand the zone — → does")
	}

	m = key(m, tea.KeyMsg{Type: tea.KeyRight})
	if m.actLevel != actList {
		t.Fatal("right should descend into the row list")
	}
	if n := len(strings.Split(m.activityView(), "\n")); n < 3 {
		t.Errorf("the table should show a row per item, got:\n%s", m.activityView())
	}

	m = key(m, tea.KeyMsg{Type: tea.KeyDown})
	if sel := m.actSelected(); sel == nil || sel.key != (actKey{agent: true, id: 2}) {
		t.Errorf("down should move to the second agent, got %+v", sel)
	}

	m = key(m, tea.KeyMsg{Type: tea.KeyLeft})
	if m.actLevel != actCollapsed {
		t.Fatal("left should ascend back to the collapsed line")
	}
	m = key(m, tea.KeyMsg{Type: tea.KeyLeft})
	if m.focus != focusInput {
		t.Fatal("left at the top of the zone should go home to the input")
	}
}

// The selection is stored by IDENTITY. A new agent shifts every job index down,
// so an index would silently walk to a different row while you were reading it.
func TestActivity_SelectionSurvivesANewRow(t *testing.T) {
	m := withActivity(t)
	m.focus, m.actLevel = focusActivity, actList
	m.actSel = actKey{id: 3} // the job

	m = m.handleEvent(evMsg{"type": "agents", "agents": []any{
		map[string]any{"id": 1.0, "state": "running", "prompt": "a"},
		map[string]any{"id": 2.0, "state": "idle", "prompt": "b"},
		map[string]any{"id": 9.0, "state": "running", "prompt": "brand new"},
	}})
	sel := m.actSelected()
	if sel == nil || sel.key != (actKey{id: 3}) {
		t.Fatalf("the selection must still be on job 3, got %+v", sel)
	}
}

// Descending into an agent is a scope switch: the conversation and the task
// line become that child's, and the strip becomes the way back.
func TestActivity_AgentScopeSwapsTheConversation(t *testing.T) {
	m := withActivity(t)
	m = typeStr(m, "the root task")
	m = key(m, kEnter)
	rootLen := len(m.convo)

	m.focus, m.actLevel = focusActivity, actList
	m.actSel = actKey{agent: true, id: 1}
	m = key(m, tea.KeyMsg{Type: tea.KeyRight})

	if m.scopeAgent != 1 {
		t.Fatalf("right on an agent row should enter its scope, got %d", m.scopeAgent)
	}
	if len(m.convo) != 0 {
		t.Errorf("the child's conversation starts empty until its history arrives, got %d", len(m.convo))
	}
	if m.task != "read the build log" {
		t.Errorf("the task line should become the child's prompt, got %q", m.task)
	}
	if v := m.activityView(); !strings.Contains(v, "parent") {
		t.Errorf("agent scope must offer the way back: %q", v)
	}

	// The child's scrollback arrives, and only for the scope on screen.
	m = m.handleEvent(evMsg{"type": "agent_history", "agent": 7.0, "items": []any{
		map[string]any{"kind": "user", "content": "someone else's conversation"},
	}})
	if len(m.convo) != 0 {
		t.Error("history for another agent must not paste into this scope")
	}
	m = m.handleEvent(evMsg{"type": "agent_history", "agent": 1.0, "items": []any{
		map[string]any{"kind": "user", "content": "read the build log"},
		map[string]any{"kind": "assistant", "content": "137 failures"},
	}})
	// Assert on one word: glamour styles each word separately, so escape codes
	// sit between them in the rendered text.
	if !strings.Contains(convoText(m), "137") {
		t.Errorf("the child's conversation should be on screen: %s", convoText(m))
	}

	// Leaving restores the root conversation exactly.
	m.focus, m.actLevel = focusActivity, actList
	m.actSel = parentKey
	m = key(m, tea.KeyMsg{Type: tea.KeyRight})
	if m.scopeAgent != 0 {
		t.Fatal("descending into the parent row should leave agent scope")
	}
	if len(m.convo) != rootLen || m.task != "the root task" {
		t.Errorf("the root conversation and task must come back: %d entries, task %q", len(m.convo), m.task)
	}
}

// In agent scope the input steers that CHILD, not the session.
func TestActivity_InputInScopeTellsTheChild(t *testing.T) {
	m := withActivity(t)
	m.scopeAgent = 1
	before := len(m.history)

	m = typeStr(m, "look at the last 40 lines instead")
	m = key(m, kEnter)

	if len(m.history) != before+1 {
		t.Error("a message to a child should still be recallable")
	}
	if !strings.Contains(convoText(m), "look at the last 40 lines") {
		t.Errorf("the message should be echoed: %s", convoText(m))
	}
	if m.busy {
		t.Error("telling a child is not a turn of the session's own")
	}
}

// m.task is the last thing the human ASKED for. It no longer has a line of its
// own — it read as a second, broken scroll indicator sitting under the real one
// — but it is still tracked, both for the agent-scoping swap above and for
// whatever displays it next.
func TestActivity_TaskTracksTheLastUserMessage(t *testing.T) {
	m := withActivity(t)
	if m.task != "" {
		t.Errorf("nothing asked yet, so no task: %q", m.task)
	}
	m = typeStr(m, "formalize the jobs table")
	m = key(m, kEnter)
	if m.task != "formalize the jobs table" {
		t.Errorf("task should be the last message, got %q", m.task)
	}

	m2 := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	nm, _ := m2.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m2 = nm.(tuiModel)
	m2 = m2.handleEvent(evMsg{"type": "history", "items": []any{
		map[string]any{"kind": "user", "content": "the resumed task"},
	}})
	if m2.task != "the resumed task" {
		t.Errorf("a resumed session should carry its task, got %q", m2.task)
	}

	// And it draws nothing: the top row is the header or the scroll overlay,
	// never a second copy of the newest message.
	v := newVtui(80, 24)
	v.event(map[string]any{"type": "ready", "tools": []any{"eval"}})
	v.send("formalize the jobs table")
	for _, row := range screen(v)[:2] {
		if strings.Contains(row, "formalize the jobs table") {
			t.Errorf("the task still has a line of its own: %q", strings.TrimRight(row, " "))
		}
	}
}

// ← keeps going the way it was already going. It used to stop at the
// conversation; now, with nothing left to ascend out of, it continues to the
// activity strip when there is one — and wraps home when there is not. Same
// order as tab, so the two never disagree about where "back" is.
func TestActivity_LeftWalksBackThroughTheZones(t *testing.T) {
	m := withActivity(t)
	kLeft := tea.KeyMsg{Type: tea.KeyLeft}

	m = key(m, kLeft)
	if m.focus != focusConvo {
		t.Fatalf("left at the front of the input should reach the conversation, got %d", m.focus)
	}
	m = key(m, kLeft)
	if m.focus != focusActivity {
		t.Fatalf("left again should continue to the activity strip, got %d", m.focus)
	}
	m = key(m, kLeft)
	if m.focus != focusInput {
		t.Fatalf("left from the collapsed strip should wrap home to the input, got %d", m.focus)
	}
}

// With no agents and no jobs there is no strip to walk into, so ← wraps
// straight home — a session that never delegates never lands on an empty zone.
func TestActivity_LeftWrapsHomeWithNoActivity(t *testing.T) {
	m := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = nm.(tuiModel)
	m.convo = []convoEntry{{collapsed: "a message"}}

	kLeft := tea.KeyMsg{Type: tea.KeyLeft}
	m = key(m, kLeft)
	if m.focus != focusConvo {
		t.Fatalf("want the conversation, got %d", m.focus)
	}
	m = key(m, kLeft)
	if m.focus != focusInput {
		t.Fatalf("want the input, got %d", m.focus)
	}
}

// /mcp rides the same `server` event as /rag and /lsp, but addresses the SET.
// The wire form is the only thing the engine sees, so pin it here: the sentinel
// id for "all of them", and where the optional target lands.
func TestTUI_MCPSlashWireForm(t *testing.T) {
	send := func(line string) map[string]string {
		t.Helper()
		var buf bytes.Buffer
		m := newTUIModel(&dunProc{stdin: bufCloser{&buf}}, "/ws")
		m.runSlash(line)
		var got map[string]string
		if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
			t.Fatalf("%s wrote %q, which is not one JSON event: %v", line, buf.String(), err)
		}
		return got
	}

	// Bare /mcp lists — no action, every server.
	if got := send("/mcp"); got["type"] != "server" || got["id"] != allServers || got["action"] != "" {
		t.Errorf("bare /mcp should ask every server for its status; got %v", got)
	}
	// /mcp restart bounces the set.
	if got := send("/mcp restart"); got["id"] != allServers || got["action"] != "restart" {
		t.Errorf("/mcp restart should target every server; got %v", got)
	}
	// A named target replaces the sentinel — it does NOT become a third field.
	if got := send("/mcp restart lsp"); got["id"] != "lsp" || got["action"] != "restart" {
		t.Errorf("/mcp restart lsp should target lsp; got %v", got)
	}
	// Case is normalized, the way every other slash handler does it.
	if got := send("/mcp RESTART LSP"); got["id"] != "lsp" || got["action"] != "restart" {
		t.Errorf("/mcp should lowercase action and target; got %v", got)
	}
}

// ── idle-gated suggestions ─────────────────────────────────────────
//
// The engine used to volunteer a suggestion call at the end of every turn,
// including autonomous ones. It now asks only when the UI says the human is
// idle, and these pin the four conditions that gate the request.

// suggestReq drives the model to the point where a suggestion could fire and
// reports what went to the engine.
func suggestReq(t *testing.T, setup func(*tuiModel)) (tuiModel, string) {
	t.Helper()
	var buf bytes.Buffer
	m := newTUIModel(&dunProc{stdin: bufCloser{&buf}}, "/ws")
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = nm.(tuiModel)
	m.starting = false                             // a live session: servers are up
	m.convo = []convoEntry{{collapsed: "a reply"}} // something to predict from
	// The turn ends: the idle clock starts here.
	m = m.handleEvent(evMsg{"type": "done"})
	// setup runs AFTER `done` because it describes the state at the moment the
	// tick LANDS. Running it before proved nothing for the busy case — `done`
	// clears busy, so that subtest passed while testing the opposite.
	if setup != nil {
		setup(&m)
	}
	m.lastKeyAt = time.Now().Add(-2 * idleSuggestDelay) // pretend the wait elapsed
	out, _ := m.update(idleSuggestMsg{})
	return out.(tuiModel), buf.String()
}

func TestSuggest_FiresOnceWhenIdle(t *testing.T) {
	m, sent := suggestReq(t, nil)
	if !strings.Contains(sent, `"id":"suggest"`) {
		t.Fatalf("an idle, empty input should ask for suggestions; engine got %q", sent)
	}
	if !m.suggestedThisIdle {
		t.Error("the idle must be marked spent")
	}
	// A second tick in the same idle must NOT ask again.
	var buf2 bytes.Buffer
	m.proc = &dunProc{stdin: bufCloser{&buf2}}
	m.lastKeyAt = time.Now().Add(-2 * idleSuggestDelay)
	if _, _ = m.update(idleSuggestMsg{}); strings.Contains(buf2.String(), "suggest") {
		t.Errorf("a second tick in one idle must not ask again; engine got %q", buf2.String())
	}
}

func TestSuggest_HeldBackWhileTheHumanIsBusy(t *testing.T) {
	for name, setup := range map[string]func(*tuiModel){
		"text in the input": func(m *tuiModel) { m.input.SetValue("half a thought") },
		"a turn is running": func(m *tuiModel) { m.busy = true },
		"answering an ask":  func(m *tuiModel) { m.asking = true },
		"suggestions off":   func(m *tuiModel) { m.suggestMode = "off" },
		"picker already up": func(m *tuiModel) { m.suggestions = []suggestion{{text: "x"}} },
		"inspector is open": func(m *tuiModel) { m.inspecting = true },
	} {
		t.Run(name, func(t *testing.T) {
			if _, sent := suggestReq(t, setup); strings.Contains(sent, "suggest") {
				t.Errorf("must not ask while %s; engine got %q", name, sent)
			}
		})
	}
}

// /rephrase [on|off|status]: the TUI keeps its own copy of the toggle (so bare
// /rephrase reports status without a round-trip) and tells the engine, which is
// the copy that actually rewrites at turn time.
func TestRephraseSlash_TogglesAndReports(t *testing.T) {
	defer func(want bool) { rephrasePrompt = want }(rephrasePrompt)

	for _, tc := range []struct{ arg string }{{"on"}, {"off"}} {
		t.Run(tc.arg, func(t *testing.T) {
			var buf bytes.Buffer
			m := newTUIModel(&dunProc{stdin: bufCloser{&buf}}, "/ws")
			rephraseSlash(&m, []string{tc.arg})
			want := tc.arg == "on"
			if rephrasePrompt != want {
				t.Fatalf("/rephrase %s: rephrasePrompt = %v, want %v", tc.arg, rephrasePrompt, want)
			}
			if got := buf.String(); !strings.Contains(got, `"id":"rephrase"`) || !strings.Contains(got, `"action":"`+tc.arg+`"`) {
				t.Fatalf("the engine must be told; stdin got %q", got)
			}
			if !strings.Contains(m.convo[len(m.convo)-1].collapsed, "rephrase: "+tc.arg) {
				t.Fatalf("no confirmation line: %q", m.convo[len(m.convo)-1].collapsed)
			}
		})
	}

	t.Run("bare shows status", func(t *testing.T) {
		rephrasePrompt = true
		var buf bytes.Buffer
		m := newTUIModel(&dunProc{stdin: bufCloser{&buf}}, "/ws")
		rephraseSlash(&m, nil)
		last := m.convo[len(m.convo)-1].collapsed
		if !strings.Contains(last, "rephrase: on") {
			t.Fatalf("bare /rephrase should report on: %q", last)
		}
		if buf.String() != "" {
			t.Fatalf("bare /rephrase needs no engine round-trip; stdin got %q", buf.String())
		}
	})

	t.Run("bad arg", func(t *testing.T) {
		var buf bytes.Buffer
		m := newTUIModel(&dunProc{stdin: bufCloser{&buf}}, "/ws")
		rephraseSlash(&m, []string{"sometimes"})
		if !strings.Contains(m.convo[len(m.convo)-1].collapsed, "usage") {
			t.Fatalf("want a usage error: %q", m.convo[len(m.convo)-1].collapsed)
		}
	})
}

// The wait is a DEBOUNCE, not a poll: a keystroke inside the window pushes the
// deadline out instead of letting the tick through.
func TestSuggest_KeystrokeRestartsTheWait(t *testing.T) {
	var buf bytes.Buffer
	m := newTUIModel(&dunProc{stdin: bufCloser{&buf}}, "/ws")
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = nm.(tuiModel)
	m.convo = []convoEntry{{collapsed: "a reply"}}
	m = m.handleEvent(evMsg{"type": "done"})

	// A key arrives now, so the deadline is 3s from now, not from `done`.
	nm2, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'h'}})
	m = nm2.(tuiModel)
	if cmd == nil {
		t.Fatal("a keystroke in a suggestible session should keep the debounce armed")
	}
	out, again := m.update(idleSuggestMsg{})
	m = out.(tuiModel)
	if strings.Contains(buf.String(), "suggest") {
		t.Errorf("the tick fired early after a keystroke; engine got %q", buf.String())
	}
	if again == nil {
		t.Error("an early tick must re-arm for the remainder, not give up")
	}
	if !m.idleTickPending {
		t.Error("the re-armed tick must be tracked")
	}
}

// Nothing to predict from, or suggestions off: no timer at all. A keypress the
// UI ignores must stay ignored.
func TestSuggest_NoTimerWhenItCouldNeverFire(t *testing.T) {
	m := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	if _, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}}); cmd != nil {
		t.Error("an empty conversation has nothing to suggest from — no timer")
	}
	m.convo = []convoEntry{{collapsed: "a reply"}}
	m.suggestMode = "off"
	if _, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}}); cmd != nil {
		t.Error("suggestions off — no timer")
	}
}

// Pending messages: a mid-turn message is queued, then lifted into the next
// tool result. The provisional marker must clear on tool_result (not wait for done),
// and the message must remain in the convo as a normal user entry.
func TestTUI_PendingClearedOnToolResult(t *testing.T) {
	m := newTUIModel(&dunProc{}, "/ws")
	m.busy = true

	// First append the echo (sendUser does this), then the queued event marks it.
	m.convo = append(m.convo, convoEntry{collapsed: stUser.Render("› fix the bug")})
	// Simulate a queued mid-turn message (the "queued" event).
	m = m.handleEvent(evMsg{"type": "queued", "text": "fix the bug", "count": 1.0})
	// The message should be provisional in the convo.
	found := false
	for _, e := range m.convo {
		if e.provisional && e.provisionalText == "fix the bug" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("message should be provisional in convo after queued event")
	}
	if m.queuedMsgs != 1 {
		t.Fatalf("queued event should set queuedMsgs, got %d", m.queuedMsgs)
	}

	// A tool result arrives — liftQueued drained the buffer, so the provisional
	// marker must clear and the message renders normally.
	m = m.handleEvent(evMsg{"type": "tool_result", "tool": "eval", "result": "ok"})
	for i, e := range m.convo {
		if e.provisional {
			t.Fatalf("convo[%d] should not be provisional after tool_result", i)
		}
	}
	if m.queuedMsgs != 0 {
		t.Fatalf("tool_result should clear queuedMsgs, got %d", m.queuedMsgs)
	}

	// The queued message should be in the convo as a user entry.
	text := convoText(m)
	if !strings.Contains(text, "fix the bug") {
		t.Fatalf("convo should contain the lifted message: %q", text)
	}
}

// When a turn has no tool calls (the LLM replies without calling tools),
// the queued messages are flushed by flushQueued and rendered on the done event.
// ── virtual TUI rendering tests ──────────────────────────────────────

func TestVtui_UserMessageFullWidth(t *testing.T) {
	v := newVtui(40, 15) // phone-size terminal
	v.event(map[string]any{"type": "ready", "tools": []any{"eval"}})
	v.send("hello world")

	// The user message should appear in the conversation.
	if !strings.Contains(v.convo(), "hello world") {
		t.Fatalf("convo missing user message:\n%s", v.convo())
	}
	// The user message entry should have userText set for full-width rendering.
	m := v.model()
	var foundUser bool
	for _, e := range m.convo {
		if e.userText == "hello world" {
			foundUser = true
			break
		}
	}
	if !foundUser {
		t.Fatal("user message should have userText set for full-width background rendering")
	}
}

func TestVtui_PhoneSizeWrapping(t *testing.T) {
	v := newVtui(20, 10) // very narrow terminal
	v.event(map[string]any{"type": "ready", "tools": []any{"eval"}})
	v.send("short")
	v.event(map[string]any{"type": "token", "text": "this is a long response that should wrap across multiple lines in a narrow terminal"})
	v.event(map[string]any{"type": "done"})

	out := v.view()
	lines := strings.Split(out, "\n")
	// Check conversation content lines only (skip header, divider, input, status).
	for i, ln := range lines {
		plain := stripANSI(ln)
		// Skip non-convo lines.
		if strings.HasPrefix(plain, "dun") || strings.HasPrefix(plain, "─") {
			continue
		}
		if strings.Contains(plain, "ready") || strings.Contains(plain, "scroll") || strings.Contains(plain, "ask dun") {
			continue
		}
		// Convo content should wrap to terminal width.
		if len(plain) > 0 && len(plain) > 25 {
			t.Errorf("convo line %d too long for 20-col terminal (%d chars): %q", i, len(plain), plain)
		}
	}
}

func TestVtui_SendShowsMessage(t *testing.T) {
	v := newVtui(80, 24)
	v.event(map[string]any{"type": "ready", "tools": []any{"eval"}})
	v.send("test message")

	// The message should appear in the rendered view.
	out := v.view()
	if !strings.Contains(out, "test message") {
		t.Fatalf("sent message not visible in view:\n%s", out)
	}
	// Model should be busy.
	if !v.model().busy {
		t.Fatal("model should be busy after send")
	}
}

func TestVtui_Resize(t *testing.T) {
	v := newVtui(80, 24)
	v.event(map[string]any{"type": "ready", "tools": []any{"eval"}})
	v.send("hello")

	wide := v.view()
	v.resize(40, 15)
	narrow := v.view()

	// Narrow view should have shorter lines.
	wideLines := strings.Split(wide, "\n")
	narrowLines := strings.Split(narrow, "\n")

	maxWide := 0
	for _, ln := range wideLines {
		n := len(stripANSI(ln))
		if n > maxWide {
			maxWide = n
		}
	}
	maxNarrow := 0
	for _, ln := range narrowLines {
		n := len(stripANSI(ln))
		if n > maxNarrow {
			maxNarrow = n
		}
	}
	if maxNarrow >= maxWide {
		t.Errorf("narrow view max line (%d) should be < wide view max line (%d)", maxNarrow, maxWide)
	}
}

func TestVtui_ScrollOverlay(t *testing.T) {
	// Build a conversation tall enough to scroll in a small terminal.
	v := newVtui(60, 12)
	v.event(map[string]any{"type": "ready", "tools": []any{"eval"}})

	// Fill with enough content to overflow the viewport.
	for i := 0; i < 10; i++ {
		v.send(fmt.Sprintf("message %d", i))
		v.event(map[string]any{"type": "token", "text": fmt.Sprintf("response %d to your message\n", i)})
		v.event(map[string]any{"type": "done"})
	}

	// At bottom with YOffset=0, no overlay (nothing off-screen above).
	m := v.model()
	var overlay string
	if m.vp.YOffset != 0 {
		// Viewport may be at bottom with YOffset > 0 if content is tall.
		// In that case overlay should show the last off-screen message.
		overlay = m.scrollOverlay()
		if overlay == "" {
			t.Log("at bottom with YOffset>0: no overlay (content may fit)")
		}
	} else {
		overlay = m.scrollOverlay()
		if overlay != "" {
			t.Errorf("at YOffset=0: overlay should be empty, got %q", stripANSI(overlay))
		}
	}

	// Scroll up by directly setting YOffset (simulating user scroll).
	v.setYOffset(3)
	v.setScrollPin(false)
	_ = v.view() // refresh with new offset

	m = v.model()
	if m.scrollPinned {
		t.Fatal("should be unpinned after scrolling up")
	}

	overlay = m.scrollOverlay()
	if overlay == "" {
		t.Fatalf("scrolled up: overlay should show off-screen user message (yOff=%d, convoLen=%d)", m.vp.YOffset, len(m.convo))
	}
	plain := stripANSI(overlay)
	if !strings.HasPrefix(plain, "› ") {
		t.Errorf("overlay should start with '› ', got %q", plain)
	}

	// Scroll back to bottom.
	v.setScrollPin(true)
	_ = v.view()
	m = v.model()
	if !m.scrollPinned {
		t.Fatal("should be pinned again at bottom")
	}
	// At bottom, GotoBottom sets YOffset to the end — overlay may or may not
	// show depending on whether content extends above the viewport.
	overlay = m.scrollOverlay()
	if m.vp.YOffset == 0 && overlay != "" {
		t.Errorf("at YOffset=0: overlay should be empty, got %q", stripANSI(overlay))
	}
}

// TestVtui_ScrollOverlayChanges verifies that the overlay text updates as
// the user scrolls further up — the first off-screen user message should
// change to reflect which message is now hidden.
func TestVtui_ScrollOverlayChanges(t *testing.T) {
	v := newVtui(60, 12)
	v.event(map[string]any{"type": "ready", "tools": []any{"eval"}})

	// Build a tall conversation with distinct user messages.
	for i := 0; i < 10; i++ {
		v.send(fmt.Sprintf("msg %d", i))
		v.event(map[string]any{"type": "token", "text": fmt.Sprintf("reply %d\n", i)})
		v.event(map[string]any{"type": "done"})
	}

	v.setScrollPin(false)

	// Scroll progressively further up and verify the overlay changes.
	var seen []string
	for yOff := 3; yOff <= 30; yOff += 5 {
		v.setYOffset(yOff)
		_ = v.view()
		overlay := v.model().scrollOverlay()
		if overlay == "" {
			continue // might not have off-screen user messages at this offset
		}
		plain := stripANSI(overlay)
		seen = append(seen, plain)
	}

	if len(seen) < 2 {
		t.Fatalf("expected overlay to show different messages at different scroll positions, got %v", seen)
	}

	// Verify the overlay actually changed between positions.
	if seen[0] == seen[len(seen)-1] {
		t.Errorf("overlay did not change across scroll positions: always %q", seen[0])
	}

	// Each overlay should reference a user message.
	for _, s := range seen {
		if !strings.HasPrefix(s, "› msg ") {
			t.Errorf("overlay should reference a user message, got %q", s)
		}
	}
}

func TestTUI_PendingFlushedOnDone(t *testing.T) {
	m := newTUIModel(&dunProc{}, "/ws")
	m.busy = true

	// Queue a mid-turn message (echo first, then queued event marks it).
	m.convo = append(m.convo, convoEntry{collapsed: stUser.Render("› also check tests")})
	m = m.handleEvent(evMsg{"type": "queued", "text": "also check tests", "count": 1.0})

	// No tool_call/tool_result — just tokens and done.
	m = m.handleEvent(evMsg{"type": "token", "text": "ok"})
	m = m.handleEvent(evMsg{"type": "done"})

	// The provisional marker should be cleared by the done handler.
	for i, e := range m.convo {
		if e.provisional {
			t.Fatalf("convo[%d] should not be provisional after done", i)
		}
	}
	text := convoText(m)
	if !strings.Contains(text, "also check tests") {
		t.Fatalf("convo should contain the flushed message: %q", text)
	}
}

// Multiple queued messages should all be rendered after the tool result that
// lifted them, in the order they were queued.
func TestTUI_MultiplePendingMessages(t *testing.T) {
	m := newTUIModel(&dunProc{}, "/ws")
	m.busy = true

	// Echo first, then queued event marks it.
	m.convo = append(m.convo, convoEntry{collapsed: stUser.Render("› first")})
	m = m.handleEvent(evMsg{"type": "queued", "text": "first", "count": 1.0})
	m.convo = append(m.convo, convoEntry{collapsed: stUser.Render("› second")})
	m = m.handleEvent(evMsg{"type": "queued", "text": "second", "count": 2.0})

	// Both should be provisional.
	provCount := 0
	for _, e := range m.convo {
		if e.provisional {
			provCount++
		}
	}
	if provCount != 2 {
		t.Fatalf("should have 2 provisional, got %d", provCount)
	}

	m = m.handleEvent(evMsg{"type": "tool_result", "tool": "eval", "result": "ok"})
	for i, e := range m.convo {
		if e.provisional {
			t.Fatalf("convo[%d] should not be provisional after tool_result", i)
		}
	}

	text := convoText(m)
	if !strings.Contains(text, "first") || !strings.Contains(text, "second") {
		t.Fatalf("convo should contain both messages: %q", text)
	}
}

// Pending messages on error should also be rendered into the convo.
func TestTUI_PendingClearedOnError(t *testing.T) {
	m := newTUIModel(&dunProc{}, "/ws")
	m.busy = true

	// Echo first (sendUser does this), then queued event marks it.
	m.convo = append(m.convo, convoEntry{collapsed: stUser.Render("› before crash")})
	m = m.handleEvent(evMsg{"type": "queued", "text": "before crash", "count": 1.0})
	m = m.handleEvent(evMsg{"type": "error", "error": "provider down"})

	// The message should no longer be provisional — it was flushed on error.
	for i, e := range m.convo {
		if e.provisional {
			t.Fatalf("convo[%d] should not be provisional after error", i)
		}
	}
	text := convoText(m)
	if !strings.Contains(text, "before crash") {
		t.Fatalf("convo should contain the message flushed on error: %q", text)
	}
}

// TestVtui_ScrollOverlayInHeader verifies that the scroll overlay replaces
// the title bar (not a separate line) and that the header shows the normal
// dun title when at the bottom.
func TestVtui_ScrollOverlayInHeader(t *testing.T) {
	v := newVtui(80, 15)
	v.event(map[string]any{"type": "ready", "tools": []any{"eval"}})

	// Build a conversation tall enough to scroll (60+ lines of output).
	for i := 0; i < 15; i++ {
		v.send(fmt.Sprintf("user message %d", i))
		// Each response is 4 lines to ensure we get well over 60 total.
		resp := fmt.Sprintf("Response %d:\n", i)
		for j := 0; j < 3; j++ {
			resp += fmt.Sprintf("  line %d.%d of the assistant reply\n", i, j)
		}
		v.event(map[string]any{"type": "token", "text": resp})
		v.event(map[string]any{"type": "done"})
	}

	// At bottom: header should show the dun title bar, not an overlay.
	out := v.view()
	lines := strings.Split(out, "\n")
	if len(lines) == 0 {
		t.Fatal("view produced no output")
	}
	firstLine := lines[0]
	if strings.Contains(firstLine, "› user message") {
		t.Errorf("at bottom: header should show dun title, not overlay; got %q", firstLine)
	}
	if !strings.Contains(firstLine, "dun") {
		t.Errorf("at bottom: header should contain 'dun'; got %q", firstLine)
	}

	// Scroll up: header should now show the off-screen user message.
	v.setYOffset(5)
	v.setScrollPin(false)
	out = v.view()
	lines = strings.Split(out, "\n")
	firstLine = lines[0]
	if !strings.Contains(firstLine, "›") {
		t.Errorf("scrolled up: header should show overlay with '›'; got %q", firstLine)
	}
	if strings.Contains(firstLine, "dun") && !strings.Contains(firstLine, "user message") {
		t.Errorf("scrolled up: header should show user message, not dun title; got %q", firstLine)
	}

	// Verify no extra blank line between header and viewport (the overlay
	// replaced the header, it didn't add a line).
	// Count the total lines — should equal terminal height + status line.
	if len(lines) < v.h {
		t.Errorf("view too short: got %d lines, expected at least %d", len(lines), v.h)
	}

	// Scroll further up: overlay should update to show a different message.
	v.setYOffset(20)
	out = v.view()
	lines = strings.Split(out, "\n")
	firstLine = lines[0]
	if !strings.Contains(firstLine, "›") {
		t.Errorf("scrolled further: header should still show overlay; got %q", firstLine)
	}

	// Scroll back to bottom: header should revert to dun title.
	v.setScrollPin(true)
	v.m.refresh() // triggers GotoBottom()
	out = v.view()
	lines = strings.Split(out, "\n")
	firstLine = lines[0]
	if strings.Contains(firstLine, "› user message") && !strings.Contains(firstLine, "dun") {
		t.Errorf("back at bottom: header should show dun title; got %q", firstLine)
	}
}

// TestVtui_ScrollOverlayAfterReplay verifies the overlay works after a
// replay (the /reload path: fresh model → history event → replay → scroll).
func TestVtui_ScrollOverlayAfterReplay(t *testing.T) {
	v := newVtui(80, 15)
	v.event(map[string]any{"type": "ready", "tools": []any{"eval"}})

	// Simulate a history event (what the engine sends on resume/reload).
	items := make([]any, 0)
	for i := 0; i < 10; i++ {
		items = append(items, map[string]any{"kind": "user", "content": fmt.Sprintf("user message %d", i)})
		resp := fmt.Sprintf("Response %d:\n", i)
		for j := 0; j < 3; j++ {
			resp += fmt.Sprintf("  line %d.%d of the assistant reply\n", i, j)
		}
		items = append(items, map[string]any{"kind": "assistant", "content": resp})
	}
	v.event(map[string]any{"type": "history", "items": items})

	m := v.model()
	if len(m.convo) == 0 {
		t.Fatal("replay produced no convo entries")
	}

	// Verify userText is set on user entries.
	userCount := 0
	for _, e := range m.convo {
		if e.userText != "" {
			userCount++
		}
	}
	if userCount != 10 {
		t.Fatalf("expected 10 user entries with userText, got %d", userCount)
	}

	// At bottom: no overlay.
	overlay := m.scrollOverlay()
	if overlay != "" {
		t.Errorf("at bottom: overlay should be empty, got %q", stripANSI(overlay))
	}

	// Scroll up: overlay should show the last off-screen user message.
	v.setYOffset(5)
	v.setScrollPin(false)
	m = v.model()
	overlay = m.scrollOverlay()
	if overlay == "" {
		t.Fatalf("scrolled up (yOff=%d): overlay should show off-screen user message; convoLen=%d blockHLen=%d",
			m.vp.YOffset, len(m.convo), len(m.blockH))
	}
	plain := stripANSI(overlay)
	if !strings.HasPrefix(plain, "› ") {
		t.Errorf("overlay should start with '› ', got %q", plain)
	}

	// Verify the header in View() shows the overlay.
	out := v.view()
	lines := strings.Split(out, "\n")
	firstLine := lines[0]
	if !strings.Contains(firstLine, "›") {
		t.Errorf("header should show overlay; got %q", firstLine)
	}
}

// TestVtui_ScrollOverlayAfterRefresh verifies that the overlay survives a
// refresh() call while scrolled up — the common case where blinkTickMsg
// fires refresh() between a scroll and the next View(). This caught the
// bug where AtBottom() used stale m.vp.Height.
func TestVtui_ScrollOverlayAfterRefresh(t *testing.T) {
	v := newVtui(80, 15)
	v.event(map[string]any{"type": "ready", "tools": []any{"eval"}})

	// Build a tall conversation.
	for i := 0; i < 10; i++ {
		v.send(fmt.Sprintf("msg %d", i))
		resp := fmt.Sprintf("Reply %d:\n", i)
		for j := 0; j < 3; j++ {
			resp += fmt.Sprintf("  line %d.%d\n", i, j)
		}
		v.event(map[string]any{"type": "token", "text": resp})
		v.event(map[string]any{"type": "done"})
	}

	// Scroll up by setting YOffset and unpinning (simulates mouse wheel).
	v.setYOffset(10)
	v.setScrollPin(false)

	// A blinkTick fires refresh() — this is what happens in the real TUI
	// between a scroll event and the next View().
	v.m.refresh()

	// refresh() should NOT have re-pinned or reset YOffset since scrollPinned
	// is false.
	m := v.model()
	if m.scrollPinned {
		t.Fatal("refresh() should not re-pin when scrollPinned is false")
	}
	if m.vp.YOffset == 0 {
		t.Fatal("refresh() should not reset YOffset when scrollPinned is false")
	}

	// The overlay should still show.
	out := v.view()
	lines := strings.Split(out, "\n")
	firstLine := lines[0]
	if !strings.Contains(firstLine, "›") {
		t.Errorf("after refresh while scrolled: header should show overlay; got %q", firstLine)
	}

	// Now pin and refresh — should scroll to bottom and hide overlay.
	v.setScrollPin(true)
	v.m.refresh()
	out = v.view()
	lines = strings.Split(out, "\n")
	firstLine = lines[0]
	if strings.Contains(firstLine, "› msg") && !strings.Contains(firstLine, "dun") {
		t.Errorf("after pin+refresh: header should show dun title; got %q", firstLine)
	}
}

// TestVtui_ScrollOverlayViaMouseWheel verifies the overlay works through the
// actual mouse wheel path (not direct YOffset manipulation).
func TestVtui_ScrollOverlayViaMouseWheel(t *testing.T) {
	v := newVtui(80, 15)
	v.event(map[string]any{"type": "ready", "tools": []any{"eval"}})

	for i := 0; i < 10; i++ {
		v.send(fmt.Sprintf("msg %d", i))
		resp := fmt.Sprintf("Reply %d:\n", i)
		for j := 0; j < 3; j++ {
			resp += fmt.Sprintf("  line %d.%d\n", i, j)
		}
		v.event(map[string]any{"type": "token", "text": resp})
		v.event(map[string]any{"type": "done"})
	}

	// At bottom: header shows dun title.
	out := v.view()
	lines := strings.Split(out, "\n")
	if !strings.Contains(lines[0], "dun") {
		t.Errorf("at bottom: header should show dun; got %q", lines[0])
	}

	// Scroll up via mouse wheel (one click = 3 lines by default).
	msg := tea.MouseMsg{Button: tea.MouseButtonWheelUp, Action: tea.MouseActionPress}
	nm, _ := v.m.Update(msg)
	v.m = nm.(tuiModel)

	// Should be unpinned now.
	if v.m.scrollPinned {
		t.Fatalf("mouse wheel should unpin scroll (yOff=%d, atBottom=%v)",
			v.m.vp.YOffset, v.m.vp.AtBottom())
	}

	// Header should show overlay.
	out = v.view()
	lines = strings.Split(out, "\n")
	firstLine := lines[0]
	if !strings.Contains(firstLine, "›") {
		t.Errorf("after mouse wheel: header should show overlay; yOff=%d; got %q",
			v.m.vp.YOffset, firstLine)
	}
}

// TestVtui_ScrollOverlayContentGrowsWhileScrolled verifies the overlay stays
// visible when new content arrives (token event) while the user is scrolled up.
func TestVtui_ScrollOverlayContentGrowsWhileScrolled(t *testing.T) {
	v := newVtui(80, 15)
	v.event(map[string]any{"type": "ready", "tools": []any{"eval"}})

	// Build enough content to scroll.
	for i := 0; i < 8; i++ {
		v.send(fmt.Sprintf("msg %d", i))
		resp := fmt.Sprintf("Reply %d:\n", i)
		for j := 0; j < 3; j++ {
			resp += fmt.Sprintf("  line %d.%d\n", i, j)
		}
		v.event(map[string]any{"type": "token", "text": resp})
		v.event(map[string]any{"type": "done"})
	}

	// Scroll up.
	v.setYOffset(10)
	v.setScrollPin(false)

	// Verify overlay is showing.
	out := v.view()
	lines := strings.Split(out, "\n")
	if !strings.Contains(lines[0], "›") {
		t.Fatalf("scrolled up: header should show overlay; got %q", lines[0])
	}

	// New content arrives while scrolled — a token event triggers refresh()
	// but does NOT re-pin (only done/error do that).
	v.event(map[string]any{"type": "token", "text": "new content line 1\nnew content line 2\n"})

	// The overlay should still show — refresh() should not have re-pinned.
	m := v.model()
	if m.scrollPinned {
		t.Fatal("token event should not re-pin when scrollPinned is false")
	}

	out = v.view()
	lines = strings.Split(out, "\n")
	firstLine := lines[0]
	if !strings.Contains(firstLine, "›") {
		t.Errorf("after content grows: header should still show overlay; got %q", firstLine)
	}
}

// TestVtui_ScrollOverlaySwitchesMessages verifies that when two user messages
// are separated by lots of content, scrolling past each one updates the overlay
// to show the correct off-screen message. This caught a bug where the overlay
// stuck on the first off-screen message and never switched to the next one.
func TestVtui_ScrollOverlaySwitchesMessages(t *testing.T) {
	v := newVtui(80, 15)
	v.event(map[string]any{"type": "ready", "tools": []any{"eval"}})

	// Build a conversation with two user messages separated by lots of content.
	// Use wide lines so each wraps to exactly one row — we need predictable
	// row offsets for the scrollOverlay math.
	wideLine := func(n int) string {
		return strings.Repeat("x", 70) + fmt.Sprintf(" line %d\n", n)
	}

	v.send("first")
	resp1 := ""
	for i := 0; i < 30; i++ {
		resp1 += wideLine(i)
	}
	v.event(map[string]any{"type": "token", "text": resp1})
	v.event(map[string]any{"type": "done"})

	v.send("second")
	resp2 := ""
	for i := 0; i < 30; i++ {
		resp2 += wideLine(i)
	}
	v.event(map[string]any{"type": "token", "text": resp2})
	v.event(map[string]any{"type": "done"})

	// Scroll up progressively and verify the overlay switches.
	var overlays []string
	for yOff := 5; yOff <= 110; yOff += 5 {
		v.setYOffset(yOff)
		v.setScrollPin(false)
		overlay := v.model().scrollOverlay()
		if overlay != "" {
			overlays = append(overlays, stripANSI(overlay))
		}
	}

	if len(overlays) == 0 {
		t.Fatal("expected at least one overlay, got none")
	}

	// Find which messages appeared in the overlay.
	hasFirst := false
	hasSecond := false
	for _, o := range overlays {
		if strings.Contains(o, "first") {
			hasFirst = true
		}
		if strings.Contains(o, "second") {
			hasSecond = true
		}
	}

	if !hasSecond {
		t.Errorf("overlay never showed 'second'; got: %v", overlays)
	}
	if !hasFirst {
		t.Errorf("overlay never showed 'first'; got: %v", overlays)
	}

	// As yOff increases (scrolling further up), the overlay should show 'first'
	// (the earlier message, already off-screen) then switch to 'second' once
	// it too scrolls off the top of the viewport.
	firstMsg := ""
	secondMsg := ""
	for _, o := range overlays {
		if strings.Contains(o, "first") && firstMsg == "" {
			firstMsg = "first"
		}
		if strings.Contains(o, "second") && firstMsg != "" && secondMsg == "" {
			secondMsg = "second"
		}
	}
	if firstMsg != "first" || secondMsg != "second" {
		t.Errorf("overlay should show 'first' then 'second' as scroll increases, got order: %v", overlays)
	}
}

// TestVtui_ScrollOverlaySwitchesAfterResume verifies the overlay switches
// between two user messages after a resume (history replay) + scroll cycle.
// This caught a bug where the overlay stuck on one message after resume
// and never updated when scrolling past the other.
func TestVtui_ScrollOverlaySwitchesAfterResume(t *testing.T) {
	v := newVtui(80, 15)
	v.event(map[string]any{"type": "ready", "tools": []any{"eval"}})

	wideLine := func(n int) string {
		return strings.Repeat("x", 70) + fmt.Sprintf(" line %d\n", n)
	}

	// Simulate a history replay with two user messages separated by content.
	items := make([]any, 0)
	items = append(items, map[string]any{"kind": "user", "content": "first"})
	resp1 := ""
	for i := 0; i < 30; i++ {
		resp1 += wideLine(i)
	}
	items = append(items, map[string]any{"kind": "assistant", "content": resp1})
	items = append(items, map[string]any{"kind": "user", "content": "second"})
	resp2 := ""
	for i := 0; i < 30; i++ {
		resp2 += wideLine(i)
	}
	items = append(items, map[string]any{"kind": "assistant", "content": resp2})
	v.event(map[string]any{"type": "history", "items": items})

	// Scroll up, then resume (pin + refresh), then scroll up again.
	// The overlay should still switch between messages on the second scroll.

	// First scroll — verify overlay works.
	v.setYOffset(63)
	v.setScrollPin(false)
	overlay := stripANSI(v.model().scrollOverlay())
	if !strings.Contains(overlay, "second") {
		t.Logf("first scroll overlay: %q", overlay)
	}

	// Resume: pin and refresh (simulates new content arriving or user scrolling to bottom).
	v.setScrollPin(true)
	v.m.refresh()

	// Second scroll — overlay should still switch.
	v.setYOffset(5)
	v.setScrollPin(false)
	overlay = stripANSI(v.model().scrollOverlay())
	if !strings.Contains(overlay, "first") {
		t.Errorf("after resume, low scroll: overlay should show 'first', got %q", overlay)
	}

	v.setYOffset(63)
	v.setScrollPin(false)
	overlay = stripANSI(v.model().scrollOverlay())
	if !strings.Contains(overlay, "second") {
		t.Errorf("after resume, high scroll: overlay should show 'second', got %q", overlay)
	}
}

// TestVtui_ScrollOverlaySwitchesViaMouseWheelAfterReplay verifies the overlay
// switches between two user messages when scrolling via mouse wheel after a
// history replay (the /reload path). This caught a bug where the overlay stuck
// on one message and never updated as the user scrolled further up.
func TestVtui_ScrollOverlaySwitchesViaMouseWheelAfterReplay(t *testing.T) {
	v := newVtui(80, 15)
	v.event(map[string]any{"type": "ready", "tools": []any{"eval"}})

	wideLine := func(n int) string {
		return strings.Repeat("x", 70) + fmt.Sprintf(" line %d\n", n)
	}

	// Simulate a history replay with two user messages separated by content.
	items := make([]any, 0)
	items = append(items, map[string]any{"kind": "user", "content": "first"})
	resp1 := ""
	for i := 0; i < 30; i++ {
		resp1 += wideLine(i)
	}
	items = append(items, map[string]any{"kind": "assistant", "content": resp1})
	items = append(items, map[string]any{"kind": "user", "content": "second"})
	resp2 := ""
	for i := 0; i < 30; i++ {
		resp2 += wideLine(i)
	}
	items = append(items, map[string]any{"kind": "assistant", "content": resp2})
	v.event(map[string]any{"type": "history", "items": items})

	// Scroll up via mouse wheel, collecting overlays at each step.
	// One wheel click = 3 lines. Scroll enough to pass both messages.
	var seen []string
	for i := 0; i < 30; i++ {
		msg := tea.MouseMsg{Button: tea.MouseButtonWheelUp, Action: tea.MouseActionPress}
		nm, _ := v.m.Update(msg)
		v.m = nm.(tuiModel)

		overlay := v.m.scrollOverlay()
		if overlay != "" {
			plain := stripANSI(overlay)
			if len(seen) == 0 || seen[len(seen)-1] != plain {
				seen = append(seen, plain)
			}
		}
	}

	if len(seen) < 2 {
		t.Fatalf("expected overlay to show at least two different messages while scrolling, got %v", seen)
	}

	// Verify both messages appeared.
	hasFirst, hasSecond := false, false
	for _, s := range seen {
		if strings.Contains(s, "first") {
			hasFirst = true
		}
		if strings.Contains(s, "second") {
			hasSecond = true
		}
	}
	if !hasFirst {
		t.Errorf("overlay never showed 'first'; saw: %v", seen)
	}
	if !hasSecond {
		t.Errorf("overlay never showed 'second'; saw: %v", seen)
	}
}

// TestVtui_ViewFitsTerminal pins the frame's row budget. Bubble Tea's renderer
// keeps the LAST h lines of whatever View returns, so one row of overdraw costs
// the header — the row the scroll overlay is drawn in. That is invisible in any
// test that calls scrollOverlay() directly, which is what every other overlay
// test here does.
func TestVtui_ViewFitsTerminal(t *testing.T) {
	const w, h = 80, 20
	for _, tc := range []struct {
		name string
		prep func(v *vtui)
	}{
		{"bare", func(*vtui) {}},
		{"after a send", func(v *vtui) { v.send("do the thing") }},
		// The activity strip is what still takes a row off the top, now that the
		// task line — the original overdraw — is gone.
		{"activity strip", func(v *vtui) {
			v.event(map[string]any{"type": "agents", "agents": []any{
				map[string]any{"n": 1.0, "state": "running", "prompt": "dig"},
			}})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := newVtui(w, h)
			v.event(map[string]any{"type": "ready", "tools": []any{"eval"}})
			tc.prep(v)
			if got := lipgloss.Height(v.view()); got != h {
				t.Errorf("View() drew %d rows into a %d-row terminal", got, h)
			}
			if v.m.vp.Height != v.m.convoHeight() {
				t.Errorf("viewport height %d != drawn height %d — AtBottom/GotoBottom "+
					"clamp against the wrong window", v.m.vp.Height, v.m.convoHeight())
			}
		})
	}
}

// TestVtui_ScrollOverlayReachesTheScreen scrolls with the wheel and reads the
// overlay off the TOP ROW OF THE RENDERED FRAME, not from scrollOverlay(). With
// a task line present the frame used to overdraw by one row, so the renderer
// dropped the overlay entirely and the top row the user saw was the task line —
// which never changes while scrolling, and read as "the indicator is stuck".
func TestVtui_ScrollOverlayReachesTheScreen(t *testing.T) {
	v := newVtui(80, 15)
	v.event(map[string]any{"type": "ready", "tools": []any{"eval"}})

	wideLine := func(n int) string {
		return strings.Repeat("x", 70) + fmt.Sprintf(" line %d\n", n)
	}
	body := func() string {
		var b strings.Builder
		for i := 0; i < 30; i++ {
			b.WriteString(wideLine(i))
		}
		return b.String()
	}
	// Resume: history replay sets the task line, the way /reload does.
	v.event(map[string]any{"type": "history", "items": []any{
		map[string]any{"kind": "user", "content": "FIRSTMSG"},
		map[string]any{"kind": "assistant", "content": body()},
		map[string]any{"kind": "user", "content": "SECONDMSG"},
		map[string]any{"kind": "assistant", "content": body()},
	}})

	// What the terminal actually shows: Bubble Tea's renderer keeps the last h
	// lines of the frame (standard_renderer.go), so an overdrawn frame loses its
	// top row before anyone sees it — see screen().
	var tops []string
	for i := 0; i < 25; i++ {
		nm, _ := v.m.Update(tea.MouseMsg{Button: tea.MouseButtonWheelUp, Action: tea.MouseActionPress})
		v.m = nm.(tuiModel)
		if top := topRow(v); len(tops) == 0 || tops[len(tops)-1] != top {
			tops = append(tops, top)
		}
	}

	joined := strings.Join(tops, " | ")
	if !strings.Contains(joined, "SECONDMSG") {
		t.Errorf("top row never showed SECONDMSG while scrolling: %s", joined)
	}
	if !strings.Contains(joined, "FIRSTMSG") {
		t.Errorf("top row never showed FIRSTMSG while scrolling: %s", joined)
	}
	if len(tops) < 2 {
		t.Errorf("top row never changed while scrolling past both messages: %s", joined)
	}
}

// bigConvo is a resume-sized conversation: two user messages with a lot of
// content between and after them.
func bigConvo(w, h int) *vtui {
	v := newVtui(w, h)
	v.event(map[string]any{"type": "ready", "tools": []any{"eval"}})
	var b strings.Builder
	for i := 0; i < 60; i++ {
		b.WriteString(strings.Repeat("x", w-10) + fmt.Sprintf(" line %d\n", i))
	}
	v.event(map[string]any{"type": "history", "items": []any{
		map[string]any{"kind": "user", "content": "FIRSTMSG"},
		map[string]any{"kind": "assistant", "content": b.String()},
		map[string]any{"kind": "user", "content": "SECONDMSG"},
		map[string]any{"kind": "assistant", "content": b.String()},
	}})
	return v
}

// TestVtui_ResizeKeepsScrollPosition — a resize must not move the reader. The
// viewport used to be rebuilt from scratch on every WindowSizeMsg, and a fresh
// one starts at YOffset 0, so each resize threw you to the top of the
// conversation. On a phone the soft keyboard opening and closing IS a resize
// and it fires constantly, which made a scrolled-up session unreadable.
func TestVtui_ResizeKeepsScrollPosition(t *testing.T) {
	v := bigConvo(80, 40)
	v.m.vp.SetYOffset(40)
	v.m.scrollPinned = false
	want, wantTop := v.m.vp.YOffset, topRow(v)

	v.resize(80, 20) // keyboard opens
	if got := v.m.vp.YOffset; got != want {
		t.Errorf("keyboard open moved the reader: YOffset %d → %d", want, got)
	}
	if got := topRow(v); got != wantTop {
		t.Errorf("keyboard open changed the top row: %q → %q", wantTop, got)
	}

	v.resize(80, 40) // keyboard closes
	if got := v.m.vp.YOffset; got != want {
		t.Errorf("keyboard close moved the reader: YOffset %d → %d", want, got)
	}
}

// TestVtui_PinnedStaysAtBottom — pinned means pinned, including across the
// resize that changes how many rows the conversation gets.
func TestVtui_PinnedStaysAtBottom(t *testing.T) {
	v := bigConvo(80, 40)
	// The history event added the task line, which costs the conversation a
	// row. refresh() had already scrolled using the previous frame's height, so
	// the viewport sat one row short of the bottom and hid the newest line.
	if !v.m.vp.AtBottom() {
		t.Errorf("pinned but not at the bottom after a resume: YOffset=%d height=%d",
			v.m.vp.YOffset, v.m.vp.Height)
	}
	for _, h := range []int{20, 40, 12, 40} {
		v.resize(80, h)
		if !v.m.vp.AtBottom() {
			t.Errorf("pinned but not at the bottom after resizing to h=%d: YOffset=%d height=%d",
				h, v.m.vp.YOffset, v.m.vp.Height)
		}
	}
}

// topRow is what the terminal shows on its first line — see screen() for why
// that is not simply the first line View returned.
func topRow(v *vtui) string { return strings.TrimRight(screen(v)[0], " ") }

// TestVtui_WrapFitsWithoutCuttingText — refresh() skips wrapping a block whose
// widest line already fits the pane. If that check is ever wrong the damage is
// silent: the viewport truncates over-width lines (MaxWidth in its View), so
// the text is not pushed off the edge, it is simply gone.
//
// The oracle is independent of the caching: whatever refresh decided, each
// block's recorded height must equal the height of that block correctly
// wrapped to the pane.
func TestVtui_WrapFitsWithoutCuttingText(t *testing.T) {
	v := newVtui(80, 12)
	v.event(map[string]any{"type": "ready", "tools": []any{"eval"}})
	var b strings.Builder
	for i := 0; i < 40; i++ {
		b.WriteString(strings.Repeat("word ", 14) + fmt.Sprintf("line %d\n", i))
	}
	b.WriteString(strings.Repeat("Z", 120) + "\n") // a token no width can break on
	v.event(map[string]any{"type": "history", "items": []any{
		map[string]any{"kind": "user", "content": "FIRSTMSG"},
		map[string]any{"kind": "assistant", "content": b.String()},
	}})

	// Narrow, widen, and come back: the cache has to be right in every
	// direction, not just on the way down.
	for _, w := range []int{80, 79, 60, 40, 24, 100, 40, 80} {
		v.resize(w, 12)
		for i := range v.m.convo {
			e := &v.m.convo[i]
			if e.userText != "" || e.docs != nil {
				continue // rendered with a full-width background instead
			}
			want := lipgloss.Height(cellbuf.Wrap(e.view(), max(1, v.m.vp.Width), ""))
			if got := v.m.blockH[i]; got != want {
				t.Errorf("w=%d entry %d: recorded height %d, correctly-wrapped height %d — text is being cut off",
					w, i, got, want)
			}
		}
	}
}

// TestVtui_MarkdownReflowsOnResize — a finalized assistant message is rendered
// through glamour exactly once, and that render bakes in the word-wrap width
// that was current when it happened. A pre-wrapped line has no break
// opportunity left for cellbuf to find on a narrower pane, so without
// re-rendering from mdSource a resize down leaves over-width lines that the
// viewport silently truncates (MaxWidth): the text is not pushed off the edge,
// it is simply gone. This test resizes in both directions and asserts no
// rendered line ever exceeds the window.
func TestVtui_MarkdownReflowsOnResize(t *testing.T) {
	v := newVtui(100, 24)
	v.event(map[string]any{"type": "ready", "tools": []any{"eval"}})
	md := strings.Repeat(
		"this is a reasonably long markdown line that should wrap when the window gets narrower than it is wide\n\n", 3)
	for _, chunk := range []string{md[:100], md[100:]} {
		v.event(map[string]any{"type": "token", "text": chunk})
	}
	v.event(map[string]any{"type": "done"})
	// Narrow, widen, narrow again: the re-render has to be right in every
	// direction, not just on the way down.
	for _, w := range []int{100, 60, 30, 80} {
		v.resize(w, 24)
		for i, l := range v.m.vp.lines {
			if cw := lipgloss.Width(l); cw > w {
				t.Errorf("w=%d row %d: rendered line is %d cells wide — text is being cut off", w, i, cw)
			}
		}
	}
}

// TestVtui_MarkdownTokenDoesNotReRender — F1: the re-render branch must fire on
// a WIDTH change only. A streamed token between two resizes triggers a refresh
// (via the render tick) at the same width, and it must not pay for a glamour
// re-render of any finalized block — that is the per-token invariant the wrap
// cache exists for. The oracle is the rendered string itself: if refresh
// re-rendered from mdSource, e.collapsed would be rebuilt (same input, but we
// pin it by identity across the token).
func TestVtui_MarkdownTokenDoesNotReRender(t *testing.T) {
	v := newVtui(100, 24)
	v.event(map[string]any{"type": "ready", "tools": []any{"eval"}})
	md := strings.Repeat(
		"this is a reasonably long markdown line that should wrap when the window gets narrower than it is wide\n\n", 3)
	for _, chunk := range []string{md[:100], md[100:]} {
		v.event(map[string]any{"type": "token", "text": chunk})
	}
	v.event(map[string]any{"type": "done"})
	e := &v.m.convo[len(v.m.convo)-1]
	if e.mdSource == "" {
		t.Fatal("the finalized block has no mdSource — the test is not exercising the fix")
	}
	// Move to a width where the branch has already run once, so wrapW is set.
	v.resize(60, 24)
	if v.m.convo[len(v.m.convo)-1].wrapW == 0 {
		t.Fatalf("wrapW was not set after a resize — the cache is not being filled (mdSource=%q)",
			v.m.convo[len(v.m.convo)-1].mdSource)
	}
	e = &v.m.convo[len(v.m.convo)-1]
	before := e.collapsed
	// Stream another token at the SAME width: refresh runs (render tick), but
	// no mdSource block may be re-rendered. m.cur grows, so vp.lines change —
	// the finalized block's own render must not.
	v.event(map[string]any{"type": "token", "text": "more"})
	nm, _ := v.m.Update(renderTickMsg{})
	v.m = nm.(tuiModel)
	e = &v.m.convo[len(v.m.convo)-1]
	if e.collapsed != before {
		t.Error("a token at unchanged width re-rendered the finalized markdown — the branch must fire on width change only")
	}
}

// TestVtui_MarkdownReflowFromZeroWrap — F1: the `e.wrapW == 0` clause of the
// re-render branch. A block appended while the pane is at width W gets
// measured at W on its first refresh (wrapW := W), so the zero clause is
// reached only when a refresh happens at a DIFFERENT width than the one that
// last measured it — which is exactly what a resize does, and what this test
// drives: every resize must leave no line wider than the window, in both
// directions.
func TestVtui_MarkdownReflowFromZeroWrap(t *testing.T) {
	v := newVtui(100, 24)
	v.event(map[string]any{"type": "ready", "tools": []any{"eval"}})
	md := strings.Repeat(
		"this is a reasonably long markdown line that should wrap when the window gets narrower than it is wide\n\n", 3)
	for _, chunk := range []string{md[:100], md[100:]} {
		v.event(map[string]any{"type": "token", "text": chunk})
	}
	v.event(map[string]any{"type": "done"})
	e := &v.m.convo[len(v.m.convo)-1]
	if e.mdSource == "" {
		t.Fatal("the finalized block has no mdSource — the test is not exercising the fix")
	}
	// The entry was measured at 100 on the done-refresh; a resize to any other
	// width must re-render it, and the result must fit.
	for _, w := range []int{60, 30, 100, 45} {
		v.resize(w, 24)
		for i, l := range v.m.vp.lines {
			if cw := lipgloss.Width(l); cw > w {
				t.Errorf("w=%d row %d: rendered line is %d cells wide — text is being cut off", w, i, cw)
			}
		}
	}
}

// TestVtui_MarkdownReflowsOnResizeFocused — F2: in conversation focus the pane
// reserves one column for the gutter, so the effective wrap width is vp.Width-1
// while m.md still wraps at the terminal width. The two differ by a few cells,
// and this file has been burned by off-by-a-few-cells before — pin that no
// rendered line exceeds the WINDOW in either focus state, across resizes.
func TestVtui_MarkdownReflowsOnResizeFocused(t *testing.T) {
	v := newVtui(100, 24)
	v.event(map[string]any{"type": "ready", "tools": []any{"eval"}})
	md := strings.Repeat(
		"this is a reasonably long markdown line that should wrap when the window gets narrower than it is wide\n\n", 3)
	for _, chunk := range []string{md[:100], md[100:]} {
		v.event(map[string]any{"type": "token", "text": chunk})
	}
	v.event(map[string]any{"type": "done"})
	for _, focus := range []int{focusInput, focusConvo} {
		v.m.focus = focus
		if focus == focusConvo {
			v.m.sel = len(v.m.convo) - 1 // a selection makes selMode real
		}
		for _, w := range []int{100, 60, 30, 80} {
			v.resize(w, 24)
			for i, l := range v.m.vp.lines {
				if cw := lipgloss.Width(l); cw > w {
					t.Errorf("focus=%d w=%d row %d: rendered line is %d cells wide — text is being cut off", focus, w, i, cw)
				}
			}
		}
	}
}

// enterAgentScopeByKeys descends into a child using only the keys a person
// presses — the existing scope test sets focus and actSel by hand, which
// proves leaveAgentScope works but not that anyone can reach it.
func enterAgentScopeByKeys(t *testing.T) tuiModel {
	t.Helper()
	m := withActivity(t)
	m = typeStr(m, "the root task")
	m = key(m, kEnter)
	m = key(m, kTab) // input → convo
	m = key(m, kTab) // convo → activity
	if m.focus != focusActivity {
		t.Fatalf("tab twice should reach the activity zone, focus=%d", m.focus)
	}
	m = key(m, kRight) // collapsed → list
	m = key(m, kRight) // descend into the selected agent
	if m.scopeAgent == 0 {
		t.Fatal("could not enter agent scope with keys")
	}
	return m
}

// You must be able to get back out of a child, by the keys the screen names.
// Descending left the user in a conversation with no exit: ← only cycled focus
// round and round, and esc — the other thing anyone tries — QUIT dun.
func TestActivity_LeavingAgentScope(t *testing.T) {
	for _, tc := range []struct {
		name string
		k    tea.KeyMsg
	}{
		{"left", tea.KeyMsg{Type: tea.KeyLeft}},
		{"esc", kEsc},
	} {
		t.Run("from the input, "+tc.name, func(t *testing.T) {
			m := enterAgentScopeByKeys(t)
			m = key(m, tc.k)
			if m.scopeAgent != 0 {
				t.Errorf("%s did not leave agent scope", tc.name)
			}
			if m.quitting {
				t.Errorf("%s quit dun instead of leaving the child", tc.name)
			}
			if m.task != "the root task" {
				t.Errorf("the root conversation did not come back: task %q", m.task)
			}
		})
	}

	// From the conversation, ← still closes open blocks first; it only leaves
	// the scope once there is nothing left to ascend inside it.
	t.Run("from the conversation, left", func(t *testing.T) {
		m := enterAgentScopeByKeys(t)
		m = key(m, kTab) // input → convo
		if m.focus != focusConvo {
			t.Fatalf("focus=%d, want the conversation", m.focus)
		}
		m = key(m, tea.KeyMsg{Type: tea.KeyLeft})
		if m.scopeAgent != 0 {
			t.Error("← from the child's conversation did not go back to the session")
		}
	})

	// And the strip's own way back still works, and says how to do it.
	t.Run("via the activity strip", func(t *testing.T) {
		m := enterAgentScopeByKeys(t)
		m = key(m, kTab)
		m = key(m, kTab)
		m = key(m, kRight) // open the strip
		m = key(m, kRight) // descend into "parent"
		if m.scopeAgent != 0 {
			t.Error("the strip's parent row did not leave agent scope")
		}
	})
}

// esc still quits when there is no scope to leave — the exit path must not
// have been traded away for the fix above.
func TestTUI_EscStillQuitsOutsideAgentScope(t *testing.T) {
	m := withActivity(t)
	m = key(m, kEsc)
	if !m.quitting {
		t.Error("esc outside agent scope should still quit")
	}
}

// The pre-conversation cost must be readable BEFORE the first turn. It used to
// arrive only on the usage event, which fires after a turn completes — so a
// session that had run nothing but tool calls showed "not reported" for a number
// the harness computed at startup, which is exactly when somebody asks.
func TestTUI_ContextCostArrivesWithoutACompletedTurn(t *testing.T) {
	m := newTUIModel(&dunProc{}, "/ws")
	m = m.handleEvent(evMsg{
		"type": "context_cost", "system_tokens": float64(638), "system_exact": true,
		"system_prompt": float64(11), "system_shared": float64(497),
		"system_parts": []any{
			map[string]any{"name": "built-in tools", "tokens": float64(44)},
			map[string]any{"name": "mcp: raglit", "tokens": float64(43)},
			map[string]any{"name": "mcp: chrome", "tokens": float64(43)},
		},
	})

	s := m.ctxStats
	if s.systemTokens != 638 || !s.systemExact || s.systemPrompt != 11 || s.systemShared != 497 {
		t.Fatalf("context_cost not applied: %+v", s)
	}
	if len(s.systemParts) != 3 {
		t.Fatalf("want 3 rows, got %v", s.systemParts)
	}
	sum := s.systemPrompt + s.systemShared
	for _, p := range s.systemParts {
		sum += p.Tokens
	}
	if sum != s.systemTokens {
		t.Errorf("rows sum to %d but total says %d", sum, s.systemTokens)
	}
}

// The estimate lands first and the exact count replaces it. Both carry the same
// fields, so a later event must overwrite rather than merge — a stale estimated
// row surviving beside exact ones is the mixed table the design refuses.
func TestTUI_ExactCostReplacesTheEstimate(t *testing.T) {
	m := newTUIModel(&dunProc{}, "/ws")
	m = m.handleEvent(evMsg{
		"type": "context_cost", "system_tokens": float64(900), "system_exact": false,
		"system_prompt": float64(20),
		"system_parts":  []any{map[string]any{"name": "built-in tools", "tokens": float64(880)}},
	})
	m = m.handleEvent(evMsg{
		"type": "context_cost", "system_tokens": float64(638), "system_exact": true,
		"system_prompt": float64(11), "system_shared": float64(497),
		"system_parts": []any{map[string]any{"name": "built-in tools", "tokens": float64(130)}},
	})
	if !m.ctxStats.systemExact || m.ctxStats.systemTokens != 638 {
		t.Fatalf("exact count did not replace the estimate: %+v", m.ctxStats)
	}
	if len(m.ctxStats.systemParts) != 1 || m.ctxStats.systemParts[0].Tokens != 130 {
		t.Errorf("stale estimated rows survived: %v", m.ctxStats.systemParts)
	}
}

// readSideCalls replaces the TUI's copy on every usage event — the engine
// sends the session total, not a delta. A second event must not double the
// first, and an event without the field (an older engine) must not clear a
// newer engine's numbers... or wait, it must: the map is the whole truth, and
// an engine that stopped sending it is an engine that has no side calls.
func TestSideCalls_ReadReplacesAndToleratesMissing(t *testing.T) {
	var s contextStats
	s.readSideCalls(map[string]any{"side_calls": map[string]any{
		"suggest": map[string]any{"calls": 2.0, "latency_ms": 600.0, "last_ms": 300.0,
			"processed": 80.0, "cached": 120.0, "generated": 80.0, "avg_ms": 300.0, "tok_per_s": 133.0},
	}})
	if got := s.sideCalls["suggest"].calls; got != 2 {
		t.Fatalf("first read: calls = %d, want 2", got)
	}
	// Same kind reported again with a bigger total: replace, don't sum.
	s.readSideCalls(map[string]any{"side_calls": map[string]any{
		"suggest": map[string]any{"calls": 3.0, "latency_ms": 900.0, "last_ms": 300.0,
			"processed": 120.0, "cached": 180.0, "generated": 120.0, "avg_ms": 300.0, "tok_per_s": 133.0},
	}})
	if got := s.sideCalls["suggest"].calls; got != 3 {
		t.Fatalf("second read must replace, not accumulate: calls = %d", got)
	}
	// A malformed row is dropped, not fatal.
	s.readSideCalls(map[string]any{"side_calls": map[string]any{"commit": "bogus"}})
	if len(s.sideCalls) != 0 {
		t.Fatalf("bogus row must not survive: %v", s.sideCalls)
	}
	// No field at all: an older engine (or a turn from before the first side
	// call). The read leaves the previous snapshot in place — the last real
	// numbers are still the best knowledge, and a section that blinks away on
	// every turn is worse than one that lags.
	s.readSideCalls(map[string]any{"side_calls": map[string]any{
		"suggest": map[string]any{"calls": 3.0},
	}})
	s.readSideCalls(map[string]any{"total": 10})
	if got := s.sideCalls["suggest"].calls; got != 3 {
		t.Fatalf("missing field must not erase the last real snapshot: calls = %d", got)
	}
}

// sideCallBlock renders one row per kind, in a stable (sorted) order, with the
// kind's plain-English meaning — a reader of /context should not have to know
// which helper "rescue" names.
func TestSideCallBlock_Renders(t *testing.T) {
	var s contextStats
	s.readSideCalls(map[string]any{"side_calls": map[string]any{
		"commit": map[string]any{"calls": 1.0, "latency_ms": 1200.0, "last_ms": 1200.0,
			"processed": 500.0, "cached": 400.0, "generated": 60.0, "avg_ms": 1200.0, "tok_per_s": 50.0},
		"suggest": map[string]any{"calls": 2.0, "latency_ms": 600.0, "last_ms": 300.0,
			"processed": 80.0, "cached": 120.0, "generated": 80.0, "avg_ms": 300.0, "tok_per_s": 133.0},
	}})
	out := sideCallBlock(&s)
	if out == "" {
		t.Fatal("side calls present but the block is empty")
	}
	for _, want := range []string{"side calls", "commit", "suggest",
		"commit messages (/worktree commit)", "next-message predictions (/suggest)",
		"1 call", "2 calls", "avg 1200 ms", "last 1200 ms", "last 300 ms",
		"500 proc / 400 cached / 60 gen", "50 tok/s", "133 tok/s"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in block:\n%s", want, out)
		}
	}
	// Stable order: commit before suggest, every time.
	if strings.Index(out, "commit") > strings.Index(out, "suggest") {
		t.Errorf("rows must be sorted: %s", out)
	}

	var empty contextStats
	if got := sideCallBlock(&empty); got != "" {
		t.Errorf("no side calls means no section: %q", got)
	}
}

// /context shows the side-call section: the usage event is what carries it in.
func TestContext_ShowsSideCalls(t *testing.T) {
	var buf bytes.Buffer
	m := newTUIModel(&dunProc{stdin: bufCloser{&buf}}, "/ws")
	m.ctxStats.readSideCalls(map[string]any{"side_calls": map[string]any{
		"rephrase": map[string]any{"calls": 1.0, "latency_ms": 400.0, "last_ms": 400.0,
			"processed": 200.0, "cached": 100.0, "generated": 40.0, "avg_ms": 400.0, "tok_per_s": 100.0},
	}})
	m.convo = []convoEntry{{collapsed: "old"}}
	m.showContext()
	last := m.convo[len(m.convo)-1].collapsed
	for _, want := range []string{"side calls", "rephrase", "prompt rewrites (/rephrase)", "1 call"} {
		if !strings.Contains(last, want) {
			t.Errorf("/context missing %q:\n%s", want, last)
		}
	}
}
