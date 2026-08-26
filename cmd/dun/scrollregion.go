package main

import (
	"fmt"
	"os"
	"strings"

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
	off                        bool     // hard-disabled by env
	nSync, nScroll, nNil, nOff int      // DUN_SR_STATS
	live                       bool     // the renderer is currently ignoring our rows
	top, bot                   int      // 1-based inclusive screen rows the region covers
	last                       []string // what the region shows right now
}

// maxScrollStep bounds how far a single frame may scroll before a repaint is
// the cheaper write: past this, the inserted lines cost more than the region.
const maxScrollStep = 8

func newScrollRegion() *scrollRegion {
	return &scrollRegion{off: os.Getenv("DUN_FAST_SCROLL") == "0"}
}

// rows is how many screen rows the region owns.
func (s *scrollRegion) rows() int {
	if s == nil || !s.live {
		return 0
	}
	return s.bot - s.top + 1
}

// plan returns the command that makes the terminal show `lines` in the region,
// which is one of: nothing (unchanged), a scroll of a few lines (the fast path
// this exists for), or a full region repaint.
func (s *scrollRegion) plan(lines []string, top, bot int) tea.Cmd {
	if s == nil || s.off {
		return nil
	}
	if s.nSync+s.nScroll+s.nNil > 0 && (s.nSync+s.nScroll+s.nNil)%50 == 0 {
		s.dumpStats()
	}
	// A region has to be at least two rows for scrolling to mean anything, and
	// the boundaries have to be real screen rows.
	if top < 1 || bot < top+1 || len(lines) != bot-top+1 {
		return s.disable()
	}
	if !s.live || top != s.top || bot != s.bot {
		return s.sync(lines, top, bot)
	}
	if sameLines(s.last, lines) {
		s.nNil++
		return nil
	}
	// Scrolled down (content moved up, new rows at the bottom): the old tail
	// matches the new head.
	n := len(lines)
	for k := 1; k <= maxScrollStep && k < n; k++ {
		if sameLines(s.last[k:], lines[:n-k]) {
			added := append([]string(nil), lines[n-k:]...)
			s.last = append([]string(nil), lines...)
			s.nScroll++
			return tea.ScrollDown(added, s.top, s.bot)
		}
	}
	// Scrolled up (content moved down, new rows at the top).
	for k := 1; k <= maxScrollStep && k < n; k++ {
		if sameLines(s.last[:n-k], lines[k:]) {
			added := append([]string(nil), lines[:k]...)
			s.last = append([]string(nil), lines...)
			return tea.ScrollUp(added, s.top, s.bot)
		}
	}
	// Something else changed — a re-wrap, a selection, a collapsed block. Paint
	// the whole region; it is what the normal renderer would have cost anyway.
	return s.sync(lines, top, bot)
}

func (s *scrollRegion) sync(lines []string, top, bot int) tea.Cmd {
	s.nSync++
	s.live, s.top, s.bot = true, top, bot
	s.last = append([]string(nil), lines...)
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
	s.nOff++
	s.live, s.last = false, nil
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

// dumpStats writes the region's decision counts, for measurement runs.
func (s *scrollRegion) dumpStats() {
	path := os.Getenv("DUN_SR_STATS")
	if path == "" || s == nil {
		return
	}
	f, err := os.Create(path)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "sync=%d scroll=%d nil=%d disable=%d\n", s.nSync, s.nScroll, s.nNil, s.nOff)
}
