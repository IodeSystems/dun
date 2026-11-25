package repairs

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/iodesystems/dun/internal/numfmt"
)

// Style colors the text: Head a section's title, Dim a note, Warn a flagged
// cell. Plain (the CLI) leaves the text as it is.
type Style struct {
	Head, Dim, Warn func(string) string
}

// Plain is the CLI's style.
var Plain = Style{Head: id, Dim: id, Warn: id}

func id(s string) string { return s }

// SlowCmd is where a command's time is flagged: slower than this is felt.
const SlowCmd = 300 * time.Millisecond

// Table lays out rows: the first column left-aligned, the rest right, each
// as wide as its widest cell; head is the first row. Each line starts with
// indent. A row's cells past the head's count are joined as a tail.
func Table(indent string, head []string, rows [][]string) string {
	w := make([]int, len(head))
	for _, r := range append([][]string{head}, rows...) {
		for i := 0; i < len(r) && i < len(w); i++ {
			w[i] = max(w[i], len([]rune(r[i])))
		}
	}
	var b strings.Builder
	for _, r := range append([][]string{head}, rows...) {
		b.WriteString(indent)
		for i := 0; i < len(w); i++ {
			c := ""
			if i < len(r) {
				c = r[i]
			}
			pad := strings.Repeat(" ", w[i]-len([]rune(c)))
			if i == 0 {
				b.WriteString(c + pad)
			} else {
				b.WriteString("  " + pad + c)
			}
		}
		if len(r) > len(w) {
			b.WriteString("  " + strings.Join(r[len(w):], " "))
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// RenderRepairs is the Repairs tab, and `dun usage --repairs`.
func RenderRepairs(r Report, st Style) string {
	var b strings.Builder
	b.WriteString("  " + st.Head("cost of repairs"))
	b.WriteString(st.Dim(" — the rounds that were resends or repair rounds, and the backoff waited") + "\n")
	b.WriteString(Table("    ", []string{"", "rounds", "tokens", "cost", "waited"}, [][]string{
		costRow("this month", r.Cost), costRow("today", r.CostToday)}))
	b.WriteString("\n\n  " + st.Head("repairs") + st.Dim(" since "+r.Since.Format("Jan 2")+", by kind, then provider · model") + "\n")
	if len(r.Groups) == 0 {
		b.WriteString("    " + st.Dim("none recorded this month") + "\n")
	} else {
		var rows [][]string
		for _, g := range r.Groups {
			rows = append(rows, countRow(g.Kind, g.Count))
			models := make([]string, 0, len(g.ByModel))
			for m := range g.ByModel {
				models = append(models, m)
			}
			sort.Slice(models, func(i, j int) bool {
				a, c := g.ByModel[models[i]], g.ByModel[models[j]]
				if a.N+a.OK+a.Failed != c.N+c.OK+c.Failed {
					return a.N+a.OK+a.Failed > c.N+c.OK+c.Failed
				}
				return models[i] < models[j]
			})
			if len(models) == 1 {
				rows[len(rows)-1] = append(rows[len(rows)-1], st.Dim(models[0]))
				continue
			}
			for _, m := range models {
				rows = append(rows, countRow("  "+m, g.ByModel[m]))
			}
		}
		b.WriteString(Table("    ", []string{"kind", "count", "today", "ok", "failed", "waited"}, rows) + "\n")
	}
	b.WriteString("\n  " + st.Head("tool errors") + st.Dim(" by tool: ERROR results, refused arguments, unknown tools, and node_edit landed with a caveat (warned)") + "\n")
	if len(r.Tools) == 0 {
		b.WriteString("    " + st.Dim("none recorded this month") + "\n")
	} else {
		var rows [][]string
		for _, t := range r.Tools {
			causes := make([]string, 0, len(t.ByCause))
			for c := range t.ByCause {
				causes = append(causes, c)
			}
			sort.Strings(causes)
			var by []string
			for _, c := range causes {
				by = append(by, fmt.Sprintf("%s %s", c, numfmt.Short(t.ByCause[c])))
			}
			rows = append(rows, []string{t.Tool, numfmt.Short(t.N), strings.Join(by, " · ")})
		}
		b.WriteString(Table("    ", []string{"tool", "errors"}, rows) + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func costRow(label string, c Cost) []string {
	cost := "-"
	if c.Cost > 0 {
		cost = numfmt.TurnMoney(c.Cost, c.Currency)
	}
	return []string{label, numfmt.Short(c.Rounds), numfmt.Short(c.Tokens), cost, waitText(c.WaitMS)}
}

func countRow(label string, c Count) []string {
	dash := func(n int) string {
		if n == 0 {
			return "-"
		}
		return numfmt.Short(n)
	}
	n := c.N
	if n == 0 { // only outcomes: a resolution line counts what it resolves
		n = c.OK + c.Failed
	}
	return []string{label, dash(n), dash(c.Today), dash(c.OK), dash(c.Failed), waitText(c.WaitMS)}
}

func waitText(ms int64) string {
	if ms <= 0 {
		return "-"
	}
	d := time.Duration(ms) * time.Millisecond
	if d < time.Minute {
		return numfmt.Secs(float64(ms))
	}
	return d.Round(time.Second).String()
}

// RenderLatency is the per-model round timing table.
func RenderLatency(ls []Latency, st Style) string {
	if len(ls) == 0 {
		return "    " + st.Dim("no rounds recorded this month")
	}
	rate := func(v float64) string {
		if v <= 0 {
			return "-"
		}
		return numfmt.Rate(v)
	}
	var rows [][]string
	for _, l := range ls {
		rows = append(rows, []string{l.Model, numfmt.Short(l.Rounds),
			numfmt.Secs(float64(l.LatencyP50)), numfmt.Secs(float64(l.LatencyP95)),
			rate(l.PrefillP50), rate(l.PrefillP95), rate(l.GenerateP50), rate(l.GenerateP95)})
	}
	return Table("    ", []string{"model", "rounds", "p50", "p95", "prefill p50", "p95", "gen p50", "p95"}, rows)
}

// RenderCmds is a command-time table, slowest first; a time over SlowCmd
// is flagged.
func RenderCmds(cs []Cmd, st Style) string {
	if len(cs) == 0 {
		return "    " + st.Dim("none timed yet")
	}
	ms := func(v int64) string {
		s := numfmt.Secs(float64(v))
		if time.Duration(v)*time.Millisecond > SlowCmd {
			return s + "!"
		}
		return s
	}
	var rows [][]string
	for _, c := range cs {
		rows = append(rows, []string{c.Cmd, c.Surface, numfmt.Short(c.N), ms(c.P50), ms(c.P95), ms(c.Max), ms(c.Last)})
	}
	t := Table("    ", []string{"command", "where", "n", "p50", "p95", "max", "last"}, rows)
	// The flag is colored after the layout, so the widths count plain text.
	return strings.ReplaceAll(t, "!", st.Warn("!"))
}
