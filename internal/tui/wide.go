package tui

import (
	"fmt"
	"regexp"
	"strings"
	"sync"

	"charm.land/glamour/v2"
	"github.com/charmbracelet/x/ansi"
)

// Wide blocks: an answer's fenced code blocks and tables keep their lines.
//
// Prose wraps at the question band's edge (bandCap) because a long line of
// prose is hard to follow back to its start. A line of code or a table row
// wrapped is worse than long: the wrap breaks the very columns it is read
// by. So a code block or table wider than the prose column is rendered at
// its own width, up to the pane's, and when it is wider still each row is cut
// at the pane's edge with what is left said there — "… +N cols →". The rows
// do not wrap, so the block is as many rows as it has lines, and scrolling it
// sideways changes rows in place (no bounce). The cut is bubbleteandmd's
// (WideBlocks, answer.go); the sideways scroll is dun's (panAnswer).
//
// The answer is selected with ↑/↓ like any block; → descends into its wide
// blocks, and then ←/→ (h/l) scroll them sideways, home/end go to an edge,
// and ← at the left edge or esc comes back out — the box keys (box.go), on
// the other axis. Every wide block of one answer scrolls together.

// wideState is an answer's sideways scroll.
type wideState struct {
	off    int  // the first column shown
	max    int  // the largest off, as of the last render (0: nothing is cut)
	inside bool // descended into: ←/→ scroll it
}

// mdSeg is one part of an answer's markdown: prose, or a block that keeps
// its lines (code, a table) with the width it needs.
type mdSeg struct {
	text  string
	wide  bool
	table bool
	need  int // display width the block needs, rendered (estimated from the source)
}

var (
	fenceRe    = regexp.MustCompile("^ {0,3}(```+|~~~+)")
	tableSepRe = regexp.MustCompile(`^\s*\|?\s*:?-+:?\s*(\|\s*:?-+:?\s*)*\|?\s*$`)
)

// glamour's margins around a block: two columns of document margin, and two
// more of a code block's own indent.
const (
	codeMargin  = 4
	tableMargin = 2
)

// splitWide cuts markdown into prose and its fenced code blocks and tables.
// An unclosed fence (a reply still streaming) runs to the end.
func splitWide(src string) []mdSeg {
	lines := strings.Split(src, "\n")
	var out []mdSeg
	var prose []string
	flush := func() {
		if len(prose) > 0 {
			out = append(out, mdSeg{text: strings.Join(prose, "\n")})
			prose = nil
		}
	}
	for i := 0; i < len(lines); i++ {
		if m := fenceRe.FindStringSubmatch(lines[i]); m != nil {
			fence := m[1]
			j := i + 1
			for j < len(lines) && !strings.HasPrefix(strings.TrimLeft(lines[j], " "), fence[:3]) {
				j++
			}
			end := min(j, len(lines)-1)
			need := 0
			for _, l := range lines[i+1 : max(i+1, j)] {
				need = max(need, ansi.StringWidth(strings.ReplaceAll(l, "\t", "    ")))
			}
			flush()
			out = append(out, mdSeg{text: strings.Join(lines[i:end+1], "\n"), wide: true, need: need + codeMargin})
			i = end
			continue
		}
		if strings.Contains(lines[i], "|") && i+1 < len(lines) && tableSepRe.MatchString(lines[i+1]) && strings.Contains(lines[i+1], "-") {
			j := i
			for j < len(lines) && strings.Contains(lines[j], "|") && strings.TrimSpace(lines[j]) != "" {
				j++
			}
			flush()
			rows := lines[i:j]
			out = append(out, mdSeg{text: strings.Join(rows, "\n"), wide: true, table: true, need: tableNeed(rows)})
			i = j - 1
			continue
		}
		prose = append(prose, lines[i])
	}
	flush()
	return out
}

// tableNeed is the width a table's rows need: each column as wide as its
// widest cell, one space either side, a rule between columns.
func tableNeed(rows []string) int {
	var widths []int
	for k, r := range rows {
		if k == 1 {
			continue // the separator row
		}
		r = strings.TrimSpace(r)
		r = strings.TrimPrefix(strings.TrimSuffix(r, "|"), "|")
		for c, cell := range strings.Split(r, "|") {
			if c >= len(widths) {
				widths = append(widths, 0)
			}
			widths[c] = max(widths[c], ansi.StringWidth(strings.TrimSpace(cell)))
		}
	}
	n := tableMargin
	for _, w := range widths {
		n += w + 3
	}
	return n
}

// wideRenderers are glamour renderers by wrap width, for wide blocks. A
// handful of widths in a session; the cache is dropped when it grows past
// wideRendererMax.
var (
	wideMu        sync.Mutex
	wideRenderers = map[int]*glamour.TermRenderer{}
)

const wideRendererMax = 32

