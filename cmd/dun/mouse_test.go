package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func mouse(m tuiModel, a mouseAction, b tea.MouseButton, x, y int) tuiModel {
	nm, _ := m.Update(mouseMsg(a, b, x, y))
	return nm.(tuiModel)
}

// click is a left press and release on one cell.
func click(m tuiModel, x, y int) tuiModel {
	m = mouse(m, mousePress, tea.MouseLeft, x, y)
	return mouse(m, mouseRelease, tea.MouseLeft, x, y)
}

// dclick is a double click: what selects in the conversation.
func dclick(m tuiModel, x, y int) tuiModel { return click(click(m, x, y), x, y) }

// rowY is the screen row of row k of block i, as the model last laid it out.
func rowY(m tuiModel, i, k int) int {
	return m.convoTopRow() + m.frame.rowOffset[i] + k - m.vp.YOffset
}

// onPane says row k of block i is inside the pane.
func onPane(m tuiModel, i, k int) bool {
	r := m.frame.rowOffset[i] + k - m.vp.YOffset
	return r >= 0 && r < m.vp.Height
}

func TestBlockAtRow(t *testing.T) {
	off, h := []int{0, 3, 4, 9}, []int{3, 1, 5, 2}
	want := []int{0, 0, 0, 1, 2, 2, 2, 2, 2, 3, 3, -1}
	for row, w := range want {
		if got := blockAtRow(off, h, row); got != w {
			t.Errorf("row %d: block %d, want %d", row, got, w)
		}
	}
	if got := blockAtRow(off, h, -1); got != -1 {
		t.Errorf("row -1: block %d, want -1", got)
	}
}

// mouseConvo is a conversation taller than its pane, with a wrapped block and
// an expandable one.
func mouseConvo(t *testing.T) (m tuiModel, wrapped, tog int) {
	m, wrapped, tog, _ = mouseConvoAt(t)
	return
}

// mouseConvoAt also says where "message 0" is.
func mouseConvoAt(t *testing.T) (m tuiModel, wrapped, tog, first int) {
	t.Helper()
	m = newTUIModel(&dunProc{stdin: discardWC{}}, "/ws")
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 16})
	m = nm.(tuiModel)
	m = m.handleEvent(evMsg{"type": "ready", "tools": []any{"eval"}})
	first = len(m.convo)
	for i := 0; i < 6; i++ {
		m.convo = append(m.convo, convoEntry{collapsed: fmt.Sprintf("message %d", i)})
	}
	wrapped = len(m.convo)
	m.convo = append(m.convo, convoEntry{collapsed: strings.Repeat("wrap me please ", 8)})
	tog = len(m.convo)
	m.convo = append(m.convo, convoEntry{collapsed: "toggle me", full: "toggle me\nfull 1\nfull 2"})
	for i := 6; i < 14; i++ {
		m.convo = append(m.convo, convoEntry{collapsed: fmt.Sprintf("message %d", i)})
	}
	m.refresh()
	return m, wrapped, tog, first
}

// A click selects the block under it, from the frame's row map and the pane's
// scroll offset, on any row of a wrapped block.
func TestMouse_ClickSelectsTheBlockUnderIt(t *testing.T) {
	m, wrapped, _ := mouseConvo(t)
	if m.frame.blockH[wrapped] < 2 {
		t.Fatalf("precondition: the wrapped block is %d rows", m.frame.blockH[wrapped])
	}
	clicked := map[int]bool{}
	for _, yoff := range []int{0, 3, 7, m.vp.maxYOffset()} {
		m.vp.SetYOffset(yoff)
		m.scrollPinned = m.vp.AtBottom()
		for i := len(m.convo) - 1; i >= 0; i-- { // newest first: a selection reflows the rows below it
			last := m.frame.blockH[i] - 1
			if !onPane(m, i, last) {
				continue
			}
			m = dclick(m, 5, rowY(m, i, last))
			clicked[i] = true
			if m.focus != focusConvo || m.sel != i {
				t.Fatalf("yoff %d: click on the last row of block %d: focus %d sel %d", yoff, i, m.focus, m.sel)
			}
			if m.bandShown() != band.on() {
				t.Fatalf("yoff %d: block %d selected, band shown %v", yoff, i, m.bandShown())
			}
			if m.drag.on {
				t.Fatal("a click must not leave a drag behind")
			}
		}
		m.setFocus(focusInput)
		m.refresh()
	}
	if len(clicked) != len(m.convo) || !clicked[wrapped] {
		t.Errorf("clicked %d of %d blocks (wrapped %v): %v", len(clicked), len(m.convo), clicked[wrapped], clicked)
	}
}

