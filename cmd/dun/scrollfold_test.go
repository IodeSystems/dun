package main

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// scrollfold_test.go: a block folds only under scroll pressure (scrollfold.go).
// For every update: the rows of every finished block that were on screen are
// still on screen, unchanged, all moved by the same amount — none when the view
// is not pinned, up by the rows appended when it is. A block folds only once it
// is wholly above the view.

// still is one frame's geometry: the view's top row and height, every display
// row, and each entry's rows.
type still struct {
	y, vh  int
	lines  []string
	ro, bh []int
	n      int
	pinned bool
}

func snap(v *vtui) still {
	s := still{y: v.m.vp.YOffset, vh: v.m.vp.Height, n: len(v.m.convo), pinned: v.m.scrollPinned}
	for _, l := range v.m.vp.lines {
		s.lines = append(s.lines, strings.TrimRight(stripANSI(l), " "))
	}
	for i := range v.m.convo {
		s.ro = append(s.ro, v.m.convo[i].rowOffset)
		s.bh = append(s.bh, v.m.convo[i].lastH)
	}
	return s
}

// stay checks that what was on screen in a is where it was in b. The last
// entry of a is left out: it is the one being written to (a live box, a run
// taking a call) and changes inside itself by design.
func stay(t *testing.T, step string, a, b still) {
	t.Helper()
	d, have := 0, false
	for i := 0; i+1 < a.n && i < b.n; i++ {
		top, h := a.ro[i], a.bh[i]
		if top+h <= a.y || top >= a.y+a.vh {
			continue // not on screen before
		}
		dd := (b.ro[i] - b.y) - (top - a.y) // how far its screen row moved
		if !have {
			d, have = dd, true
		}
		if dd != d {
			t.Fatalf("%s: entry %d moved %d rows, another moved %d", step, i, dd, d)
		}
		if b.bh[i] != h {
			t.Fatalf("%s: entry %d, on screen, changed height %d → %d", step, i, h, b.bh[i])
		}
		for r := max(top, a.y); r < min(top+h, a.y+a.vh); r++ {
			if a.lines[r] != b.lines[r-top+b.ro[i]] {
				t.Fatalf("%s: entry %d row %d changed on screen:\n  %q\n→ %q", step, i, r-top, a.lines[r], b.lines[r-top+b.ro[i]])
			}
		}
	}
	switch {
	case !a.pinned && d != 0:
		t.Fatalf("%s: scrolled up, the view moved %d rows", step, d)
	case d > 0:
		t.Fatalf("%s: pinned, the view moved DOWN %d rows", step, d)
	}
}

// foldSession drives a turn of thinking and tool calls, several rounds, and
// checks every event and render tick with stay.
type foldSession struct {
	t *testing.T
	v *vtui
	k int
}

func (f *foldSession) ev(ev map[string]any) {
	f.t.Helper()
	a := snap(f.v)
	f.v.event(ev)
	nm, _ := f.v.m.Update(renderTickMsg{})
	f.v.m = nm.(tuiModel)
	f.k++
	stay(f.t, fmt.Sprintf("step %d (%v)", f.k, ev["type"]), a, snap(f.v))
}

func (f *foldSession) round(r int) {
	f.t.Helper()
	for k := 0; k < 12; k++ {
		f.ev(map[string]any{"type": "reasoning", "text": fmt.Sprintf("round %d, line %d of the plan, which is long enough to wrap. ", r, k)})
		if k%3 == 2 {
			f.ev(map[string]any{"type": "reasoning", "text": "\n"})
		}
	}
	f.ev(map[string]any{"type": "tool_call", "tool": "exec", "args": map[string]any{"command": fmt.Sprintf("echo %d", r)}})
	f.ev(map[string]any{"type": "tool_result", "tool": "exec", "result": fmt.Sprintf("%d\n", r)})
}

func newFoldSession(t *testing.T, w, h int) *foldSession {
	v := newVtui(w, h)
	f := &foldSession{t: t, v: v}
	f.ev(map[string]any{"type": "ready", "tools": []any{"exec"}})
	return f
}