func wideRenderer(w int) *glamour.TermRenderer {
	wideMu.Lock()
	defer wideMu.Unlock()
	if r, ok := wideRenderers[w]; ok {
		return r
	}
	if len(wideRenderers) >= wideRendererMax {
		wideRenderers = map[int]*glamour.TermRenderer{}
	}
	r := newMarkdown(w)
	wideRenderers[w] = r
	return r
}

// wideIndicatorRoom is the most columns "… +N cols →" takes from a row.
const wideIndicatorRoom = 16

// trimRightCells drops a rendered row's trailing blanks (glamour pads every
// row to its wrap width), keeping its styles.
func trimRightCells(r string) string {
	plain := ansi.Strip(r)
	w := ansi.StringWidth(strings.TrimRight(plain, " "))
	if w == ansi.StringWidth(plain) {
		return r
	}
	return ansi.Truncate(r, w, "")
}

// cutRow is the part of row r from column off that fits avail columns: a
// dim "…" first when off hides something to its left, and "… +N cols →" at
// the right edge when more is there.
func cutRow(r string, off, avail int) string {
	w := ansi.StringWidth(r)
	if off == 0 && w <= avail {
		return r
	}
	left := ""
	from := off
	if off > 0 && w > off {
		left = StDim.Render("…")
		from++
	}
	lw := ansi.StringWidth(ansi.Strip(left))
	if w-from <= avail-lw {
		return left + ansi.Cut(r, from, w)
	}
	ind := ""
	for room := 12; ; room++ {
		ind = fmt.Sprintf(" … +%d cols →", w-(from+avail-lw-room))
		if ansi.StringWidth(ind) <= room {
			ind = strings.Repeat(" ", max(0, room-ansi.StringWidth(ind))) + ind
			break
		}
	}
	shown := avail - lw - ansi.StringWidth(ind)
	return left + ansi.Cut(r, from, from+shown) + StDim.Render(ind)
}

// proseW is the prose column: what m.md wraps at (newMarkdown's floor too).
func (m tuiModel) proseW() int { return max(20, min(m.mdWidth, bandCap)-2) }

// renderEntry renders a finished answer at avail columns from its source
// (its Stream's Final; the Stream is made again when the widths moved) and
// keeps its sideways scroll in step: created when something is cut, the
// offset clamped when a wider pane needs less of it.
func (m *tuiModel) renderEntry(e *convoEntry, avail int) string {
	pw := m.proseW()
	if key := answerKey(pw, avail); e.ans == nil || e.ansKey != key {
		e.ans, e.ansKey = m.kit().stream(pw, avail)
	}
	text := answerOut(e.ans, e.mdSource, true)
	maxOff := cutMax(text)
	switch {
	case maxOff > 0 && e.wide == nil:
		e.wide = &wideState{max: maxOff}
	case maxOff == 0 && e.wide != nil:
		e.wide.max, e.wide.off, e.wide.inside = 0, 0, false
	case e.wide != nil:
		e.wide.max = maxOff
		e.wide.off = min(e.wide.off, maxOff)
	}
	if e.wide != nil && e.wide.off > 0 {
		text = m.panAnswer(e.mdSource, text, avail, e.wide.off)
	}
	return text
}

// selWide is the selected answer's sideways scroll, when it has one.
func (m *tuiModel) selWide() *wideState {
	if m.sel < 0 || m.sel >= len(m.convo) {
		return nil
	}
	return m.convo[m.sel].wide
}

// insideWide is the answer the person has descended into, or nil.
func (m *tuiModel) insideWide() *wideState {
	if w := m.selWide(); w != nil && w.inside {
		return w
	}
	return nil
}

// wideStep is how far one ←/→ moves: a third of the pane, at least 8.
func (m *tuiModel) wideStep() int { return max(8, m.vp.Width/3) }

// scrollWide moves the selected answer's columns by n, clamped, and has
// refresh render it again.
func (m *tuiModel) scrollWide(w *wideState, n int) {
	w.off = min(max(w.off+n, 0), w.max)
	e := &m.convo[m.sel]
	e.wrapW = 0 // render again at the new offset
	e.invalidateWrap()
}

// insideWideKeys is the keyboard while descended into an answer's wide
// blocks: ←/→ (h/l) scroll, home/end go to an edge, ← at the left edge or
// esc leave; ↑/↓ leave and move on. It reports whether the key was taken.
func (m *tuiModel) insideWideKeys(k string) bool {
	w := m.insideWide()
	if w == nil {
		return false
	}
	switch k {
	case "right", "l":
		m.scrollWide(w, m.wideStep())
	case "left", "h":
		if w.off == 0 {
			w.inside = false
			return true
		}
		m.scrollWide(w, -m.wideStep())
	case "home", "0":
		m.scrollWide(w, -w.off)
	case "end", "$":
		m.scrollWide(w, w.max)
	case "esc":
		w.inside = false
	default:
		w.inside = false // ↑/↓ and the rest: out, and the key does its job
		return false
	}
	return true
}