// A second click on the selected block is enter: it cycles the block's view.
func TestMouse_SecondClickToggles(t *testing.T) {
	m, _, tog := mouseConvo(t)
	m.vp.GotoBottom()
	for !onPane(m, tog, 0) {
		m.vp.SetYOffset(m.vp.YOffset - 1)
	}
	m.scrollPinned = false
	m = dclick(m, 5, rowY(m, tog, 0))
	if m.sel != tog || m.convo[tog].state != viewMinimized {
		t.Fatalf("first click selects only: sel %d state %v", m.sel, m.convo[tog].state)
	}
	m = dclick(m, 5, rowY(m, tog, 0))
	if m.convo[tog].state == viewMinimized {
		t.Fatal("second click should toggle the block open")
	}
	m = dclick(m, 5, rowY(m, tog, 0))
	if m.convo[tog].state != viewMinimized {
		t.Fatalf("third click should toggle it closed, state %v", m.convo[tog].state)
	}
}

// A click in an open box's body descends into it, so the wheel scrolls it; its
// header row only selects.
func TestMouse_ClickInBoxBodyDescends(t *testing.T) {
	v, think, _ := boxConvo(t, 40)
	m := v.m
	for !onPane(m, think, 0) {
		m.vp.SetYOffset(m.vp.YOffset - 1)
	}
	m.scrollPinned = false
	m = dclick(m, 5, rowY(m, think, 0))
	b := m.convo[think].box
	if m.sel != think || b.inside {
		t.Fatalf("header click: sel %d (want %d) inside %v", m.sel, think, b.inside)
	}
	m = dclick(m, 5, rowY(m, think, 2))
	if !b.inside || m.insideBox() != b {
		t.Fatal("a click in the body should descend into the box")
	}
	off := b.off
	m = mouse(m, mousePress, tea.MouseWheelUp, 5, rowY(m, think, 2))
	if b.off != off-wheelDelta {
		t.Errorf("inside, the wheel should scroll the box: %d → %d", off, b.off)
	}
	// From another block, a body click selects and descends in one go.
	m = dclick(m, 5, rowY(m, think-1, 0))
	if b.inside || m.sel == think {
		t.Fatalf("selecting another block should leave the box: sel %d inside %v", m.sel, b.inside)
	}
	m = dclick(m, 5, rowY(m, think, 2))
	if m.sel != think || !b.inside {
		t.Errorf("body click from elsewhere: sel %d inside %v", m.sel, b.inside)
	}
}

// A click on the input focuses it and puts the cursor under the pointer, on a
// wrapped second row too.
func TestMouse_ClickFocusesInput(t *testing.T) {
	m, _, _ := mouseConvo(t)
	m = dclick(m, 5, rowY(m, len(m.convo)-1, 0))
	if m.focus != focusConvo {
		t.Fatal("precondition: convo focus")
	}
	text := strings.Repeat("abcdefghij ", 6) // wraps at 40 columns
	m.input.SetValue(text)
	m.refresh()
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 16}) // re-sync heights
	m = nm.(tuiModel)
	if m.input.height() < 2 {
		t.Fatalf("precondition: the input wraps, height %d", m.input.height())
	}
	segs := m.input.segments()
	m = click(m, 2+3, m.lowerTopRow()+1)
	if m.focus != focusInput {
		t.Fatalf("click on the input: focus %d", m.focus)
	}
	if want := segs[1].start + 3; m.input.cursor != want {
		t.Errorf("cursor %d, want %d (row 1, col 3)", m.input.cursor, want)
	}
}

// The activity strip: a click opens the collapsed line, a click on a row
// selects it, a click on the selected row opens it.
func TestMouse_ClickActivity(t *testing.T) {
	m := withActivity(t)
	top := m.actTopRow()
	m = click(m, 3, top)
	if m.focus != focusActivity || m.actLevel != actList {
		t.Fatalf("collapsed click: focus %d level %d", m.focus, m.actLevel)
	}
	rows := m.actRows()
	m = click(m, 3, top+2) // head row, then rows[0], rows[1]
	if m.actSel != rows[1].key {
		t.Fatalf("row click selected %+v, want %+v", m.actSel, rows[1].key)
	}
	m = click(m, 3, top+2)
	if m.scopeAgent != rows[1].key.id {
		t.Errorf("second click should open agent %d, scope %d", rows[1].key.id, m.scopeAgent)
	}
}