// turn is a question, rounds of thinking and calls, a reply, done.
func (f *foldSession) turn(q string, rounds int) {
	f.t.Helper()
	f.v.send(q)
	for r := 0; r < rounds; r++ {
		f.round(r)
	}
	f.ev(map[string]any{"type": "token", "text": "The answer, in a paragraph or two.\n\nAnd more of it."})
	f.ev(map[string]any{"type": "done"})
}

// runFolded says every finished thought of run entry i is folded.
func runFolded(e convoEntry) bool {
	for _, it := range e.run.items {
		if it.think != nil && !it.think.live && !it.folded {
			return false
		}
	}
	return true
}

// Pinned to the bottom while it streams, wide and on a phone: nothing on
// screen moves except up by what was appended; the runs that scrolled off the
// top folded; the ones on screen did not.
func TestScrollFold_PinnedStreaming(t *testing.T) {
	for _, size := range [][2]int{{100, 30}, {49, 21}} {
		t.Run(fmt.Sprintf("%dx%d", size[0], size[1]), func(t *testing.T) {
			f := newFoldSession(t, size[0], size[1])
			for q := 0; q < 4; q++ {
				f.turn(fmt.Sprintf("question %d", q), 1+2*(q%2))
			}
			m := f.v.m
			if m.foldedRows <= 0 {
				t.Fatalf("nothing folded above the view: %d rows", m.foldedRows)
			}
			for i, e := range m.convo {
				if e.run == nil {
					continue
				}
				above := e.rowOffset+e.lastH <= m.vp.YOffset
				onScreen := e.rowOffset < m.vp.YOffset+m.vp.Height && e.rowOffset+e.lastH > m.vp.YOffset
				switch {
				case above && !runFolded(e):
					t.Errorf("run %d is above the view and still open", i)
				case onScreen && runFolded(e) && i > 0 && m.convo[i-1].rowOffset >= m.vp.YOffset:
					// Folded while its whole block, and the one before, are on
					// screen: it was folded ahead of the scroll.
					t.Errorf("run %d is on screen and folded:\n%s", i, f.v.view())
				}
			}
		})
	}
}

// Scrolled up reading history while new output arrives: the view does not
// move at all, and the blocks above it fold under it without a row moving.
func TestScrollFold_ScrolledUpWhileOutputArrives(t *testing.T) {
	for _, size := range [][2]int{{100, 30}, {49, 21}} {
		t.Run(fmt.Sprintf("%dx%d", size[0], size[1]), func(t *testing.T) {
			f := newFoldSession(t, size[0], size[1])
			f.turn("question 0", 3)
			f.turn("question 1", 3)
			// Read back: a run halfway up, its block on screen.
			r := -1
			for i, e := range f.v.m.convo {
				if e.run != nil {
					r = i
				}
			}
			f.v.setScrollPin(false)
			f.v.setYOffset(f.v.m.convo[r].rowOffset - 2)
			f.v.m.refresh()
			f.v.send("question 2") // typing does not move the reader's view
			f.v.setScrollPin(false)
			f.v.setYOffset(f.v.m.convo[r].rowOffset - 2)
			f.v.m.refresh()
			for k := 0; k < 3; k++ {
				f.round(k)
			}
			f.ev(map[string]any{"type": "token", "text": "The answer, in a paragraph or two.\n\nAnd more of it."})
			// Not "done": the end of a turn re-pins the view to the bottom
			// (tui.go, "Force scroll to bottom"), on purpose, for now.
		})
	}
}

// Scrolling down past a run: the run folds once it is above the view, and the
// fold moves no visible row (the view's top moves up by the rows it gave back).
func TestScrollFold_ScrollingDownFoldsWhatLeaves(t *testing.T) {
	f := newFoldSession(t, 100, 30)
	f.turn("question 0", 1)
	f.turn("question 1", 3)
	f.turn("question 2", 3)
	f.v.setScrollPin(false)
	f.v.setYOffset(0)
	r := -1
	for i, e := range f.v.m.convo {
		if e.run != nil && r < 0 {
			r = i
		}
	}
	// It folded when the stream pushed it off; read from the top as if it
	// had not (a run of a turn that never scrolled).
	for _, it := range f.v.m.convo[r].run.items {
		it.folded = false
	}
	f.v.m.convo[r].run.ver++
	f.v.m.refresh()
	if runFolded(f.v.m.convo[r]) {
		t.Fatal("precondition: the run, on screen, is open")
	}
	for y := 0; y < f.v.m.vp.maxYOffset(); y++ {
		f.v.setYOffset(y)
		a := snap(f.v)
		f.v.m.refresh()
		stay(t, fmt.Sprintf("scroll to %d", y), a, snap(f.v))
		e := f.v.m.convo[r]
		if a.ro[r]+a.bh[r] > a.y && e.run != nil && runFolded(e) {
			t.Fatalf("at %d the run folded while on screen", y)
		}
	}
	if !runFolded(f.v.m.convo[r]) {
		t.Errorf("scrolled past it, the run is still open")
	}
}

