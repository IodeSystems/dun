package main

import (
	"os"
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
	if !strings.Contains(txt, "commands") || !strings.Contains(txt, "/config") || !strings.Contains(txt, "/quit") {
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
	// /quit is a deliberate exit — still works.
	q := typeStr(m, "/quit")
	if _, cmd := q.Update(kEnter); cmd == nil {
		t.Fatal("/quit should exit even with disableExit")
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
