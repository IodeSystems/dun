package main

import (
	"fmt"
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
	// Something that is not a shift at all.
	now = now.Add(minRegionGap)
	if got := typeName(s.plan(rows("x", "y", "z"), 2, 4, 4, now)()); got != "tea.syncScrollAreaMsg" {
		t.Fatalf("a re-wrap should repaint the region, got %s", got)
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
	if want := "\n\n\nfour"; got != want {
		t.Fatalf("blankRegion = %q, want %q", got, want)
	}
	if blankRegion(in, 99) != in {
		t.Error("a count past the end must leave the pane alone")
	}
}
