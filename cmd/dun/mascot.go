package main

import (
	"os"
	"strconv"
	"strings"

	"github.com/charmbracelet/colorprofile"
)

// The foreman: dun's mascot, a hard hat over a mustache, drawn in coloured
// half blocks on a fresh session's splash (owner, 2026-10-23: "he looks so
// shitty, it's fucking great"). Not on /status, not on a resumed session
// (it has no splash), and not on a terminal with fewer than 256 colours,
// where the splash's first row is the one line mascotPlain.
//
// Each grid is pixels, two to a terminal row: ▀ with the top pixel as the
// foreground and the bottom one as the background; ▄ or a space where a half
// is transparent, which is the terminal's own background. The grids are
// ported verbatim from the owner-approved drawing (scratchpad pix.py).

// mascotColors are the grid letters' xterm-256 colours.
var mascotColors = map[byte]int{
	'Y': 220, // hat
	'y': 172, // brim, ridges
	'h': 230, // shine
	'k': 236, // outline, brows
	's': 180, // skin
	'd': 137, // shadow
	'w': 255, // eye
	'm': 94,  // mustache
	'b': 26,  // shirt
	'C': 19,  // collar, a shade darker than the shirt
	'K': 18,  // the collar's crease
	'u': 230, // undershirt: one off-white pixel at the bottom of the V
}

// mascot is the foreman, 13×11 pixels: 6 rows, at every width (owner,
// 2026-10-24: the small one over the 10-row one). A blue work shirt whose
// collar stands beside the neck and folds down, the crease the darkest blue.
var mascot = []string{
	"....yYYYy....",
	"...YYyYyYY...",
	"..YYhYyYYYY..",
	".yyyyyyyyyyy.",
	"..skksssskks.",
	"..sswssssws..",
	"..sssmmmmss..",
	"..ssmmmmmms..",
	"..CdskkksdC..",
	".bbCKsssKCbb.",
	"bbbbbKuKbbbbb",
}

// The taglines: the long one where it fits, the short one below that, and
// the one line a terminal without 256 colours gets in place of the foreman.
const (
	taglineLong  = "we ain't perfect. we ain't well equipped. but darn it, we'll find a way."
	taglineShort = "ain't pretty. gets it dun."
	mascotPlain  = "git 'er dun"
)

// taglineLongW is the pane width from which the long tagline shows;
// mascotNarrowW the width below which the rows beside him use their short
// labels.
const (
	taglineLongW  = 120
	mascotNarrowW = 80
)

// mascotArt is a grid rendered: one string per terminal row, each exactly
// the grid's width in cells, every row ending with its colours reset.
func mascotArt(grid []string) []string {
	w := 0
	for _, r := range grid {
		w = max(w, len(r))
	}
	rows := make([]string, len(grid), len(grid)+1)
	for i, r := range grid {
		rows[i] = r + strings.Repeat(".", w-len(r))
	}
	if len(rows)%2 == 1 {
		rows = append(rows, strings.Repeat(".", w))
	}
	fg := func(c byte) string { return strconv.Itoa(mascotColors[c]) }
	out := make([]string, 0, len(rows)/2)
	for i := 0; i < len(rows); i += 2 {
		var b strings.Builder
		top, bot := rows[i], rows[i+1]
		for x := 0; x < w; x++ {
			t, u := top[x], bot[x]
			switch {
			case t == '.' && u == '.':
				b.WriteString("\x1b[0m ")
			case u == '.':
				b.WriteString("\x1b[0;38;5;" + fg(t) + "m▀")
			case t == '.':
				b.WriteString("\x1b[0;38;5;" + fg(u) + "m▄")
			default:
				b.WriteString("\x1b[0;38;5;" + fg(t) + ";48;5;" + fg(u) + "m▀")
			}
		}
		b.WriteString("\x1b[0m")
		out = append(out, b.String())
	}
	return out
}

// mascotWidth is a grid's width in cells.
func mascotWidth(grid []string) int {
	w := 0
	for _, r := range grid {
		w = max(w, len(r))
	}
	return w
}

// mascotColour is whether this terminal draws the foreman: 256 colours or
// more. Resolved once at TUI start (setupMascot); the zero value (tests,
// a terminal not asked) is the plain line.
var mascotColour bool

// setupMascot resolves mascotColour for this terminal. Call after
// initMarkdownStyle ("notty" is no colour at all).
func setupMascot() {
	mascotColour = mdStyle != "notty" && mascotProfileOK(colorprofile.Detect(os.Stdout, os.Environ()))
}

// mascotProfileOK: 256 colours or true colour.
func mascotProfileOK(p colorprofile.Profile) bool {
	return p == colorprofile.TrueColor || p == colorprofile.ANSI256
}

// mascotFor is the foreman for a pane w columns wide and the tagline beside
// him, or no grid and the plain line on a terminal that cannot draw him.
func mascotFor(w int) (grid []string, tagline string) {
	switch {
	case !mascotColour:
		return nil, mascotPlain
	case w >= taglineLongW:
		return mascot, taglineLong
	}
	return mascot, taglineShort
}
