package main

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/iodesystems/dun"
	"github.com/iodesystems/dun/proto"
)

// Money and limits in the event stream and in /context. The engine's meter is
// dun/spend.go; this turns its snapshot into the wire shape, and the wire shape
// into /context's "cost & limits" rows and the status-line warning.
//
// A value the endpoint did not report is absent on the wire and "not reported"
// on screen — never 0, which would read as free (or as exhausted).

// spendProto is the wire shape of a meter snapshot.
func spendProto(s dun.SpendSnapshot, now time.Time) *proto.Spend {
	p := &proto.Spend{
		Currency: s.Currency, Mixed: s.Mixed,
		Calls: s.Calls, CostedCalls: s.CostedCalls,
		LastTurnCalls: s.LastTurnCalls, LastTurnCosted: s.LastTurnCosted,
		KeyUnsupported: s.BudgetUnsupported, KeyError: s.BudgetErr,
	}
	if s.CostedCalls > 0 {
		p.Session = ptr(s.Session)
	}
	if s.TurnEnded && s.LastTurnCosted > 0 {
		p.LastTurn = ptr(s.LastTurn)
	}
	if b := s.Budget; b.Reported {
		k := &proto.KeyBudget{Spend: b.Spend, Interval: b.Interval, Currency: b.Currency, Source: b.Source}
		if b.HasLimit {
			k.Limit = ptr(b.Limit)
		}
		if b.HasRemaining {
			k.Remaining = ptr(b.Remaining)
		}
		if !b.ResetAt.IsZero() {
			k.ResetAt = b.ResetAt.UTC().Format(time.RFC3339)
		}
		if !s.BudgetAt.IsZero() {
			k.AsOf = s.BudgetAt.UTC().Format(time.RFC3339)
		}
		p.Key = k
	}
	for _, w := range s.Limits.Windows() {
		r := proto.RateLimit{Name: w.Name}
		if w.Window.Limit > 0 {
			r.Limit = ptr(w.Window.Limit)
		}
		if w.Window.HasRemaining {
			r.Remaining = ptr(w.Window.Remaining)
		}
		if !w.Window.ResetAt.IsZero() {
			r.ResetAt = w.Window.ResetAt.UTC().Format(time.RFC3339)
		}
		p.Limits = append(p.Limits, r)
	}
	if !s.LimitsAt.IsZero() {
		p.LimitsAt = s.LimitsAt.UTC().Format(time.RFC3339)
	}
	if s.LedgerOn {
		l := s.Ledger
		p.Ledger = &proto.SpendLedger{Today: l.Today, Week: l.Week, Month: l.Month, Year: l.Year,
			ByModel: l.ByModel, Last24h: l.Last24h, Entries: l.Entries}
	}
	return p
}

func ptr[T any](v T) *T { return &v }

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
	s.spend = &sp
}

// money is an amount with thousands separators and two decimals.
func money(f float64) string {
	cents := int64(math.Round(f * 100))
	return fmt.Sprintf("%s.%02d", grouped(cents/100), abs64(cents%100))
}

func abs64(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

// ctxMoney is an amount for a /context row, right-aligned with ctxNum's column.
func ctxMoney(f float64) string { return fmt.Sprintf("%9s", money(f)) }

// curNote is the currency after a money value, and what a sub-cent amount
// rounds away.
func curNote(f float64, cur string) string {
	if cur == "" {
		cur = "(currency not stated)"
	}
	n := " " + cur
	if f > 0 && f < 0.005 {
		n += commaf(" (%.4f)", f)
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
	b.WriteString(stDim.Render(" — what the gateway reported"))
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
			note += commaf(" · %d of %d calls priced", sp.CostedCalls, sp.Calls)
		}
		if sp.Mixed {
			note += " · mixed currencies"
		}
		b.WriteString(ctxRow("session spend", ctxMoney(*sp.Session), note))
	} else {
		notReported("session spend", commaf(" (%d calls, none priced)", sp.Calls))
	}
	if sp.Session == nil && sp.Calls > 0 {
		b.WriteString("\n    " + stDim.Render(noCostHint(sp)))
	}
	if sp.LastTurn != nil {
		note := curNote(*sp.LastTurn, sp.Currency)
		if sp.LastTurnCosted < sp.LastTurnCalls {
			note += commaf(" · %d of %d calls priced", sp.LastTurnCosted, sp.LastTurnCalls)
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
		b.WriteString(ctxRow("key spend", ctxMoney(k.Spend), curNote(k.Spend, k.Currency)+src))
		if k.Limit != nil {
			b.WriteString(ctxRow("key limit", ctxMoney(*k.Limit), curNote(*k.Limit, k.Currency)))
		} else {
			notReported("key limit", " (none set, or not stated)")
		}
		if k.Remaining != nil {
			note := curNote(*k.Remaining, k.Currency)
			if k.Limit != nil && *k.Limit > 0 {
				note += commaf(" (%.0f%%)", 100**k.Remaining / *k.Limit)
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
		notReported("key budget", " (read failed: "+clip(sp.KeyError, 60)+")")
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
			note = commaf(" of %d", *r.Limit)
			if *r.Limit > 0 {
				note += commaf(" (%.0f%%)", 100*float64(*r.Remaining)/float64(*r.Limit))
			}
		}
		if t, ok := parseRFC3339(r.ResetAt); ok {
			if t.Before(now) {
				note += " · reset " + until(t, now) + " (stale)"
			} else {
				note += " · resets " + until(t, now)
			}
		}
		b.WriteString(ctxRow(label, fmt.Sprintf("%9s", grouped(*r.Remaining)), note))
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
		return stNote.Render("⚠ "+text) + stDim.Render(" · ")
	}
	return stDim.Render("⚠ " + text + " · ")
}
