package main

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// thinkLines streams lines from..from+n-1 of a thought and draws the tick.
func thinkLines(v *vtui, from, n int) {
	for i := from; i < from+n; i++ {
		v.event(map[string]any{"type": "reasoning", "text": fmt.Sprintf("line %d of the thought\n", i)})
	}
	nm, _ := v.m.Update(renderTickMsg{})
	v.m = nm.(tuiModel)
}

func boxH(m tuiModel, i int) int { return m.frame.blockH[i] }

// The box grows with its content up to the cap, then holds its height and
// shows the newest lines (tail-follow), with a header saying what is hidden.
func TestBox_GrowsToTheCapThenFollowsItsTail(t *testing.T) {
	v := newVtui(80, 40) // pane ~36 rows: cap 10
	v.event(map[string]any{"type": "ready", "tools": []any{"eval"}})
	v.send("plan it")
	cap := boxCap(v.m.h)
	if cap != 10 {
		t.Fatalf("precondition: cap %d", cap)
	}
	thinkLines(v, 0, 3)
	i := thinkIdx(v.m)
	if h := boxH(v.m, i); h != 4 {
		t.Errorf("a 3-line thought should be header + 3 rows, got %d", h)
	}
	thinkLines(v, 3, 40)
	if h := boxH(v.m, i); h != cap {
		t.Fatalf("box grew past its cap: %d > %d", h, cap)
	}
	scr := v.view()
	if !strings.Contains(scr, "line 42 of the thought") || strings.Contains(scr, "line 30 of the thought") {
		t.Errorf("the box does not show its tail:\n%s", scr)
	}
	if !strings.Contains(scr, "✻ thinking…") || !strings.Contains(scr, "↑ 34 more") {
		t.Errorf("header or hidden-count missing:\n%s", scr)
	}
}

// When the round ends the box keeps its height, scrolled to the end — no
// fold, no shrink — and only the header changes.
func TestBox_DoneKeepsTheHeight(t *testing.T) {
	v := newVtui(80, 40)
	v.event(map[string]any{"type": "ready", "tools": []any{"eval"}})
	v.send("plan it")
	thinkLines(v, 0, 40)
	i := thinkIdx(v.m)
	before := boxH(v.m, i)
	v.event(map[string]any{"type": "token", "text": "Here is the plan."})
	v.event(map[string]any{"type": "done"})
	if h := boxH(v.m, i); h != before {
		t.Errorf("the box changed height when the round ended: %d → %d", before, h)
	}
	scr := v.view()
	if !strings.Contains(scr, "✻ thought for") || !strings.Contains(scr, "line 39 of the thought") {
		t.Errorf("done box: wrong header or not at its end:\n%s", scr)
	}
}

// boxConvo is a conversation with a thinking box between two messages, and a
// run box after them, all longer than their boxes (test helper).
func boxConvo(t *testing.T, h int) (v *vtui, think, run int) {
	t.Helper()
	v = newVtui(80, h)
	v.event(map[string]any{"type": "ready", "tools": []any{"exec"}})
	v.send("plan it")
	thinkLines(v, 0, 20) // 20 rows, 9 shown: 11 hidden above
	v.event(map[string]any{"type": "token", "text": "the reply"})
	v.event(map[string]any{"type": "done"})
	think = thinkIdx(v.m)
	v.send("now run")
	for i := 0; i < 12; i++ {
		call(v, fmt.Sprintf("echo %d", i), "ok\n")
	}
	v.event(map[string]any{"type": "token", "text": "ran them"})
	v.event(map[string]any{"type": "done"})
	run = runIdx(v.m)
	return v, think, run
}

// In convo focus ↑/↓ step from block to block: past a thinking box and a run
// box without scrolling either.
func TestBox_SelectionMovesPastBoxes(t *testing.T) {
	v, think, run := boxConvo(t, 40)
	tb, rb := v.m.convo[think].box, v.m.convo[run].box
	toff, roff := tb.off, rb.off
	v.m = key(v.m, kTab)
	seen := map[int]bool{}
	for k := 0; k < 30 && v.m.sel > 0; k++ {
		seen[v.m.sel] = true
		before := v.m.sel
		v.m = key(v.m, kUp)
		if v.m.sel >= before {
			t.Fatalf("↑ did not move the selection off block %d", before)
		}
	}
	if !seen[think] || !seen[run] {
		t.Fatalf("the walk skipped a box: think=%v run=%v", seen[think], seen[run])
	}
	if tb.off != toff || rb.off != roff {
		t.Errorf("selecting a box scrolled it: think %d → %d, run %d → %d", toff, tb.off, roff, rb.off)
	}
	if strings.Contains(v.view(), "scrolling — ← to leave") {
		t.Error("nothing was descended into")
	}
}

