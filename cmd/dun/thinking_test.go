package main

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// think streams n lines of reasoning, one event each, and draws the frame the
// render tick would draw.
func think(v *vtui, from, n int) {
	for i := from; i < from+n; i++ {
		v.event(map[string]any{"type": "reasoning", "text": fmt.Sprintf("step %d of the plan\n", i)})
	}
	nm, _ := v.m.Update(renderTickMsg{})
	v.m = nm.(tuiModel)
}

// thinkIdx is the index of the last thinking block, or -1.
func thinkIdx(m tuiModel) int {
	for i := len(m.convo) - 1; i >= 0; i-- {
		if m.convo[i].think != nil {
			return i
		}
	}
	return -1
}

// While it streams, thinking shows in full; the reply's first token folds it
// to one line that says what it was.
func TestThinking_StreamsThenFolds(t *testing.T) {
	v := newVtui(80, 24)
	v.event(map[string]any{"type": "ready", "tools": []any{"eval"}})
	v.send("plan it")
	think(v, 0, 3)
	i := thinkIdx(v.m)
	if i < 0 || !v.m.convo[i].think.live || v.m.convo[i].state != viewExpanded {
		t.Fatalf("no live, expanded thinking block: %d", i)
	}
	if !strings.Contains(v.view(), "step 2 of the plan") || !strings.Contains(v.view(), "thinking…") {
		t.Fatalf("live thinking not on screen:\n%s", v.view())
	}

	v.event(map[string]any{"type": "token", "text": "Here is the plan."})
	e := v.m.convo[i]
	if e.think.live || e.state != viewMinimized {
		t.Fatalf("thinking did not fold when the reply began: live=%v state=%v", e.think.live, e.state)
	}
	if got := stripANSI(e.view()); strings.Contains(got, "\n") || !strings.Contains(got, "thought") || !strings.Contains(got, "15 words") {
		t.Errorf("folded thinking is not a one-line summary: %q", got)
	}
	if strings.Contains(v.view(), "step 2 of the plan") {
		t.Errorf("folded thinking still shows its body:\n%s", v.view())
	}
	// The reply came after the thinking, not into it.
	if v.m.cur != "Here is the plan." {
		t.Errorf("reply: %q", v.m.cur)
	}
}

// A tool call ends the round's thinking too, and the next round's thinking is
// a block of its own.
func TestThinking_ToolCallEndsTheRound(t *testing.T) {
	v := newVtui(80, 24)
	v.event(map[string]any{"type": "ready", "tools": []any{"eval"}})
	v.send("go")
	think(v, 0, 1)
	first := thinkIdx(v.m)
	v.event(map[string]any{"type": "tool_call", "tool": "eval", "args": map[string]any{"code": "1"}})
	v.event(map[string]any{"type": "tool_result", "tool": "eval", "result": "1"})
	if v.m.convo[first].think.live || v.m.convo[first].state != viewMinimized {
		t.Fatal("tool call did not end the thinking")
	}
	think(v, 1, 1)
	if second := thinkIdx(v.m); second == first || !v.m.convo[second].think.live {
		t.Fatalf("the next round did not open its own block: %d vs %d", second, first)
	}
	v.event(map[string]any{"type": "done"})
	if v.m.liveThinking() >= 0 {
		t.Error("done left thinking live")
	}
}

// A reader who scrolled up is not moved by thinking streaming in below them,
// nor by it folding.
func TestThinking_DoesNotScrollAReaderWhoScrolledUp(t *testing.T) {
	v := tallConvo(t)
	v.send("again")
	v.m = key(v.m, tea.KeyMsg{Type: tea.KeyPgUp})
	if v.m.scrollPinned {
		t.Fatal("precondition: pgup should unpin")
	}
	y := v.m.vp.YOffset
	think(v, 0, 20)
	if v.m.vp.YOffset != y {
		t.Fatalf("streaming thinking moved the viewport: %d → %d", y, v.m.vp.YOffset)
	}
	v.event(map[string]any{"type": "token", "text": "ok"})
	if v.m.vp.YOffset != y {
		t.Fatalf("folding the thinking moved the viewport: %d → %d", y, v.m.vp.YOffset)
	}

	// A pinned reader does follow it.
	v2 := tallConvo(t)
	v2.send("again")
	think(v2, 0, 20)
	if !v2.m.vp.AtBottom() {
		t.Error("a pinned reader should follow the thinking")
	}
}

