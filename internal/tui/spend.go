package tui

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/iodesystems/dun/internal/numfmt"
	"github.com/iodesystems/dun/internal/textfmt"
	"github.com/iodesystems/dun/proto"
)

// Money and limits in the event stream and in /context. The engine's meter is
// dun/spend.go; this turns its snapshot into the wire shape, and the wire shape
// into /context's "cost & limits" rows and the status-line warning.
//
// A value the endpoint did not report is absent on the wire and "not reported"
// on screen — never 0, which would read as free (or as exhausted).

// readSpend decodes a Spend off an event: the "spend" field of a usage event,
// or a "spend" event's own flat payload. A missing one keeps the last snapshot.
func (s *contextStats) readSpend(ev map[string]any) {
	var src any = ev["spend"]
	if str(ev["type"]) == "spend" {
		src = ev
	}
	if src == nil {
		return
	}
	b, err := json.Marshal(src)
	if err != nil {
		return
	}
	var sp proto.Spend
	if json.Unmarshal(b, &sp) != nil {
		return
	}
	if sp.Month == nil && s.spend != nil {
		sp.Month = s.spend.Month // sent once, at start
	}
	s.spend = &sp
}

// ctxMoney is an amount for a /context row, right-aligned with ctxNum's column.
func ctxMoney(f float64) string { return fmt.Sprintf("%9s", numfmt.Money(f)) }

// curNote is the currency after a money value, and what a sub-cent amount
// rounds away.
func curNote(f float64, cur string) string {
	if cur == "" {
		cur = "(currency not stated)"
	}
	n := " " + cur
	if f > 0 && f < 0.005 {
		n += numfmt.Commaf(" (%.4f)", f)
	}
	return n
}

// until says how far off t is, coarsely: "in 3m", "in 2d 4h", "2m ago".
func until(t, now time.Time) string {
	d := t.Sub(now)
	ago := d < 0
	if ago {
		d = -d
	}
	var s string
	switch {
	case d < time.Minute:
		s = fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		s = fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		s = fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		s = fmt.Sprintf("%dd %dh", int(d.Hours())/24, int(d.Hours())%24)
	}
	if ago {
		return s + " ago"
	}
	return "in " + s
}

func parseRFC3339(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	return t, err == nil
}

// costBlock is /context's "cost & limits" section.
// intervalLabel names the interval rollup: the key budget's period when the
// gateway states it, else the last 7 days.
func intervalLabel(sp *proto.Spend) string {
	switch {
	case sp.IntervalOfKey:
		return "key period (" + sp.Interval + ")"
	case sp.Interval != "":
		return "last " + sp.Interval
	}
	return "last 7d"
}

// ledgerRow is one ledger total: the first currency in the value column,
// any others after it.
func ledgerRow(label string, by map[string]float64, tail string) string {
	curs := make([]string, 0, len(by))
	for c := range by {
		curs = append(curs, c)
	}
	sort.Strings(curs)
	if len(curs) == 0 {
		return ctxRow(label, ctxMoney(0), tail)
	}
	note := curNote(by[curs[0]], curs[0])
	for _, c := range curs[1:] {
		note += " + " + numfmt.MoneyOf(by[c], c)
	}
	return ctxRow(label, ctxMoney(by[curs[0]]), note+tail)
}

// ledgerBlock is every session's and sub-agent's spend on this machine,
// from the ledger (/usage's Spend tab): the periods under the all-time
// total, then this month by model, the most first.
func ledgerBlock(s *contextStats) string {
	sp := s.spend
	var b strings.Builder
	b.WriteString("\n  " + stHeader.Render("this machine"))
	b.WriteString(StDim.Render(" — every session and sub-agent, from the ledger"))
	l := (*proto.SpendLedger)(nil)
	if sp != nil {
		l = sp.Ledger
	}
	if l == nil || !l.Entries {
		b.WriteString("\n  " + StDim.Render("nothing spent yet: no response reported a cost this year"))
		return b.String()
	}
	b.WriteString(ledgerRow("machine spend", l.Total, " · all time, every session on this machine"))
	for _, r := range []struct {
		label string
		by    map[string]float64
	}{{"this hour", l.Hour}, {"today", l.Today}, {"this week", l.Week}, {"this month", l.Month},
		{"this year", l.Year}, {intervalLabel(sp), l.Interval}} {
		b.WriteString(ledgerRow("  "+r.label, r.by, ""))
	}
	if len(l.Last24h) > 0 {
		b.WriteString(ledgerRow("this key, 24h", l.Last24h, " · the rate its budget is measured against"))
	}
	if len(l.ByModel) > 0 {
		b.WriteString("\n\n  " + stHeader.Render("this month by model"))
		names := make([]string, 0, len(l.ByModel))
		for n := range l.ByModel {
			names = append(names, n)
		}
		sum := func(by map[string]float64) (t float64) {
			for _, v := range by {
				t += v
			}
			return t
		}
		sort.Slice(names, func(i, j int) bool {
			a, c := sum(l.ByModel[names[i]]), sum(l.ByModel[names[j]])
			if a != c {
				return a > c
			}
			return names[i] < names[j]
		})
		for _, n := range names {
			b.WriteString(ledgerRow("  "+n, l.ByModel[n], ""))
		}
	}
	return b.String()
}