// → descends into a box; inside, ↑/↓ pgup/pgdn g/G scroll it and the header
// says so; ←, h and esc leave with the position kept, and the selection stays.
func TestBox_DescendScrollAndLeave(t *testing.T) {
	v, think, _ := boxConvo(t, 40)
	v.m = key(v.m, kTab)
	for v.m.sel > think {
		v.m = key(v.m, kUp)
	}
	b := v.m.convo[think].box
	if v.m.sel != think || b.off != 11 {
		t.Fatalf("precondition: think selected at its end: sel=%d off=%d", v.m.sel, b.off)
	}
	v.m = key(v.m, kRight)
	if !b.inside || !strings.Contains(v.view(), "scrolling — ← to leave") || !strings.Contains(v.view(), "inside  ·  ↑/↓") {
		t.Fatalf("→ did not descend, or no cue:\n%s", v.view())
	}
	v.m = key(v.m, kUp)
	if b.off != 10 || v.m.sel != think {
		t.Fatalf("↑ inside should scroll the box: off=%d sel=%d", b.off, v.m.sel)
	}
	v.m = key(v.m, tea.KeyMsg{Type: tea.KeyPgUp})
	v.m = key(v.m, tea.KeyMsg{Type: tea.KeyPgUp})
	v.m = key(v.m, kUp)
	if b.off != 0 || v.m.sel != think {
		t.Fatalf("inside, the top of the box is not an exit: off=%d sel=%d", b.off, v.m.sel)
	}
	v.m = key(v.m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("G")})
	if b.off != 11 {
		t.Errorf("G should go to the end: off=%d", b.off)
	}
	v.m = key(v.m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("g")})
	v.m = key(v.m, kDown)
	v.m = key(v.m, tea.KeyMsg{Type: tea.KeyLeft})
	if b.inside || b.off != 1 || b.follow || v.m.sel != think || v.m.focus != focusConvo {
		t.Fatalf("← should leave, keeping the position: inside=%v off=%d follow=%v sel=%d", b.inside, b.off, b.follow, v.m.sel)
	}
	// l descends too; esc leaves (and does not quit).
	v.m = key(v.m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("l")})
	if !b.inside {
		t.Fatal("l should descend")
	}
	v.m = key(v.m, kEsc)
	if b.inside || v.m.quitting {
		t.Fatalf("esc should leave the box: inside=%v quitting=%v", b.inside, v.m.quitting)
	}
	// Enter still opens the pager, from outside or in.
	v.m = key(v.m, kRight)
	v.m = key(v.m, kEnter)
	if !v.m.paging {
		t.Error("enter should open the pager")
	}
}

// A live box left at its end follows its tail again; left above its end, it
// stays where the person put it.
func TestBox_TailFollowResumesOnLeave(t *testing.T) {
	v := newVtui(80, 40)
	v.event(map[string]any{"type": "ready", "tools": []any{"eval"}})
	v.send("plan it")
	thinkLines(v, 0, 20)
	i := thinkIdx(v.m)
	b := v.m.convo[i].box
	v.m = key(v.m, kTab) // the live thought is the newest block
	if v.m.sel != i {
		t.Fatalf("precondition: sel=%d think=%d", v.m.sel, i)
	}
	v.m = key(v.m, kRight)
	v.m = key(v.m, kUp)
	thinkLines(v, 20, 5)
	if b.off != 10 {
		t.Fatalf("scrolled up inside, new lines should not move it: off=%d", b.off)
	}
	v.m = key(v.m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("G")})
	v.m = key(v.m, tea.KeyMsg{Type: tea.KeyLeft})
	thinkLines(v, 25, 5)
	if !b.follow || b.off != b.max || !strings.Contains(v.view(), "line 29 of the thought") {
		t.Errorf("left at its end, the box should follow its tail: follow=%v off=%d max=%d", b.follow, b.off, b.max)
	}
}