// A resize re-wraps (the one time the view may move), and folds nothing: the
// run on screen stays open at the new width.
func TestScrollFold_ResizeFoldsNothing(t *testing.T) {
	f := newFoldSession(t, 100, 30)
	f.v.send("question 0")
	f.round(0)
	f.round(1)
	r := runIdx(f.v.m)
	f.v.resize(49, 21)
	if runFolded(f.v.m.convo[r]) {
		t.Errorf("the resize folded the run on screen:\n%s", f.v.view())
	}
	f.round(2)
	f.ev(map[string]any{"type": "done"})
	f.v.resize(100, 30)
	f.turn("question 1", 2)
}

// A block the person opened stays open when it scrolls off and back, and the
// selected block does not fold while it is the selection.
func TestScrollFold_HandOpenedStaysOpen(t *testing.T) {
	f := newFoldSession(t, 49, 21)
	f.v.send("question 0")
	f.ev(map[string]any{"type": "retry", "scope": "request", "kind": "interrupted",
		"attempt": 1.0, "delay_ms": 10.0, "reason": "the request was cut", "text": "cut"})
	f.ev(map[string]any{"type": "retry", "kind": "recovered", "text": "recovered"})
	open := -1
	for i, e := range f.v.m.convo {
		if e.full != "" {
			open = i
		}
	}
	if open < 0 {
		t.Fatalf("no retry block:\n%s", f.v.convo())
	}
	f.v.m.convo[open].state = viewExpanded // opened by hand
	f.v.m.refresh()
	h := f.v.m.convo[open].lastH
	f.round(0)
	f.ev(map[string]any{"type": "done"})
	f.turn("question 1", 3)
	e := f.v.m.convo[open]
	if e.rowOffset+e.lastH > f.v.m.vp.YOffset {
		t.Fatal("precondition: the opened block is above the view")
	}
	if e.state != viewExpanded || e.lastH != h {
		t.Errorf("the opened block changed above the view: state %d, %d → %d rows", e.state, h, e.lastH)
	}

	// The selection: a run selected in convo focus does not fold above the view.
	f.v.m = key(f.v.m, tea.KeyPressMsg{Code: tea.KeyTab})
	r := runIdx(f.v.m)
	f.v.m.convo[r].run.items[0].folded = false
	f.v.m.sel = r
	f.v.setYOffset(f.v.m.vp.maxYOffset())
	f.v.m.refresh()
	if runFolded(f.v.m.convo[r]) && f.v.m.convo[r].run.items[0].think != nil {
		t.Errorf("the selected run folded")
	}
}

// On a phone the thinking box that a call takes over keeps its rows: the
// thought's text stays where it was, and no blank rows open where it was.
func TestScrollFold_NoBlankRowsWhenACallTakesTheThought(t *testing.T) {
	f := newFoldSession(t, 49, 21)
	f.v.send("question 0")
	for k := 0; k < 12; k++ {
		f.ev(map[string]any{"type": "reasoning", "text": fmt.Sprintf("line %d of the plan.\n", k)})
	}
	f.ev(map[string]any{"type": "tool_call", "tool": "exec", "args": map[string]any{"command": "echo 0"}})
	r := runIdx(f.v.m)
	e := f.v.m.convo[r]
	rows := f.v.m.vp.lines[e.rowOffset : e.rowOffset+e.lastH]
	for k, l := range rows {
		if strings.TrimSpace(stripANSI(l)) == "" {
			t.Errorf("row %d of the run is blank:\n%s", k, f.v.view())
		}
	}
}