// The block the person has selected stays as it is when the round ends.
func TestThinking_SelectedStaysOpen(t *testing.T) {
	v := newVtui(80, 24)
	v.event(map[string]any{"type": "ready", "tools": []any{"eval"}})
	v.send("plan it")
	think(v, 0, 3)
	v.m = key(v.m, kTab) // convo focus starts on the newest block: the thinking
	i := thinkIdx(v.m)
	if v.m.focus != focusConvo || v.m.sel != i {
		t.Fatalf("precondition: focus=%d sel=%d think=%d", v.m.focus, v.m.sel, i)
	}
	v.event(map[string]any{"type": "token", "text": "reply"})
	v.event(map[string]any{"type": "done"})
	e := v.m.convo[i]
	if e.think.live {
		t.Error("still live after done")
	}
	if e.state != viewExpanded || !strings.Contains(stripANSI(e.view()), "step 2 of the plan") {
		t.Errorf("selected thinking folded under the reader: state=%v", e.state)
	}
}

// Enter opens the thinking in the pager: the convo pane's whole height,
// scrollable, and esc comes back to the selection.
func TestThinking_EnterOpensThePager(t *testing.T) {
	v := newVtui(80, 20)
	v.event(map[string]any{"type": "ready", "tools": []any{"eval"}})
	v.send("plan it")
	think(v, 0, 60)
	v.event(map[string]any{"type": "token", "text": "reply"})
	v.event(map[string]any{"type": "done"})
	i := thinkIdx(v.m)
	v.m = key(v.m, kTab)
	for v.m.sel > i {
		v.m = key(v.m, kUp)
	}
	if v.m.sel != i {
		t.Fatalf("could not select the thinking: sel=%d think=%d", v.m.sel, i)
	}
	v.m = key(v.m, kEnter)
	if !v.m.paging {
		t.Fatal("enter did not open the pager")
	}
	scr := v.view()
	if !strings.Contains(scr, "step 0 of the plan") || !strings.Contains(scr, "thought") || !strings.Contains(scr, "esc back") {
		t.Fatalf("pager not on screen:\n%s", scr)
	}
	if strings.Contains(scr, "step 59 of the plan") {
		t.Fatalf("a finished block should open at its top:\n%s", scr)
	}
	if got := len(strings.Split(scr, "\n")); got != 20 {
		t.Errorf("frame is %d rows, terminal is 20", got)
	}
	v.m = key(v.m, kDown)
	if v.m.pg.off != 1 {
		t.Errorf("↓ should scroll one row: off=%d", v.m.pg.off)
	}
	v.m = key(v.m, tea.KeyMsg{Type: tea.KeyEnd})
	if scr := v.view(); !strings.Contains(scr, "step 59 of the plan") {
		t.Errorf("end did not reach the bottom:\n%s", scr)
	}
	v.m = key(v.m, kEsc)
	if v.m.paging || v.m.quitting {
		t.Fatalf("esc should close the pager, not quit: paging=%v quitting=%v", v.m.paging, v.m.quitting)
	}
	if v.m.sel != i || v.m.focus != focusConvo {
		t.Errorf("selection lost: sel=%d focus=%d", v.m.sel, v.m.focus)
	}
	if !strings.Contains(v.view(), "reply") {
		t.Errorf("conversation not back:\n%s", v.view())
	}
}

// Opened while it is still streaming, the pager follows it as it grows.
func TestThinking_PagerFollowsLiveThinking(t *testing.T) {
	v := newVtui(80, 20)
	v.event(map[string]any{"type": "ready", "tools": []any{"eval"}})
	v.send("plan it")
	think(v, 0, 40)
	v.m = key(v.m, kTab)
	v.m = key(v.m, kEnter)
	if !v.m.paging {
		t.Fatal("enter did not open the pager")
	}
	think(v, 40, 5)
	if scr := v.view(); !strings.Contains(scr, "step 44 of the plan") {
		t.Errorf("live pager did not follow:\n%s", scr)
	}
}

// Resumed history shows past thinking folded, in place.
func TestThinking_ReplayFolded(t *testing.T) {
	v := newVtui(80, 24)
	v.event(map[string]any{"type": "history", "items": []any{
		map[string]any{"kind": "user", "content": "hi"},
		map[string]any{"kind": "reasoning", "content": "they said hi\nso greet"},
		map[string]any{"kind": "assistant", "content": "hello"},
	}})
	i := thinkIdx(v.m)
	if i < 0 || v.m.convo[i].think.live || v.m.convo[i].state != viewMinimized {
		t.Fatalf("no folded thinking from history: %d", i)
	}
	if got := stripANSI(v.m.convo[i].view()); !strings.Contains(got, "thought · 5 words — they said hi") {
		t.Errorf("summary: %q", got)
	}
	if strings.Contains(v.m.convo[i+1].mdSource, "hello") == false {
		t.Errorf("thinking is not just before the reply it belongs to")
	}
}
