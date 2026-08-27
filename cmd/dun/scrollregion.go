package main

import (
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// Terminal-native scrolling for the conversation pane.
//
// The problem, measured: dun runs a full-height pane pinned to the bottom of
// the alt screen. Bubble Tea's renderer diffs the frame line by line and writes
// only what changed — which is perfect until the pane scrolls, and then EVERY
// line differs because the content moved up by one. Streaming a 2.2KB reply
// wrote 277KB to the terminal (~27KB/s, 30Hz, up to 37 of 40 lines rewritten
// per frame). Locally that is invisible; over ssh to a phone it is the lag.
//
// A terminal already knows how to scroll: set a margin region (DECSTBM) and it
// moves the rows itself, and the app writes only the line that came into view.
// Bubble Tea exposes exactly that as SyncScrollArea/ScrollUp/ScrollDown, with
// the rows inside the region "ignored" by the normal renderer. The API is
// marked deprecated for v2 — it is what v1 has, and v1 is what dun is on.
//
// The region deliberately stops ONE ROW SHORT of the pane's bottom. That last
// row is where streamed text grows a character at a time, and a mid-line edit
// is not a scroll: leaving it under the normal renderer costs one rewritten
// line per frame, while pulling it into the region would force a full-region
// repaint on every token — worse than what this replaces.
//
// DUN_FAST_SCROLL=0 turns it off, because a deprecated rendering path that
// mis-paints on some terminal needs an escape hatch that is not "downgrade".
type scrollRegion struct {
	off      bool      // hard-disabled by env
	live     bool      // the renderer is currently ignoring our rows
	top, bot int       // 1-based inclusive screen rows the region covers
	paneH    int       // the pane height plan() saw when it claimed those rows
	last     []string  // what the region shows right now
	lastAt   time.Time // when the last region command was issued (see minRegionGap)
}

// maxScrollStep bounds how far a single frame may scroll before a repaint is
// the cheaper write: past this, the inserted lines cost more than the region.
const maxScrollStep = 8

// minRegionGap serialises region commands, and it is not an optimisation —
// it is the correctness fix.
//
// Bubble Tea runs every Cmd in its own goroutine (tea.go, handleCommands), so
// two region commands issued in consecutive Updates can reach the renderer in
// either order. These commands are INCREMENTAL: applying "insert this line at
// the top" out of order scrambles the pane. Measured: three ↑ presses inside
// 20ms rendered rows as 38, 36, 37. Streaming never showed it, because its
// commands are ~3 a second.
//
// So at most one is issued per gap, and a change that arrives inside the gap
// schedules a retry rather than racing. The race window is a goroutine
// handoff — microseconds against this gap.
const minRegionGap = 25 * time.Millisecond

// regionTickMsg re-runs the plan after a deferred change (see minRegionGap).
type regionTickMsg struct{}

func regionRetry(after time.Duration) tea.Cmd {
	return tea.Tick(after, func(time.Time) tea.Msg { return regionTickMsg{} })
}

func newScrollRegion() *scrollRegion {
	return &scrollRegion{off: os.Getenv("DUN_FAST_SCROLL") == "0"}
}

// rowsFor is how many rows of a pane of height h the region owns — zero unless
// the pane is still the shape plan() claimed the rows for. View and plan run at
// different moments, and between them a resize or a taller composer can move
// the layout; blanking rows the region no longer covers would leave a hole
// where the conversation should be. Falling back to real content costs one
// repaint on the next plan, and is never wrong.
func (s *scrollRegion) rowsFor(h int) int {
	if s == nil || !s.live || h != s.paneH {
		return 0
	}
	return s.bot - s.top + 1
}

// plan returns the command that makes the terminal show `lines` in the region,
// which is one of: nothing (unchanged), a scroll of a few lines (the fast path
// this exists for), or a full region repaint.
func (s *scrollRegion) plan(lines []string, top, bot, paneH int, now time.Time) tea.Cmd {
	if s == nil || s.off {
		return nil
	}
	// Nothing to do is always safe to answer immediately; anything that WRITES
	// waits its turn.
	if s.live && top == s.top && bot == s.bot && paneH == s.paneH && sameLines(s.last, lines) {
		return nil
	}
	if gap := minRegionGap - now.Sub(s.lastAt); gap > 0 {
		return regionRetry(gap)
	}
	s.lastAt = now
	// A region has to be at least two rows for scrolling to mean anything, and
	// the boundaries have to be real screen rows.
	if top < 1 || bot < top+1 || len(lines) != bot-top+1 {
		return s.disable()
	}
	if !s.live || top != s.top || bot != s.bot || paneH != s.paneH {
		return s.sync(lines, top, bot, paneH)
	}
	// Scrolled down (content moved up, new rows at the bottom): the old tail
	// matches the new head.
	n := len(lines)
	for k := 1; k <= maxScrollStep && k < n; k++ {
		if sameLines(s.last[k:], lines[:n-k]) {
			added := append([]string(nil), lines[n-k:]...)
			s.last = append([]string(nil), lines...)
			//nolint:staticcheck // SA1019: deprecated, but v1.3.10 has no replacement - the only scroll-region mechanism
			return tea.ScrollDown(added, s.top, s.bot)
		}
	}
	// Scrolled up (content moved down, new rows at the top).
	for k := 1; k <= maxScrollStep && k < n; k++ {
		if sameLines(s.last[:n-k], lines[k:]) {
			added := append([]string(nil), lines[:k]...)
			s.last = append([]string(nil), lines...)
			//nolint:staticcheck // SA1019: deprecated, but v1.3.10 has no replacement - the only scroll-region mechanism
			return tea.ScrollUp(added, s.top, s.bot)
		}
	}
	// Something else changed — a re-wrap, a selection, a collapsed block. Paint
	// the whole region; it is what the normal renderer would have cost anyway.
	return s.sync(lines, top, bot, paneH)
}

func (s *scrollRegion) sync(lines []string, top, bot, paneH int) tea.Cmd {
	s.live, s.top, s.bot, s.paneH = true, top, bot, paneH
	s.last = append([]string(nil), lines...)
	//nolint:staticcheck // SA1019: deprecated, but v1.3.10 has no replacement - the only scroll-region mechanism
	return tea.SyncScrollArea(append([]string(nil), lines...), top, bot)
}

// disable hands the rows back to the normal renderer. The renderer's cache
// still holds whatever View last claimed was on those rows — which was blank
// filler while the region was live — so the screen has to be repainted, not
// diffed, or the conversation would vanish under its own placeholder.
func (s *scrollRegion) disable() tea.Cmd {
	if s == nil || !s.live {
		return nil
	}
	s.live, s.last = false, nil
	//nolint:staticcheck // SA1019: deprecated, but v1.3.10 has no replacement - the only scroll-region mechanism
	return tea.Batch(tea.ClearScrollArea, tea.ClearScreen)
}

func sameLines(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// blankRegion replaces the region's rows in a rendered pane with empty ones.
// The renderer never writes ignored rows, so their content is arbitrary — but
// the LINE COUNT is load-bearing: it is what puts every row below the pane
// (divider, composer, status) on the screen row the layout expects.
func blankRegion(pane string, n int) string {
	if n <= 0 {
		return pane
	}
	rows := strings.Split(pane, "\n")
	if n > len(rows) {
		return pane
	}
	for i := 0; i < n; i++ {
		rows[i] = ""
	}
	return strings.Join(rows, "\n")
}
