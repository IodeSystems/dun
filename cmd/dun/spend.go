package main

import (
	"time"

	"github.com/iodesystems/dun/internal/spendmeter"
	"github.com/iodesystems/dun/proto"
)

// spendProto is the wire shape of a meter snapshot.
func spendProto(s spendmeter.Snapshot, now time.Time) *proto.Spend {
	p := &proto.Spend{
		Currency: s.Currency, Mixed: s.Mixed,
		Calls: s.Calls, CostedCalls: s.CostedCalls,
		Prompt: s.Prompt, Cached: s.Cached, Completion: s.Completion, CacheReported: s.CacheReported,
		LastTurnCalls: s.LastTurnCalls, LastTurnCosted: s.LastTurnCosted,
		KeyUnsupported: s.BudgetUnsupported, KeyError: s.BudgetErr,
	}
	if s.CostedCalls > 0 {
		p.Session = ptr(s.Session)
		p.SessionHour, p.SessionToday, p.SessionInterval = ptr(s.SessionHour), ptr(s.SessionToday), ptr(s.SessionInterval)
	}
	if !s.IntervalStart.IsZero() {
		p.Interval, p.IntervalOfKey = s.IntervalLen, s.IntervalOfKey
		p.IntervalStart = s.IntervalStart.UTC().Format(time.RFC3339)
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
		p.Ledger = &proto.SpendLedger{Hour: l.Hour, Today: l.Today, Week: l.Week, Month: l.Month, Year: l.Year,
			Total: l.Total, Interval: l.Interval, ByModel: l.ByModel, Last24h: l.Last24h, Entries: l.Entries}
	}
	return p
}

func ptr[T any](v T) *T { return &v }
