package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func rows(ss ...string) []string { return ss }

// typeName identifies a plan's decision: Bubble Tea's scroll messages are
// unexported, so their type name is the only handle a test has on them.
func typeName(v any) string { return fmt.Sprintf("%T", v) }

func TestScrollRegionScrollsOnAppend(t *testing.T) {
	s := &scrollRegion{}
	now := time.Now()
	// First plan claims the rows: a full paint.
	if got := typeName(s.plan(rows("a", "b", "c"), 2, 4, 4, now)()); got != "tea.syncScrollAreaMsg" {
		t.Fatalf("first plan should sync, got %s", got)
	}
	// Unchanged content: nothing at all, and not rate-limited either.
	if c := s.plan(rows("a", "b", "c"), 2, 4, 4, now); c != nil {
		t.Fatalf("unchanged content should cost nothing, got %s", typeName(c()))
	}
	// One line scrolled off the top — the streaming case.
	now = now.Add(minRegionGap)
	if got := typeName(s.plan(rows("b", "c", "d"), 2, 4, 4, now)()); got != "tea.scrollDownMsg" {
		t.Fatalf("an append at the bottom should scroll, got %s", got)
	}
	// Scrolled back up.
	now = now.Add(minRegionGap)
	if got := typeName(s.plan(rows("a", "b", "c"), 2, 4, 4, now)()); got != "tea.scrollUpMsg" {
		t.Fatalf("a line arriving at the top should scroll, got %s", got)
	}
	// Something that is not a shift at all: the rows go back to the renderer,
	// whose line diff draws an in-place change for less.
	now = now.Add(minRegionGap)
	if got := typeName(s.plan(rows("x", "y", "z"), 2, 4, 4, now)()); got != "tea.clearScrollAreaMsg" {
		t.Fatalf("an in-place change should hand the rows back, got %s", got)
	}
	if s.rowsFor(4) != 0 {
		t.Error("handed back, the region must not blank its rows")
	}
	// More in-place change (a box streaming at its cap): still the renderer's.
	now = now.Add(minRegionGap)
	if c := s.plan(rows("x", "y", "w"), 2, 4, 4, now); c != nil {
		t.Fatalf("in-place change while handed back should cost nothing, got %s", typeName(c()))
	}
	// The pane scrolls again: the region takes the rows back.
	now = now.Add(minRegionGap)
	if got := typeName(s.plan(rows("y", "w", "v"), 2, 4, 4, now)()); got != "tea.syncScrollAreaMsg" {
		t.Fatalf("a scroll should reclaim the region, got %s", got)
	}
}

// The ordering guard: Bubble Tea runs each Cmd in its own goroutine, so two
// incremental commands issued together can land out of order and scramble the
// pane. A second change inside the gap must defer, not race.
func TestScrollRegionSerialisesCommands(t *testing.T) {
	s := &scrollRegion{}
	now := time.Now()
	s.plan(rows("a", "b", "c"), 2, 4, 4, now)

	if got := typeName(s.plan(rows("b", "c", "d"), 2, 4, 4, now.Add(minRegionGap/5))()); got != "main.regionTickMsg" {
		t.Fatalf("a change inside the gap must schedule a retry, got %s", got)
	}
	// The retry does not change what the region believes is on screen, so the
	// next plan still describes the same move.
	if got := typeName(s.plan(rows("b", "c", "d"), 2, 4, 4, now.Add(minRegionGap))()); got != "tea.scrollDownMsg" {
		t.Fatalf("after the gap the move should go through, got %s", got)
	}
}

// A pane that moved or changed shape is not a scroll: the rows the region holds
// are no longer the rows it claimed.
func TestScrollRegionResyncsOnLayoutChange(t *testing.T) {
	s := &scrollRegion{}
	now := time.Now()
	s.plan(rows("a", "b", "c"), 2, 4, 4, now)
	now = now.Add(minRegionGap)
	if got := typeName(s.plan(rows("a", "b", "c"), 3, 5, 4, now)()); got != "tea.syncScrollAreaMsg" {
		t.Fatalf("moved boundaries should repaint, got %s", got)
	}
	if s.rowsFor(4) != 3 {
		t.Errorf("region should own 3 rows of a 4-row pane, got %d", s.rowsFor(4))
	}
	if s.rowsFor(9) != 0 {
		t.Errorf("a pane of a different height must not be blanked, got %d", s.rowsFor(9))
	}
}

func TestBlankRegionKeepsLineCount(t *testing.T) {
	in := "one\ntwo\nthree\nfour"
	got := blankRegion(in, 3)
	if want := "one\n\n\nfour"; got != want {
		t.Fatalf("blankRegion = %q, want %q", got, want)
	}
	if blankRegion(in, 99) != in {
		t.Error("a count past the end must leave the pane alone")
	}
}

// The rows View leaves blank for the region must be rows the renderer
// ignores, and Bubble Tea ignores 0-based frame lines [top, bot) while the
// region sits on 1-based screen rows top..bot. The region's first screen row
// is therefore the renderer's: View must give it real content, or every full
// repaint blanks the top line of the conversation (it did — seen in tmux).
func TestScrollRegionTopRowIsNotBlanked(t *testing.T) {
	v := tallConvo(t)
	v.event(map[string]any{"type": "token", "text": "more"})
	if !v.m.sr.live {
		t.Fatal("precondition: the region should be live on a full, pinned pane")
	}
	top, bot := v.m.sr.top, v.m.sr.bot
	frame := strings.Split(v.m.View(), "\n")
	if first := stripANSI(frame[top-1]); strings.TrimSpace(first) == "" {
		t.Errorf("the region's first screen row (frame line %d) is blank; the renderer paints it", top-1)
	}
	if want := stripANSI(v.m.vp.visible()[0]); !strings.Contains(want, strings.TrimSpace(stripANSI(frame[top-1]))) {
		t.Errorf("frame line %d = %q, want the pane's first row %q", top-1, stripANSI(frame[top-1]), want)
	}
	for i := top; i < bot; i++ { // what setIgnoredLines(top, bot) ignores
		if frame[i] != "" {
			t.Errorf("frame line %d is ignored by the renderer but not blank", i)
		}
	}
}
