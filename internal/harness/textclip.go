package harness

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/iodesystems/dun/internal/textfmt"
)

// workClip is s on one line, cut at max runes.
func workClip(s string, max int) string {
	return textfmt.ClipRune(textfmt.OneLine(s), max)
}

// headLine is the first non-blank line of s, cut at max runes.
func headLine(s string, max int) string {
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			return workClip(l, max)
		}
	}
	return ""
}

// clipBlock keeps line breaks and cuts at max runes.
func clipBlock(s string, max int) string {
	return textfmt.ClipRune(strings.TrimSpace(s), max)
}

// boundLines keeps lines from the start and the end, at most max runes in
// all, with one line in between saying how many were left out.
func boundLines(lines []string, max int) []string {
	total := 0
	for _, l := range lines {
		total += utf8.RuneCountInString(l) + 1
	}
	if total <= max {
		return lines
	}
	var head, tail []string
	used, i, j := 0, 0, len(lines)-1
	for i <= j {
		// Alternate: the start says what was asked, the end what came of it.
		l := lines[i]
		if n := utf8.RuneCountInString(l) + 1; used+n <= max/2 || len(head) == 0 {
			head = append(head, clipBlock(l, max/2))
			used += min(n, max/2)
			i++
		} else {
			break
		}
	}
	for j >= i {
		l := lines[j]
		n := utf8.RuneCountInString(l) + 1
		if used+n > max {
			break
		}
		tail = append([]string{l}, tail...)
		used += n
		j--
	}
	out := head
	if left := j - i + 1; left > 0 {
		out = append(out, fmt.Sprintf("(%d events left out here)", left))
	}
	return append(out, tail...)
}

func workDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}
