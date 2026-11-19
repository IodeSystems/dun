package tui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
)

// The person's asks on /plan and /journal (USER 2026-11-18): the intake (the
// messages the model has not acknowledged) is one row above the asks; only
// the ask the turn works shows working, the other open ones "open · <age>";
// done and filed asks fold into one row, "▸ N done · M filed", opened to
// list them. The engine says which ask works and what state each is in
// (harness asktool.go); the screens only draw it.

// The intake's and the asks fold's rows (planView.order, keys).
const (
	planIntakeRow = -3
	planAsksRow   = -4
	planIntakeKey = "intake"
	planAsksKey   = "asks"
)

// askCounts counts closed asks by state for the fold's row.
type askCounts struct{ done, filed, dropped, gone int }

func (c *askCounts) add(state string) {
	switch state {
	case "done":
		c.done++
	case "filed":
		c.filed++
	case "dropped":
		c.dropped++
	default:
		c.gone++
	}
}

// head is "▸ 3 done · 2 filed", with dropped and gone when there are any.
func (c askCounts) head() string {
	parts := []string{fmt.Sprintf("%d done", c.done), fmt.Sprintf("%d filed", c.filed)}
	if c.dropped > 0 {
		parts = append(parts, fmt.Sprintf("%d dropped", c.dropped))
	}
	if c.gone > 0 {
		parts = append(parts, fmt.Sprintf("%d gone", c.gone))
	}
	return "▸ " + strings.Join(parts, " · ")
}

// askAge is how long ago t was, short: "now", "5m", "3h", "2d".
func askAge(now, t time.Time) string {
	d := now.Sub(t)
	switch {
	case t.IsZero() || d < time.Minute:
		return "now"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m"
	case d < 48*time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h"
	}
	return strconv.Itoa(int(d.Hours()/24)) + "d"
}

func (v *planView) clock() time.Time {
	if v.now != nil {
		return v.now()
	}
	return time.Now()
}

// intakeHead is the intake's row: `▸ new · 3 messages · "the tools and
// stubs dir…" +2`, working while the turn works it.
func (v *planView) intakeHead() (string, *lipgloss.Style) {
	in := v.intake
	n := len(in.msgs)
	head := "▸ new · " + strconv.Itoa(n) + " message"
	if n != 1 {
		head += "s"
	}
	head += " · " + strconv.Quote(askClip(in.msgs[0].text, 30))
	if n > 1 {
		head += " +" + strconv.Itoa(n-1)
	}
	if in.working && v.live.busy {
		v.animating = true
		return planSpin[v.live.phase%len(planSpin)] + " " + head + "  · working", nil
	}
	return head + "  · " + askAge(v.clock(), in.msgs[0].at), &stNote
}

// askClip is s's first line, cut at n runes with an ellipsis.
func askClip(s string, n int) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// askMotion is an open ask row's mark and tag: working on the one the turn
// works while a turn runs, else "open · <age>".
func (v *planView) askMotion(r planRow) (mark, tag string) {
	a := r.ask
	if a.working && v.live.busy {
		v.animating = true
		return planSpin[v.live.phase%len(planSpin)], "working"
	}
	if a.state == "blocked" {
		return planMark(r.status), ""
	}
	return planMark(r.status), "open · " + askAge(v.clock(), a.at)
}
