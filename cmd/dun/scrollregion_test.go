package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
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

// regionRace models what Bubble Tea does with the region's commands: each runs
// in its own goroutine, so two issued inside minRegionGap of each other may
// reach the renderer in either order. It delivers every such run in the WORSE
// order — the later one first — and tracks the one bit of renderer state that
// decides whether the pane can move: is it ignoring the region's rows.
type regionRace struct {
	ignoring bool // the renderer skips the region's rows (sync sets, clear unsets)
	pending  []tea.Cmd
	at       []time.Time
}

func (r *regionRace) issue(c tea.Cmd, now time.Time) {
	if c == nil {
		return
	}
	if n := len(r.at); n > 0 && now.Sub(r.at[n-1]) >= minRegionGap {
		r.flush() // the earlier ones have long landed
	}
	r.pending, r.at = append(r.pending, c), append(r.at, now)
}

func (r *regionRace) flush() {
	for i := len(r.pending) - 1; i >= 0; i-- {
		r.deliver(r.pending[i]())
	}
	r.pending, r.at = nil, nil
}

func (r *regionRace) deliver(msg tea.Msg) {
	if b, ok := msg.(tea.BatchMsg); ok {
		for _, c := range b {
			r.deliver(c())
		}
		return
	}
	switch typeName(msg) {
	case "tea.syncScrollAreaMsg":
		r.ignoring = true
	case "tea.clearScrollAreaMsg":
		r.ignoring = false
	}
}

// The frozen pane: the region syncs, and a moment later something that needs
// the rows back (a selection's band, a mouse highlight) disables it. If the
// clear lands before the sync, the renderer ends up ignoring the rows while
// the model has handed them back — View draws the pane, nothing paints it, and
// scrolling moves the offset (and everything outside the pane) but not the
// text, for as long as the band stays up.
func TestScrollRegionDisableWaitsForTheGap(t *testing.T) {
	s := &scrollRegion{}
	r := &regionRace{}
	now := time.Now()
	r.issue(s.plan(rows("a", "b", "c"), 2, 4, 4, now), now)
	now = now.Add(minRegionGap / 5)
	r.issue(s.disable(now), now)
	if s.rowsFor(4) != 0 {
		t.Error("handed back, the rows are View's to draw at once")
	}
	// The retry: the band is still up, so the rows are still wanted back.
	now = now.Add(minRegionGap)
	r.issue(s.disable(now), now)
	r.flush()
	if s.live || r.ignoring {
		t.Fatalf("the renderer ignores the region's rows (%v), but the region handed them back (live=%v): the pane is frozen", r.ignoring, s.live)
	}
}

// The other way round: rows handed back, then a sync straight after (the band
// went away). The sync landing first and the clear after it leaves the model
// blanking rows (live) that the renderer then paints — as blank.
func TestScrollRegionSyncWaitsForADisable(t *testing.T) {
	s := &scrollRegion{}
	r := &regionRace{}
	now := time.Now()
	r.issue(s.plan(rows("a", "b", "c"), 2, 4, 4, now), now)
	now = now.Add(minRegionGap)
	r.issue(s.disable(now), now)
	now = now.Add(minRegionGap / 5)
	r.issue(s.plan(rows("a", "b", "c"), 2, 4, 4, now), now)
	r.flush()
	if s.live != r.ignoring {
		t.Fatalf("region live=%v but the renderer ignoring=%v", s.live, r.ignoring)
	}
	// After the gap, the rows are taken back.
	now = now.Add(minRegionGap)
	r.issue(s.plan(rows("a", "b", "c"), 2, 4, 4, now), now)
	r.flush()
	if !s.live || !r.ignoring {
		t.Fatalf("after the gap the region should sync: live=%v ignoring=%v", s.live, r.ignoring)
	}
}