// A press that moves is a drag: it highlights, copies on release, and any key
// clears it. It never selects a block.
func TestMouse_DragCopiesAndKeyClears(t *testing.T) {
	var wrote []string
	var ran []string
	fakeClip(t, map[string]string{}, &wrote, &ran, nil)

	m, _, _, first := mouseConvoAt(t)
	m.vp.SetYOffset(0)
	m.scrollPinned = false
	y0, y1 := rowY(m, first, 0), rowY(m, first+1, 0)
	m = mouse(m, mousePress, tea.MouseLeft, 0, y0)
	m = mouse(m, mouseMotion, tea.MouseLeft, 4, y1)
	if !m.drag.shown() {
		t.Fatal("motion off the press cell should start a highlight")
	}
	if !strings.Contains(m.View().Content, "\x1b[0;7m") {
		t.Error("the highlight should be drawn")
	}
	nm, cmd := m.Update(mouseMsg(mouseRelease, tea.MouseLeft, 4, y1))
	m = nm.(tuiModel)
	if m.focus != focusInput {
		t.Errorf("a drag must not select a block: focus %d sel %d", m.focus, m.sel)
	}
	if !m.drag.held || cmd == nil {
		t.Fatal("release after a drag should hold the highlight and copy")
	}
	nm, _ = m.Update(cmd())
	m = nm.(tuiModel)
	if m.flashText != "copied 15 chars → terminal" { // "message 0\nmessa"
		t.Errorf("flash %q", m.flashText)
	}
	if len(wrote) != 1 || wrote[0] != osc52("message 0\nmessa") || len(ran) != 0 {
		t.Errorf("outside tmux, no native tool: OSC 52 only; wrote %q ran %q", wrote, ran)
	}
	// esc clears the highlight and does nothing else.
	m = key(m, kEsc)
	if m.drag.on || strings.Contains(m.View().Content, "\x1b[0;7m") {
		t.Error("esc should clear the highlight")
	}
	if m.flashText != "copied 15 chars → terminal" {
		t.Errorf("esc did more than clear: flash %q", m.flashText)
	}
	// Any other key clears it too, and still does its job.
	m = mouse(m, mousePress, tea.MouseLeft, 0, y0)
	m = mouse(m, mouseMotion, tea.MouseLeft, 4, y0)
	m = mouse(m, mouseRelease, tea.MouseLeft, 4, y0)
	m = key(m, runesKey([]rune("z")))
	if m.drag.on || m.input.Value() != "z" {
		t.Errorf("a key should clear the highlight and type: on %v input %q", m.drag.on, m.input.Value())
	}
}

// Motion with no press, and the middle/right buttons, do nothing.
func TestMouse_IgnoresMotionAndOtherButtons(t *testing.T) {
	m, _, _ := mouseConvo(t)
	y := rowY(m, m.sel+1, 0)
	for _, ev := range []tea.MouseMsg{
		mouseMsg(mouseMotion, tea.MouseNone, 3, y),
		mouseMsg(mouseMotion, tea.MouseLeft, 3, y),
		mouseMsg(mouseRelease, tea.MouseLeft, 3, y),
		mouseMsg(mousePress, tea.MouseRight, 3, y),
		mouseMsg(mousePress, tea.MouseMiddle, 3, y),
	} {
		nm, _ := m.Update(ev)
		m = nm.(tuiModel)
		if m.focus != focusInput || m.drag.on {
			t.Fatalf("%+v changed state: focus %d drag %+v", ev, m.focus, m.drag)
		}
	}
}

func TestSelText(t *testing.T) {
	lines := []string{
		"\x1b[31m▎hello\x1b[0m world   ",
		" second line  ",
		" third",
	}
	cases := []struct {
		from, to cellPos
		skip     int
		want     string
	}{
		{cellPos{0, 1}, cellPos{0, 5}, 1, "hello"},
		{cellPos{0, 0}, cellPos{0, 40}, 0, "▎hello world"},
		{cellPos{0, 0}, cellPos{0, 40}, 1, "hello world"},
		{cellPos{0, 7}, cellPos{2, 3}, 1, "world\nsecond line\nthi"},
		{cellPos{1, 12}, cellPos{1, 13}, 1, ""},
	}
	for _, c := range cases {
		if got := selText(lines, c.from, c.to, c.skip); got != c.want {
			t.Errorf("%v–%v skip %d: %q, want %q", c.from, c.to, c.skip, got, c.want)
		}
	}
}

