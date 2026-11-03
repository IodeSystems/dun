// Package textfmt holds the small string helpers both the TUI and the CLI use.
package textfmt

import (
	"fmt"
	"strings"
	"time"
	"unicode"
)

func OnOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func OneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func Clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func Pad(s string, w int) string {
	if n := len([]rune(s)); n < w {
		return s + strings.Repeat(" ", max(0, w-n))
	}
	return s
}

// PlainLine is s on one line, without control characters: a manifest's
// words reach the screen.
func PlainLine(s string) string {
	return OneLine(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s))
}

func Plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// CutFields takes n whitespace-separated fields off the front of s and
// returns them with the rest as typed (its newlines and spacing kept: it is
// a message).
func CutFields(s string, n int) ([]string, string) {
	var out []string
	s = strings.TrimLeft(s, " \t\n")
	for range n {
		if s == "" {
			break
		}
		i := strings.IndexAny(s, " \t\n")
		if i < 0 {
			out, s = append(out, s), ""
			break
		}
		out, s = append(out, s[:i]), strings.TrimLeft(s[i:], " \t\n")
	}
	return out, s
}

// RelTime renders an age the way a person reads one.
func RelTime(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return t.Format("Jan 2")
	}
}
