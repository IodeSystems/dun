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
	cap := boxCap(v.m.frame.height)
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

// Selected, the box takes ↑/↓ and pgup/pgdn inside itself; at its edges the
// keys move the selection on, as for any block.
func TestBox_InnerScrollAndEdges(t *testing.T) {
	v := newVtui(80, 40)
	v.event(map[string]any{"type": "ready", "tools": []any{"eval"}})
	v.send("plan it")
	thinkLines(v, 0, 20) // 20 rows, 9 shown: 11 hidden above
	v.event(map[string]any{"type": "token", "text": "the reply"})
	v.event(map[string]any{"type": "done"})
	i := thinkIdx(v.m)
	v.m = key(v.m, kTab)
	for v.m.sel > i {
		v.m = key(v.m, kUp)
	}
	if v.m.sel != i {
		t.Fatalf("could not select the box: sel=%d box=%d", v.m.sel, i)
	}
	b := v.m.convo[i].box
	if b.off != 11 {
		t.Fatalf("precondition: box at its end, off=%d", b.off)
	}
	v.m = key(v.m, kUp)
	if v.m.sel != i || b.off != 10 || b.follow {
		t.Fatalf("↑ should scroll inside the box: sel=%d off=%d follow=%v", v.m.sel, b.off, b.follow)
	}
	if !strings.Contains(v.view(), "↓ 1 more") {
		t.Errorf("hidden-below count missing:\n%s", v.view())
	}
	v.m = key(v.m, tea.KeyMsg{Type: tea.KeyPgUp})
	if b.off != 2 {
		t.Errorf("pgup should page inside the box: off=%d", b.off)
	}
	v.m = key(v.m, kUp)
	v.m = key(v.m, kUp)
	if b.off != 0 || v.m.sel != i {
		t.Fatalf("↑ should scroll to the box's top first: off=%d sel=%d", b.off, v.m.sel)
	}
	v.m = key(v.m, kUp)
	if v.m.sel >= i {
		t.Errorf("↑ at the top should leave the box: off=%d sel=%d", b.off, v.m.sel)
	}
	// Back down: into the box at its top, ↓ walks it to the end, then leaves.
	v.m = key(v.m, kDown)
	if v.m.sel != i {
		t.Fatalf("↓ did not come back to the box: sel=%d", v.m.sel)
	}
	for k := 0; k < 11; k++ {
		v.m = key(v.m, kDown)
	}
	if v.m.sel != i || b.off != 11 || !b.follow {
		t.Fatalf("↓ should scroll the box to its end first: sel=%d off=%d follow=%v", v.m.sel, b.off, b.follow)
	}
	v.m = key(v.m, kDown)
	if v.m.sel == i {
		t.Error("↓ at the bottom should move the selection on")
	}
}

// A tool call's output is a box too: header is the call and its preview.
func TestBox_ToolOutput(t *testing.T) {
	v := newVtui(80, 40)
	v.event(map[string]any{"type": "ready", "tools": []any{"exec"}})
	v.send("go")
	var out strings.Builder
	for k := 0; k < 30; k++ {
		fmt.Fprintf(&out, "out %d\n", k)
	}
	v.event(map[string]any{"type": "tool_call", "tool": "exec", "args": map[string]any{"command": "seq 30"}})
	v.event(map[string]any{"type": "tool_result", "tool": "exec", "result": out.String()})
	last := len(v.m.convo) - 1
	if h := boxH(v.m, last); h != boxCap(v.m.frame.height) {
		t.Errorf("tool box is %d rows, want the cap", h)
	}
	scr := v.view()
	if !strings.Contains(scr, "$ seq 30") || !strings.Contains(scr, "out 29") || strings.Contains(scr, "out 10") {
		t.Errorf("tool box: header or tail wrong:\n%s", scr)
	}
}

// The cap on a short pane is a third of it, never under three.
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
	if i < 0 || !v.m.convo[i].boxed() || boxH(v.m, i) != boxCap(v.m.frame.height) {
		t.Fatalf("no capped box from history: %d", i)
	}
	scr := v.view()
	if !strings.Contains(scr, "✻ thought · ") || !strings.Contains(scr, "past line 29") || strings.Contains(scr, "past line 5\n") {
		t.Errorf("replayed box: header or tail wrong:\n%s", scr)
	}
}
