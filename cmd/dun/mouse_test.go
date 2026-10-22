package main

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

func mouse(m tuiModel, a tea.MouseAction, b tea.MouseButton, x, y int) tuiModel {
	nm, _ := m.Update(tea.MouseMsg{Action: a, Button: b, X: x, Y: y})
	return nm.(tuiModel)
}

// click is a left press and release on one cell.
func click(m tuiModel, x, y int) tuiModel {
	m = mouse(m, tea.MouseActionPress, tea.MouseButtonLeft, x, y)
	return mouse(m, tea.MouseActionRelease, tea.MouseButtonLeft, x, y)
}

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
			m = click(m, 5, rowY(m, i, last))
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
	m = click(m, 5, rowY(m, tog, 0))
	if m.sel != tog || m.convo[tog].state != viewMinimized {
		t.Fatalf("first click selects only: sel %d state %v", m.sel, m.convo[tog].state)
	}
	m = click(m, 5, rowY(m, tog, 0))
	if m.convo[tog].state == viewMinimized {
		t.Fatal("second click should toggle the block open")
	}
	m = click(m, 5, rowY(m, tog, 0))
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
	m = click(m, 5, rowY(m, think, 0))
	b := m.convo[think].box
	if m.sel != think || b.inside {
		t.Fatalf("header click: sel %d (want %d) inside %v", m.sel, think, b.inside)
	}
	m = click(m, 5, rowY(m, think, 2))
	if !b.inside || m.insideBox() != b {
		t.Fatal("a click in the body should descend into the box")
	}
	off := b.off
	m = mouse(m, tea.MouseActionPress, tea.MouseButtonWheelUp, 5, rowY(m, think, 2))
	if b.off != off-wheelDelta {
		t.Errorf("inside, the wheel should scroll the box: %d → %d", off, b.off)
	}
	// From another block, a body click selects and descends in one go.
	m = click(m, 5, rowY(m, think-1, 0))
	if b.inside || m.sel == think {
		t.Fatalf("selecting another block should leave the box: sel %d inside %v", m.sel, b.inside)
	}
	m = click(m, 5, rowY(m, think, 2))
	if m.sel != think || !b.inside {
		t.Errorf("body click from elsewhere: sel %d inside %v", m.sel, b.inside)
	}
}

// A click on the input focuses it and puts the cursor under the pointer, on a
// wrapped second row too.
func TestMouse_ClickFocusesInput(t *testing.T) {
	m, _, _ := mouseConvo(t)
	m = click(m, 5, rowY(m, len(m.convo)-1, 0))
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
	var loaded []string
	defer func(w func(string), e func(string) string, l func(string) error) {
		termWrite, clipEnv, tmuxLoad = w, e, l
	}(termWrite, clipEnv, tmuxLoad)
	termWrite = func(s string) { wrote = append(wrote, s) }
	clipEnv = func(string) string { return "" }
	tmuxLoad = func(s string) error { loaded = append(loaded, s); return nil }

	m, _, _, first := mouseConvoAt(t)
	m.vp.SetYOffset(0)
	m.scrollPinned = false
	y0, y1 := rowY(m, first, 0), rowY(m, first+1, 0)
	m = mouse(m, tea.MouseActionPress, tea.MouseButtonLeft, 0, y0)
	m = mouse(m, tea.MouseActionMotion, tea.MouseButtonLeft, 4, y1)
	if !m.drag.shown() {
		t.Fatal("motion off the press cell should start a highlight")
	}
	if !strings.Contains(m.View(), "\x1b[0;7m") {
		t.Error("the highlight should be drawn")
	}
	nm, cmd := m.Update(tea.MouseMsg{Action: tea.MouseActionRelease, Button: tea.MouseButtonLeft, X: 4, Y: y1})
	m = nm.(tuiModel)
	if m.focus != focusInput {
		t.Errorf("a drag must not select a block: focus %d sel %d", m.focus, m.sel)
	}
	if !m.drag.held || cmd == nil {
		t.Fatal("release after a drag should hold the highlight and copy")
	}
	if m.flashText != "copied 15 chars" { // "message 0\nmessa"
		t.Errorf("flash %q", m.flashText)
	}
	copyText(selText(m.vp.lines, cellPos{m.vp.YOffset + y0 - m.convoTopRow(), 0}, cellPos{m.vp.YOffset + y1 - m.convoTopRow(), 4}, 0))
	if len(wrote) != 1 || wrote[0] != osc52("message 0\nmessa") || len(loaded) != 0 {
		t.Errorf("outside tmux: OSC 52 only; wrote %q loaded %q", wrote, loaded)
	}
	// esc clears the highlight and does nothing else.
	m = key(m, kEsc)
	if m.drag.on || strings.Contains(m.View(), "\x1b[0;7m") {
		t.Error("esc should clear the highlight")
	}
	if m.flashText != "copied 15 chars" {
		t.Errorf("esc did more than clear: flash %q", m.flashText)
	}
	// Any other key clears it too, and still does its job.
	m = mouse(m, tea.MouseActionPress, tea.MouseButtonLeft, 0, y0)
	m = mouse(m, tea.MouseActionMotion, tea.MouseButtonLeft, 4, y0)
	m = mouse(m, tea.MouseActionRelease, tea.MouseButtonLeft, 4, y0)
	m = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("z")})
	if m.drag.on || m.input.Value() != "z" {
		t.Errorf("a key should clear the highlight and type: on %v input %q", m.drag.on, m.input.Value())
	}
}

// Motion with no press, and the middle/right buttons, do nothing.
func TestMouse_IgnoresMotionAndOtherButtons(t *testing.T) {
	m, _, _ := mouseConvo(t)
	y := rowY(m, m.sel+1, 0)
	for _, ev := range []tea.MouseMsg{
		{Action: tea.MouseActionMotion, Button: tea.MouseButtonNone, X: 3, Y: y},
		{Action: tea.MouseActionMotion, Button: tea.MouseButtonLeft, X: 3, Y: y},
		{Action: tea.MouseActionRelease, Button: tea.MouseButtonLeft, X: 3, Y: y},
		{Action: tea.MouseActionPress, Button: tea.MouseButtonRight, X: 3, Y: y},
		{Action: tea.MouseActionPress, Button: tea.MouseButtonMiddle, X: 3, Y: y},
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

// Inside tmux the text also goes to tmux's buffer; the real tmux never runs.
func TestCopyText_Tmux(t *testing.T) {
	defer func(w func(string), e func(string) string, l func(string) error) {
		termWrite, clipEnv, tmuxLoad = w, e, l
	}(termWrite, clipEnv, tmuxLoad)
	var wrote, loaded []string
	termWrite = func(s string) { wrote = append(wrote, s) }
	clipEnv = func(k string) string {
		if k == "TMUX" {
			return "/tmp/tmux-1/default,1,0"
		}
		return ""
	}
	tmuxLoad = func(s string) error { loaded = append(loaded, s); return nil }
	copyText("copied")
	if len(wrote) != 1 || wrote[0] != osc52("copied") || len(loaded) != 1 || loaded[0] != "copied" {
		t.Errorf("in tmux: wrote %q loaded %q", wrote, loaded)
	}
}
