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
)

// discardWC is a stand-in engine stdin: answers/sends go nowhere.
type discardWC struct{}

func (discardWC) Write(p []byte) (int, error) { return len(p), nil }
func (discardWC) Close() error                { return nil }

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
	if !strings.Contains(m.convoText(), "blue") {
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
	if !strings.Contains(m.convoText(), "A, C") {
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
	txt := m.convoText()
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
	m = typeStr(m, "co")
	if ms := m.paletteMatches(); len(ms) != 1 || ms[0].name != "config" {
		t.Fatalf("/co should match only config, got %v", ms)
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
	txt := m.convoText()
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
	// digit "1" sends the first suggestion.
	m = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("1")})
	if !strings.Contains(m.convoText(), "run the tests") {
		t.Fatalf("digit 1 should send the first suggestion, convo: %s", m.convoText())
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
	// right from an empty input opens the suggestion selector.
	m = m.handleEvent(evMsg{"type": "suggestions", "items": []any{
		map[string]any{"text": "alpha", "prob": 0.6},
		map[string]any{"text": "bravo", "prob": 0.4},
	}})
	m = key(m, kRight)
	if !m.suggestSelecting || m.suggestSel != 0 {
		t.Fatalf("right from empty input should open the selector, got selecting=%v sel=%d", m.suggestSelecting, m.suggestSel)
	}
	m = key(m, kDown)
	if m.suggestSel != 1 {
		t.Fatalf("↓ should move the selection, got %d", m.suggestSel)
	}
	// left closes the selector (doesn't hop panes).
	if closed := key(m, kLeft); closed.suggestSelecting || closed.focus != focusInput {
		t.Fatal("left should close the selector and stay on the input")
	}
	// enter sends the highlighted suggestion.
	m = key(m, kEnter)
	if !strings.Contains(m.convoText(), "bravo") {
		t.Fatalf("enter should send the selected suggestion, convo: %s", m.convoText())
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
	if !strings.Contains(m.convoText(), "unknown command") {
		t.Fatalf("expected unknown-command note, got: %s", m.convoText())
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
	if !strings.Contains(m.convoText(), "session cleared") {
		t.Fatalf("expected clear confirmation, got: %s", m.convoText())
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

// The retry UX. The user's report was "a connection error and dun dies, no retry,
// no info": the retries WERE happening, inside the LLM client, reported only to a
// log the TUI writes to a temp file. These assert the wait is on screen.
func TestTUI_RetryBanner(t *testing.T) {
	m := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	m.w, m.h = 100, 30
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
	if !strings.Contains(stripANSI(m.convoText()), "provider at capacity") {
		t.Errorf("first retry not recorded in scrollback:\n%s", stripANSI(m.convoText()))
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
	if !strings.Contains(stripANSI(m.convoText()), "recovered on attempt 5") {
		t.Error("recovery not recorded")
	}
}

// A turn-scope retry means the generation died mid-stream and will be redone, so
// the half-streamed text must go — otherwise the regenerated reply appends to a
// broken sentence.
func TestTUI_TurnRetryDiscardsPartialReply(t *testing.T) {
	m := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	m.w, m.h = 100, 30
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
	if strings.Contains(stripANSI(m.convoText()), "I'll start by rea") {
		t.Error("the discarded partial reply was finalized into the conversation")
	}
}

// Giving up is not the end of the session: the conversation is on disk, so the
// user is told that another message resumes from here.
func TestTUI_GiveUpKeepsSessionUsable(t *testing.T) {
	m := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	m.w, m.h = 100, 30
	m = m.handleEvent(evMsg{"type": "ready", "tools": []any{"eval"}})
	m.busy = true
	m = m.handleEvent(evMsg{"type": "retry", "kind": "giveup", "attempt": 5.0, "text": "gave up after 5m"})
	if m.busy {
		t.Error("giveup should clear busy so the input is usable")
	}
	m = m.handleEvent(evMsg{"type": "error", "error": "agent: chat: stream error"})
	if !strings.Contains(stripANSI(m.convoText()), "send a message to retry from here") {
		t.Errorf("no recovery hint after a failure:\n%s", stripANSI(m.convoText()))
	}
	// And the input accepts one.
	m = typeStr(m, "keep going")
	m = key(m, kEnter)
	if !strings.Contains(stripANSI(m.convoText()), "keep going") {
		t.Error("a message sent after a failure was dropped")
	}
}

// Typing while the agent works is allowed: the engine buffers the message and
// lifts it into the next tool result, so it lands in the RUNNING turn.
func TestTUI_SendWhileBusyQueues(t *testing.T) {
	m := newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	m.w, m.h = 100, 30
	m = m.handleEvent(evMsg{"type": "ready", "tools": []any{"eval"}})
	m.busy = true

	m = typeStr(m, "also update the README")
	m = key(m, kEnter)
	if strings.TrimSpace(m.input.Value()) != "" {
		t.Errorf("input not cleared; the message was refused while busy: %q", m.input.Value())
	}
	if !strings.Contains(stripANSI(m.convoText()), "also update the README") {
		t.Error("mid-turn message not echoed")
	}
	m = m.handleEvent(evMsg{"type": "queued", "text": "also update the README", "count": 1.0})
	if m.queuedMsgs != 1 {
		t.Errorf("queuedMsgs = %d; want 1", m.queuedMsgs)
	}
	if !strings.Contains(stripANSI(m.View()), "1 message queued") {
		t.Errorf("status line does not report the queued message:\n%s", stripANSI(m.View()))
	}
	// A second one batches with it.
	m = m.handleEvent(evMsg{"type": "queued", "text": "and the changelog", "count": 2.0})
	if !strings.Contains(stripANSI(m.View()), "2 messages queued") {
		t.Errorf("second queued message not counted:\n%s", stripANSI(m.View()))
	}
	// The turn ending clears it — the messages are the model's problem now.
	m = m.handleEvent(evMsg{"type": "done"})
	if m.queuedMsgs != 0 {
		t.Errorf("queuedMsgs = %d after done; want 0", m.queuedMsgs)
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
	if !strings.Contains(m.convoText(), "autostart on") {
		t.Fatalf("server reply not shown: %s", m.convoText())
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
	if !strings.Contains(m.convoText(), "/rag auto") {
		t.Fatalf("ready hint not shown: %s", m.convoText())
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
	if !strings.Contains(m.convoText(), "session is intact") {
		t.Errorf("a recoverable turn failure should say so: %s", m.convoText())
	}

	m = m.handleEvent(evMsg{"type": "error", "error": "context canceled", "fatal": true})
	txt := m.convoText()
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
	if !strings.Contains(m.convoText(), "restarting") {
		t.Errorf("the restart should be visible: %s", m.convoText())
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
	if !strings.Contains(m.convoText(), "dun --continue") {
		t.Errorf("the user should be told the conversation survived: %s", m.convoText())
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
	if !strings.Contains(m.convoText(), "reconnecting") {
		t.Errorf("the attempt should be visible: %s", m.convoText())
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
	if !strings.Contains(m.convoText(), "not sent") {
		t.Errorf("a dropped message must be reported: %s", m.convoText())
	}
	// And the slash commands that talk to the engine say so rather than panic.
	m.runSlash("/rag on")
	if !strings.Contains(m.convoText(), "no engine") {
		t.Errorf("server command should report the missing engine: %s", m.convoText())
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
	streaming := m.fullText()

	// Now finalize the same text and capture what convoText() produces.
	m.flushCur()
	finalized := m.convoText()

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
	m.vp.SetContent(m.fullText())

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