func TestHighlightKeepsWidths(t *testing.T) {
	rows := []string{"\x1b[32mgreen text\x1b[0m here", "plain row", "untouched"}
	out := highlight(rows, 10, cellPos{10, 6}, cellPos{11, 2})
	for i := range rows {
		if ansi.StringWidth(out[i]) != ansi.StringWidth(rows[i]) {
			t.Errorf("row %d width %d → %d", i, ansi.StringWidth(rows[i]), ansi.StringWidth(out[i]))
		}
		if ansi.Strip(out[i]) != ansi.Strip(rows[i]) {
			t.Errorf("row %d text changed: %q", i, ansi.Strip(out[i]))
		}
	}
	if !strings.Contains(out[0], "\x1b[0;7mtext here\x1b[0m") || !strings.Contains(out[1], "\x1b[0;7mpla\x1b[0m") || out[2] != rows[2] {
		t.Errorf("wrong cells highlighted: %q", out)
	}
}

func TestOSC52(t *testing.T) {
	if got := osc52("hi"); got != "\x1b]52;c;aGk=\x07" {
		t.Errorf("osc52 = %q", got)
	}
}

// fakeClip replaces every clipboard route: env is the environment, tools
// the commands on PATH, ran records each command line, and tmuxSC is what
// `tmux show-options -gv set-clipboard` answers.
func fakeClip(t *testing.T, env map[string]string, wrote, ran *[]string, tools map[string]bool, tmuxSC ...string) {
	t.Helper()
	w, e, l, r := termWrite, clipEnv, clipLook, clipRun
	t.Cleanup(func() { termWrite, clipEnv, clipLook, clipRun = w, e, l, r })
	termWrite = func(s string) { *wrote = append(*wrote, s) }
	clipEnv = func(k string) string { return env[k] }
	clipLook = func(name string) bool { return tools[name] }
	clipRun = func(stdin, name string, args ...string) (string, error) {
		line := strings.Join(append([]string{name}, args...), " ")
		if line == "tmux show-options -gv set-clipboard" {
			if len(tmuxSC) > 0 {
				return tmuxSC[0], nil
			}
			return "external", nil
		}
		*ran = append(*ran, line+" <- "+stdin)
		return "", nil
	}
}

// The routes per setting; the real tmux and clipboard tools never run.
func TestCopyText_Routes(t *testing.T) {
	inTmux := map[string]string{"TMUX": "/tmp/tmux-1/default,1,0"}
	cases := []struct {
		name   string
		env    map[string]string
		tools  map[string]bool
		sc     string
		wrote  int
		ran    []string
		via    string
		hinted bool
	}{
		{"plain terminal", map[string]string{}, nil, "", 1, nil, "terminal", false},
		{"local mac", map[string]string{}, map[string]bool{"pbcopy": true}, "", 1, []string{"pbcopy <- x"}, "pbcopy terminal", false},
		{"mac over ssh skips pbcopy", map[string]string{"SSH_TTY": "/dev/ttys1"}, map[string]bool{"pbcopy": true}, "", 1, nil, "terminal", false},
		{"Terminal.app, nothing else", map[string]string{"TERM_PROGRAM": "Apple_Terminal", "SSH_TTY": "x"}, nil, "", 1, nil, "terminal", true},
		{"tmux external", inTmux, nil, "external", 0, []string{"tmux load-buffer -w - <- x"}, "tmux", false},
		{"tmux on: OSC 52 only, tmux buffers it", inTmux, nil, "on", 1, nil, "tmux", false},
		{"tmux off", inTmux, nil, "off", 0, []string{"tmux load-buffer - <- x"}, "tmux buffer", true},
		{"tmux off, local wl-copy", map[string]string{"TMUX": "t", "WAYLAND_DISPLAY": "w"}, map[string]bool{"wl-copy": true}, "off", 0, []string{"wl-copy <- x", "tmux load-buffer - <- x"}, "wl-copy tmux buffer", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var wrote, ran []string
			fakeClip(t, c.env, &wrote, &ran, c.tools, c.sc)
			via, hint := copyText("x")
			if len(wrote) != c.wrote || strings.Join(ran, "|") != strings.Join(c.ran, "|") ||
				strings.Join(via, " ") != c.via || (hint != "") != c.hinted {
				t.Errorf("wrote %d ran %q via %q hint %q", len(wrote), ran, via, hint)
			}
		})
	}
}

