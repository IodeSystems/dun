package tui

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The person's asks (USER 2026-11-18): a ledger of their own, not plan
// nodes, listed at the top of /plan from what dund sends (the plan event's
// asks and intake, harness asktool.go). The intake (the messages the model
// has not acknowledged) is one row; only the ask the turn works shows
// working, the other open ones "open · <age>"; done and filed asks fold
// into one row, "▸ N done · M filed", opened to list them. A plan node
// that serves an ask is tagged "ask N". /journal folds the same way.

// The ask list's rows (planView.order): none is a plan node.
const (
	planIntakeRow = -3
	planAskRow    = -4
	planAsksRow   = -5
)

// planAsk is one ask as dund sent it.
type planAsk struct {
	n            int
	title, state string
	why          string // blocked: the question
	at           time.Time
	working      bool
	nodes        []string
}

func (a planAsk) closed() bool { return a.state != "open" && a.state != "blocked" }

// planIntake is the person's messages the model has not acknowledged.
type planIntake struct {
	msgs    []planMsg
	working bool
}

type planMsg struct {
	at   time.Time
	text string
}

// parseAsks reads a plan event's asks and intake.
func parseAsks(ev evMsg) ([]planAsk, *planIntake) {
	var asks []planAsk
	xs, _ := ev["asks"].([]any)
	for _, x := range xs {
		o, _ := x.(map[string]any)
		a := planAsk{n: intOf(o["n"]), title: str(o["title"]), state: str(o["state"]), why: str(o["why"]),
			at: time.Unix(int64(intOf(o["at"])), 0), nodes: strList(o["nodes"])}
		a.working, _ = o["working"].(bool)
		asks = append(asks, a)
	}
	o, ok := ev["intake"].(map[string]any)
	if !ok {
		return asks, nil
	}
	in := &planIntake{}
	in.working, _ = o["working"].(bool)
	ms, _ := o["messages"].([]any)
	for _, x := range ms {
		mo, _ := x.(map[string]any)
		in.msgs = append(in.msgs, planMsg{at: time.Unix(int64(intOf(mo["at"])), 0), text: str(mo["text"])})
	}
	if len(in.msgs) == 0 {
		in = nil
	}
	return asks, in
}

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

// askClip is s's first line, cut at n runes with an ellipsis.
func askClip(s string, n int) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// working is the spinner while a turn runs and the row is the one it works.
func (v *planView) working(on bool) (string, bool) {
	if !on || !v.live.busy {
		return "", false
	}
	v.animating = true
	return planSpin[v.live.phase%len(planSpin)], true
}

// askNodes appends the ask list's rows: the intake, the open asks, and the
// fold of the closed ones.
func (v *planView) askNodes(nodes []pageNode) []pageNode {
	now := v.clock()
	if in := v.intake; in != nil {
		n := len(in.msgs)
		head := "▸ new · " + strconv.Itoa(n) + " message"
		if n != 1 {
			head += "s"
		}
		head += " · " + strconv.Quote(askClip(in.msgs[0].text, 30))
		if n > 1 {
			head += " +" + strconv.Itoa(n-1)
		}
		style := &stNote
		if spin, ok := v.working(in.working); ok {
			head, style = spin+" "+head+"  · working", nil
		} else {
			head += "  · " + askAge(now, in.msgs[0].at)
		}
		var body []string
		for _, m := range in.msgs {
			body = append(body, m.at.Local().Format("15:04")+"  "+m.text)
		}
		nodes = append(nodes, pageNode{head: head, body: strings.Join(body, "\n\n"), pre: true, style: style})
		v.order = append(v.order, planIntakeRow)
		v.keys = append(v.keys, "intake")
	}
	var counts askCounts
	var folded []string
	for _, a := range v.asks {
		line := fmt.Sprintf("ask %d %s", a.n, a.title)
		if a.closed() {
			counts.add(a.state)
			line += "  · " + a.state
			if len(a.nodes) > 0 {
				line += " → " + strings.Join(a.nodes, ", ")
			}
			folded = append(folded, line)
			continue
		}
		mark, tag := "○", "open · "+askAge(now, a.at)
		if spin, ok := v.working(a.working); ok {
			mark, tag = spin, "working"
		}
		if a.state == "blocked" {
			mark, tag = "⚠", "blocked: "+a.why
		}
		head := mark + " " + line + "  · " + tag
		var body []string
		if len(a.nodes) > 0 {
			body = append(body, "plan nodes: "+strings.Join(a.nodes, ", "))
		}
		nodes = append(nodes, pageNode{head: head, body: strings.Join(body, "\n"), pre: true, err: a.state == "blocked"})
		v.order = append(v.order, planAskRow)
		v.keys = append(v.keys, "ask#"+strconv.Itoa(a.n))
	}
	if len(folded) > 0 {
		nodes = append(nodes, pageNode{head: counts.head(), body: strings.Join(folded, "\n"), pre: true, style: &StDim})
		v.order = append(v.order, planAsksRow)
		v.keys = append(v.keys, "asks")
	}
	return nodes
}