// noCostHint says why calls came back with no cost, and what would change
// that: the answer depends on the gateway.
func noCostHint(sp *proto.Spend) string {
	src := ""
	if sp.Key != nil {
		src = sp.Key.Source
	}
	switch {
	case strings.HasPrefix(src, "litellm"):
		return "LiteLLM sends a cost only on non-streaming responses, and dun streams: the gateway's operator can set " +
			"include_cost_in_streaming_usage to put it in the stream (key spend above still counts it)"
	case src == "openrouter":
		return "OpenRouter normally puts a cost in every response's usage; none arrived — check the URL points at openrouter.ai"
	}
	return "this endpoint sends no cost with its responses (LiteLLM and OpenRouter do); dun has no price table to estimate one"
}

func costBlock(s *contextStats, now time.Time) string {
	var b strings.Builder
	b.WriteString("\n\n  " + stHeader.Render("cost & limits"))
	b.WriteString(StDim.Render(" — what the gateway reported"))
	sp := s.spend
	if sp == nil {
		sp = &proto.Spend{}
	}
	notReported := func(label, why string) {
		b.WriteString(ctxRow(label, fmt.Sprintf("%9s", "—"), " not reported"+why))
	}

	// What this session spent.
	if sp.Session != nil {
		note := curNote(*sp.Session, sp.Currency)
		if sp.CostedCalls < sp.Calls {
			note += numfmt.Commaf(" · %d of %d calls priced", sp.CostedCalls, sp.Calls)
		}
		if sp.Mixed {
			note += " · mixed currencies"
		}
		b.WriteString(ctxRow("session spend", ctxMoney(*sp.Session), note))
		cur := curNote(0, sp.Currency)
		for _, r := range []struct {
			label string
			v     *float64
		}{{"  this hour", sp.SessionHour}, {"  today", sp.SessionToday}, {"  " + intervalLabel(sp), sp.SessionInterval}} {
			if r.v != nil {
				b.WriteString(ctxRow(r.label, ctxMoney(*r.v), cur))
			}
		}
	} else {
		notReported("session spend", numfmt.Commaf(" (%d calls, none priced)", sp.Calls))
	}
	if sp.Session == nil && sp.Calls > 0 {
		b.WriteString("\n    " + StDim.Render(noCostHint(sp)))
	}
	if sp.LastTurn != nil {
		note := curNote(*sp.LastTurn, sp.Currency)
		if sp.LastTurnCosted < sp.LastTurnCalls {
			note += numfmt.Commaf(" · %d of %d calls priced", sp.LastTurnCosted, sp.LastTurnCalls)
		}
		b.WriteString(ctxRow("last turn", ctxMoney(*sp.LastTurn), note))
	} else {
		notReported("last turn", "")
	}

	// The key's budget.
	switch k := sp.Key; {
	case k != nil:
		src := ""
		if k.AsOf != "" {
			if t, ok := parseRFC3339(k.AsOf); ok {
				src = " · as of " + until(t, now)
			}
		}
		// agentkit's KeyBudget.Spend is the current budget period's, or all
		// time when the budget does not reset.
		period := " · this budget period"
		if k.ResetAt == "" && k.Interval == "" {
			period = " · all time (no reset stated)"
		}
		b.WriteString(ctxRow("key spend", ctxMoney(k.Spend), curNote(k.Spend, k.Currency)+period+src))
		if k.Limit != nil {
			b.WriteString(ctxRow("key limit", ctxMoney(*k.Limit), curNote(*k.Limit, k.Currency)))
		} else {
			notReported("key limit", " (none set, or not stated)")
		}
		if k.Remaining != nil {
			note := curNote(*k.Remaining, k.Currency)
			if k.Limit != nil && *k.Limit > 0 {
				note += numfmt.Commaf(" (%.0f%%)", 100**k.Remaining / *k.Limit)
			}
			b.WriteString(ctxRow("key remaining", ctxMoney(*k.Remaining), note))
		} else {
			notReported("key remaining", "")
		}
		if t, ok := parseRFC3339(k.ResetAt); ok {
			note := " · " + t.UTC().Format("2006-01-02 15:04 UTC")
			if k.Interval != "" {
				note += " · every " + k.Interval
			}
			b.WriteString(ctxRow("key resets", fmt.Sprintf("%9s", until(t, now)), note))
		} else if k.Interval != "" {
			b.WriteString(ctxRow("key resets", fmt.Sprintf("%9s", k.Interval), " (no date stated)"))
		} else {
			notReported("key resets", "")
		}
	case sp.KeyUnsupported:
		notReported("key budget", " (the endpoint has none)")
	case sp.KeyError != "":
		notReported("key budget", " (read failed: "+textfmt.Clip(sp.KeyError, 60)+")")
	default:
		notReported("key budget", "")
	}

	// Rate limits, as the latest response reported them.
	if len(sp.Limits) == 0 {
		notReported("rate limits", "")
	}
	for _, r := range sp.Limits {
		label := r.Name + " left"
		if r.Remaining == nil {
			notReported(label, "")
			continue
		}
		note := ""
		if r.Limit != nil {
			note = numfmt.Commaf(" of %d", *r.Limit)
			if *r.Limit > 0 {
				note += numfmt.Commaf(" (%.0f%%)", 100*float64(*r.Remaining)/float64(*r.Limit))
			}
		}
		if t, ok := parseRFC3339(r.ResetAt); ok {
			if t.Before(now) {
				note += " · reset " + until(t, now) + " (stale)"
			} else {
				note += " · resets " + until(t, now)
			}
		}
		b.WriteString(ctxRow(label, fmt.Sprintf("%9s", numfmt.Grouped(*r.Remaining)), note))
	}
	return b.String()
}

