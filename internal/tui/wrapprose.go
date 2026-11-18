package tui

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/cellbuf"
)

// wrapProse wraps s to w columns at spaces only: a word is never broken at
// a hyphen or slash, so a URL moves to the next row whole when it fits one.
// A word wider than a row (a long URL) is cut at the row's edge, on a row of
// its own after the words before it. Existing newlines stay.
func wrapProse(s string, w int) string {
	if w <= 0 {
		return s
	}
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if ansi.StringWidth(line) <= w {
			out = append(out, line)
			continue
		}
		cur, curW := "", 0
		flush := func() {
			if cur != "" {
				out = append(out, cur)
			}
			cur, curW = "", 0
		}
		for _, word := range strings.Split(line, " ") {
			ww := ansi.StringWidth(word)
			switch {
			case cur != "" && curW+1+ww <= w:
				cur += " " + word
				curW += 1 + ww
			case ww <= w:
				flush()
				cur, curW = word, ww
			default:
				flush()
				parts := strings.Split(cellbuf.Wrap(word, w, ""), "\n")
				out = append(out, parts[:len(parts)-1]...)
				cur, curW = parts[len(parts)-1], ansi.StringWidth(parts[len(parts)-1])
			}
		}
		flush()
	}
	return strings.Join(out, "\n")
}