// The wheel scrolls a box only from inside it; otherwise the conversation.
func TestBox_WheelOnlyInside(t *testing.T) {
	v, think, _ := boxConvo(t, 20) // taller than the pane, so it can scroll
	v.m = key(v.m, kTab)
	for v.m.sel > think {
		v.m = key(v.m, kUp)
	}
	b := v.m.convo[think].box
	wheel := tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelUp}
	off, y := b.off, v.m.vp.YOffset
	nm, _ := v.m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelDown})
	v.m = nm.(tuiModel)
	if b.off != off || v.m.vp.YOffset == y {
		t.Fatalf("outside, the wheel should scroll the conversation: box %d → %d, yoff %d → %d", off, b.off, y, v.m.vp.YOffset)
	}
	v.m = key(v.m, kRight)
	y = v.m.vp.YOffset
	nm, _ = v.m.Update(wheel)
	v.m = nm.(tuiModel)
	if b.off != off-wheelDelta || v.m.vp.YOffset != y {
		t.Errorf("inside, the wheel should scroll the box: box %d → %d, yoff %d → %d", off, b.off, y, v.m.vp.YOffset)
	}
}

// The run box's step list descends the same way.
func TestBox_RunDescends(t *testing.T) {
	v, _, run := boxConvo(t, 40)
	v.m = key(v.m, kTab)
	for v.m.sel > run {
		v.m = key(v.m, kUp)
	}
	b := v.m.convo[run].box
	end := b.off
	if end == 0 {
		t.Fatal("precondition: the run has steps hidden above")
	}
	v.m = key(v.m, kUp) // selection moves on, the run stays
	if v.m.sel == run || b.off != end {
		t.Fatalf("↑ on a run should move the selection: sel=%d off=%d", v.m.sel, b.off)
	}
	v.m = key(v.m, kDown)
	v.m = key(v.m, kRight)
	v.m = key(v.m, kUp)
	if b.off != end-1 || v.m.sel != run {
		t.Errorf("inside a run, ↑ walks its steps: off=%d (was %d) sel=%d", b.off, end, v.m.sel)
	}
}

// The cap on a short terminal is a third of it, never under three.
func TestBox_CapRule(t *testing.T) {
	for _, c := range []struct{ h, want int }{{40, 10}, {24, 8}, {12, 4}, {6, 3}, {2, 3}} {
		if got := boxCap(c.h); got != c.want {
			t.Errorf("boxCap(%d) = %d, want %d", c.h, got, c.want)
		}
	}
}

// Streaming a long thought moves a pinned conversation by at most the box's
// own growth: once the box is at its cap, nothing above it moves at all.
func TestBox_StreamingMovesTheScreenAtMostTheCap(t *testing.T) {
	v := tallConvo(t)
	v.send("again")
	topRow := func() string { return strings.Split(v.view(), "\n")[1] }
	thinkLines(v, 0, 12) // fill the box to its cap
	settled := topRow()
	y := v.m.vp.YOffset
	for k := 12; k < 200; k += 7 {
		thinkLines(v, k, 7)
		if v.m.vp.YOffset != y || topRow() != settled {
			t.Fatalf("the conversation moved while the capped box streamed (line %d): yoff %d → %d", k, y, v.m.vp.YOffset)
		}
	}
}

// Resumed history renders the same boxes, done.
func TestBox_Replay(t *testing.T) {
	v := newVtui(80, 40)
	thought := make([]string, 30)
	for k := range thought {
		thought[k] = fmt.Sprintf("past line %d", k)
	}
	v.event(map[string]any{"type": "history", "items": []any{
		map[string]any{"kind": "user", "content": "hi"},
		map[string]any{"kind": "reasoning", "content": strings.Join(thought, "\n")},
		map[string]any{"kind": "assistant", "content": "hello"},
	}})
	i := thinkIdx(v.m)
	if i < 0 || !v.m.convo[i].boxed() || boxH(v.m, i) != boxCap(v.m.h) {
		t.Fatalf("no capped box from history: %d", i)
	}
	scr := v.view()
	if !strings.Contains(scr, "✻ thought · ") || !strings.Contains(scr, "past line 29") || strings.Contains(scr, "past line 5\n") {
		t.Errorf("replayed box: header or tail wrong:\n%s", scr)
	}
}
