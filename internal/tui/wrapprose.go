package tui

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/cellbuf"
)

// typedRowAt is a message the user typed, drawn as the composer drew it: every
// line break kept, each line wrapped by the composer's own wrapLine at the
// composer's text width (width-2: the "› " / "  " prompt takes two cells), and
// continuation rows indented under the prompt. The reading cap does not apply:
// the user chose those line lengths. The band fills to bandCap, or to the
// widest row when the text runs past it.
func typedRowAt(text string, width int) string {
	tw := max(1, width-2)
	var rows []string
	for _, line := range strings.Split(text, "\n") {
		buf := []rune(line)
		// A line with a URL: link the whole line, then cut the rows out of it
		// by column, so a URL that wraps stays one link (relinkRows below).
		linked := ""
		if strings.Contains(line, "http") && !strings.Contains(line, "\t") {
			if l := linkifyStyled(line); l != line {
				linked = l
			}
		}
		for _, s := range wrapLine(buf, 0, len(buf), tw) {
			if linked == "" {
				rows = append(rows, string(buf[s.start:s.end]))
				continue
			}
			c0 := ansi.StringWidth(string(buf[:s.start]))
			rows = append(rows, ansi.Cut(linked, c0, c0+ansi.StringWidth(string(buf[s.start:s.end]))))
		}
	}
	fill := min(width, bandCap)
	for i, r := range rows {
		p := "  "
		if i == 0 {
			p = "› "
		}
		fill = min(width, max(fill, 2+ansi.StringWidth(r)))
		rows[i] = p + r
	}
	return stUser.Width(fill).Render(relinkRows(strings.Join(rows, "\n")))
}

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
