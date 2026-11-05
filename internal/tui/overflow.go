package tui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
)

// A frame taller than the terminal is a layout bug: every part is supposed to
// be measured into the height budget (convoHeightBudget), and a part that is
// not — or that grows past it — pushes the input off the bottom of the screen,
// where it cannot be seen (USER 2026-11-05: an opened stack of agents drew
// every line of every task). This is the one check that catches it whatever
// part it is: drawView hands its parts here last.
//
// At run time the frame is cut from the TOP, so the input, the divider and the
// status line stay where they are, and the status line says which part was
// too tall. In tests, onOverflow fails the test (main_test.go).

// framePart is one named piece of the frame, for the report.
type framePart struct {
	name string
	text string
}

// onOverflow, when set, is told of every frame too tall for the screen. Tests
// set it to fail; it is nil in dun itself.
var onOverflow func(report string)

// fitFrame joins the parts and, if they come to more rows than h, keeps the
// last h of them with the status row replaced by a report naming the tallest
// part. h <= 0 (no size known yet) is not checked.
func fitFrame(parts []framePart, h, w int) string {
	out := make([]string, len(parts))
	total := 0
	for i, p := range parts {
		out[i] = p.text
		total += lipgloss.Height(p.text)
	}
	frame := strings.Join(out, "\n")
	if h <= 0 || total <= h {
		return frame
	}
	report := overflowReport(parts, total, h)
	if onOverflow != nil {
		onOverflow(report)
	}
	rows := strings.Split(frame, "\n")
	rows = rows[len(rows)-h:]
	rows[len(rows)-1] = fitHints(StErr.Render("⚠ "+report), w)
	return strings.Join(rows, "\n")
}

// overflowReport names the frame's height against the screen's and the part
// that took the most rows: "screen overflow: 140 rows on a 30-row screen —
// activity is 128 rows".
func overflowReport(parts []framePart, total, h int) string {
	tallest, rows := "", 0
	for _, p := range parts {
		if n := lipgloss.Height(p.text); n > rows {
			tallest, rows = p.name, n
		}
	}
	return fmt.Sprintf("screen overflow: %d rows on a %d-row screen — %s is %d rows", total, h, tallest, rows)
}