// Warning thresholds for the status line: a budget or a rate-limit window under
// warnFraction left gets a short dim note; under loudFraction (or empty) it is
// coloured.
const (
	warnFraction = 0.10
	loudFraction = 0.02
)

// spendWarning is the status-line note for the scarcest of the key budget and
// the rate-limit windows, "" when none is under warnFraction. A window whose
// reset has passed is ignored: it has refilled since it was reported.
func spendWarning(sp *proto.Spend, now time.Time) (text string, loud bool) {
	if sp == nil {
		return "", false
	}
	best := math.Inf(1)
	consider := func(f float64, what string) {
		if f < warnFraction && f < best {
			best = f
			text = fmt.Sprintf("%s %.0f%% left", what, math.Max(0, f*100))
		}
	}
	if k := sp.Key; k != nil && k.Limit != nil && k.Remaining != nil && *k.Limit > 0 {
		reset, ok := parseRFC3339(k.ResetAt)
		if !ok || reset.After(now) {
			consider(*k.Remaining / *k.Limit, "key budget")
		}
	}
	for _, r := range sp.Limits {
		if r.Limit == nil || r.Remaining == nil || *r.Limit <= 0 {
			continue
		}
		if t, ok := parseRFC3339(r.ResetAt); ok && !t.After(now) {
			continue
		}
		consider(float64(*r.Remaining)/float64(*r.Limit), r.Name)
	}
	if text == "" {
		return "", false
	}
	return text, best < loudFraction
}

// spendStatus renders the warning for the status line, with its separator; ""
// when there is nothing to warn about.
func (m tuiModel) spendStatus(now time.Time) string {
	text, loud := spendWarning(m.ctxStats.spend, now)
	if text == "" {
		return ""
	}
	if loud {
		return stNote.Render("⚠ "+text) + StDim.Render(" · ")
	}
	return StDim.Render("⚠ " + text + " · ")
}