func TestCopiedNote(t *testing.T) {
	if s := copiedNote(copiedMsg{n: 1234, via: []string{"pbcopy", "tmux"}}); s != "copied 1,234 chars → pbcopy + tmux" {
		t.Errorf("%q", s)
	}
	if s := copiedNote(copiedMsg{n: 3}); !strings.Contains(s, "could not copy") {
		t.Errorf("%q", s)
	}
}

// y copies the selected block's source text.
func TestYank(t *testing.T) {
	var wrote, ran []string
	fakeClip(t, map[string]string{}, &wrote, &ran, nil)
	v := newVtui(80, 24)
	v.event(map[string]any{"type": "ready", "tools": []any{"exec"}})
	v.event(map[string]any{"type": "user", "content": "hello there"})
	v.event(map[string]any{"type": "turn", "kind": "user"})
	v.event(map[string]any{"type": "token", "text": "A **bold** reply"})
	v.event(map[string]any{"type": "done"})
	v.m = key(v.m, tea.KeyPressMsg{Code: tea.KeyTab}) // the newest block
	if v.m.focus != focusConvo {
		t.Fatal("precondition: the conversation has focus")
	}
	nm, cmd := v.m.Update(runesKey([]rune{'y'}))
	if cmd == nil {
		t.Fatal("y on a selected block should copy")
	}
	nm, _ = nm.(tuiModel).Update(cmd())
	if len(wrote) != 1 || wrote[0] != osc52("A **bold** reply") {
		t.Errorf("wrote %q, want the reply's markdown", wrote)
	}
	if f := nm.(tuiModel).flashText; !strings.HasPrefix(f, "copied 16 chars") {
		t.Errorf("flash %q", f)
	}
}

func TestYankText_Run(t *testing.T) {
	e := convoEntry{run: &toolRun{items: []*runItem{
		{tool: "exec", args: map[string]any{"command": "ls"}, result: "a\nb\n"},
		{think: &thinkBlock{text: " hmm "}},
	}}}
	got := stripANSI(yankText(e))
	for _, want := range []string{"⚙ exec", "command: ls", "a\nb", "hmm"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
}

// y in the input is a letter.
func TestYank_InputTypesY(t *testing.T) {
	v := newVtui(80, 24)
	v.event(map[string]any{"type": "ready", "tools": []any{"exec"}})
	v.m = typeStr(v.m, "yay")
	if got := v.m.input.Value(); got != "yay" {
		t.Errorf("input %q", got)
	}
}

// A single click in the conversation selects nothing and keeps the focus.
func TestMouse_SingleClickInConvoDoesNothing(t *testing.T) {
	m, _, _ := mouseConvo(t)
	before := m.focus
	m = click(m, 5, rowY(m, len(m.convo)-1, 0))
	if m.focus != before {
		t.Errorf("a single click moved focus %d → %d", before, m.focus)
	}
	m.lastClickAt = m.lastClickAt.Add(-time.Second) // too slow for a double
	m = click(m, 5, rowY(m, len(m.convo)-1, 0))
	if m.focus != before {
		t.Error("two slow clicks are not a double click")
	}
}

// esc backs out of a selection: focus returns to the input, nothing selected.
func TestEscBacksOutOfConvoSelection(t *testing.T) {
	m, _, _ := mouseConvo(t)
	m = dclick(m, 5, rowY(m, len(m.convo)-1, 0))
	if m.focus != focusConvo || m.sel < 0 {
		t.Fatal("precondition: a block is selected")
	}
	m = key(m, kEsc)
	if m.focus != focusInput || m.sel != -1 {
		t.Errorf("esc: focus %d sel %d, want the input and no selection", m.focus, m.sel)
	}
	m = typeStr(m, "hi")
	if m.input.Value() != "hi" {
		t.Errorf("keys should go to the input, got %q", m.input.Value())
	}
}
