package dun

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iodesystems/agentkit/llm"
)

func costed(amount float64, cur string) llm.Usage {
	return llm.Usage{Cost: llm.Cost{Amount: amount, Currency: cur, Reported: true}}
}

func TestSpendMeterAccumulates(t *testing.T) {
	m := NewSpendMeter()
	m.Note(costed(0.25, llm.CurrencyUSD))
	m.Note(llm.Usage{}) // a response that reported no cost
	m.Note(costed(0.5, llm.CurrencyUSD))
	s := m.Snapshot()
	if s.Session != 0.75 || s.Calls != 3 || s.CostedCalls != 2 || s.Currency != llm.CurrencyUSD || s.Mixed {
		t.Fatalf("snapshot = %+v", s)
	}
	if s.TurnEnded {
		t.Error("no turn has ended")
	}
	m.EndTurn()
	m.Note(costed(1, llm.CurrencyUSD)) // the next turn's, not the last one's
	s = m.Snapshot()
	if !s.TurnEnded || s.LastTurn != 0.75 || s.LastTurnCalls != 3 || s.LastTurnCosted != 2 {
		t.Errorf("last turn = %v (%d/%d)", s.LastTurn, s.LastTurnCosted, s.LastTurnCalls)
	}
	if s.Session != 1.75 {
		t.Errorf("session = %v", s.Session)
	}
	m.Note(costed(1, llm.CurrencyCredits))
	if !m.Snapshot().Mixed {
		t.Error("a cost in another currency must mark the total mixed")
	}
}

// Nothing priced is not "spent 0".
func TestSpendMeterNothingReported(t *testing.T) {
	m := NewSpendMeter()
	m.Note(llm.Usage{PromptTokens: 10})
	m.EndTurn()
	s := m.Snapshot()
	if s.CostedCalls != 0 || s.LastTurnCosted != 0 || s.Budget.Reported || s.Limits.Reported {
		t.Errorf("snapshot = %+v", s)
	}
	var nilMeter *SpendMeter
	nilMeter.Note(costed(1, "USD")) // a harness with no meter
	nilMeter.EndTurn()
	if nilMeter.Snapshot().Calls != 0 {
		t.Error("nil meter")
	}
}

// The per-response key spend is fresher than the last poll, but only the poll
// knows the reset — the merge keeps both.
func TestSpendMeterBudgetMerge(t *testing.T) {
	m := NewSpendMeter()
	reset := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	m.SetBudget(llm.KeyBudget{Spend: 10, Limit: 50, HasLimit: true, Remaining: 40, HasRemaining: true,
		ResetAt: reset, Interval: "30d", Currency: "USD", Source: llm.BudgetSourceLiteLLM, Reported: true}, nil)
	m.Note(llm.Usage{KeyBudget: llm.KeyBudget{Spend: 12, Limit: 50, HasLimit: true, Remaining: 38, HasRemaining: true,
		Currency: "USD", Source: llm.BudgetSourceLiteLLMHeaders, Reported: true}})
	b := m.Snapshot().Budget
	if b.Spend != 12 || b.Remaining != 38 || !b.ResetAt.Equal(reset) || b.Interval != "30d" {
		t.Errorf("budget = %+v", b)
	}
	m.SetBudget(llm.KeyBudget{}, errors.New("timeout"))
	s := m.Snapshot()
	if s.BudgetErr != "timeout" || s.Budget.Spend != 12 {
		t.Errorf("a failed read keeps the last good budget: %+v", s)
	}
	m.SetBudget(llm.KeyBudget{}, llm.ErrBudgetUnsupported)
	if !m.Snapshot().BudgetUnsupported {
		t.Error("unsupported not recorded")
	}
}

type fakeBudget struct {
	calls atomic.Int32
	err   error
}

func (f *fakeBudget) KeyBudget(context.Context) (llm.KeyBudget, error) {
	f.calls.Add(1)
	if f.err != nil {
		return llm.KeyBudget{}, f.err
	}
	return llm.KeyBudget{Spend: float64(f.calls.Load()), Reported: true}, nil
}

func TestPollBudgetStopsWhenUnsupported(t *testing.T) {
	m := NewSpendMeter()
	src := &fakeBudget{err: llm.ErrBudgetUnsupported}
	done := make(chan struct{})
	go func() { m.PollBudget(context.Background(), src, time.Millisecond, 0, nil); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("poller kept running against an endpoint with no budget")
	}
	if src.calls.Load() != 1 || !m.Snapshot().BudgetUnsupported {
		t.Errorf("calls = %d", src.calls.Load())
	}
}

// Reads at start, then on a poke once the gap has passed — a poke inside the
// gap is not a read.
func TestPollBudgetPokeHonoursGap(t *testing.T) {
	m := NewSpendMeter()
	src := &fakeBudget{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	changed := make(chan struct{}, 8)
	go m.PollBudget(ctx, src, time.Hour, time.Hour, func() { changed <- struct{}{} })
	<-changed
	m.Poke()
	select {
	case <-changed:
		t.Fatal("a poke inside the gap read again")
	case <-time.After(50 * time.Millisecond):
	}
	if src.calls.Load() != 1 {
		t.Errorf("calls = %d", src.calls.Load())
	}

	m2 := NewSpendMeter()
	src2 := &fakeBudget{}
	changed2 := make(chan struct{}, 8)
	go m2.PollBudget(ctx, src2, time.Hour, 0, func() { changed2 <- struct{}{} })
	<-changed2
	m2.EndTurn() // pokes
	select {
	case <-changed2:
	case <-time.After(2 * time.Second):
		t.Fatal("a turn's end did not trigger a read")
	}
	if got := m2.Snapshot().Budget.Spend; got != 2 {
		t.Errorf("budget after second read = %v", got)
	}
}

// End to end through agentkit: a LiteLLM-shaped response reaches the meter via
// OnUsage, with cost, key budget and limits.
func TestSpendMeterFromLiteLLMResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("x-litellm-version", "1.80.0")
		w.Header().Set("x-litellm-key-spend", "4.2")
		w.Header().Set("x-litellm-key-max-budget", "5")
		w.Header().Set("x-ratelimit-limit-requests", "100")
		w.Header().Set("x-ratelimit-remaining-requests", "99")
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":1,\"total_tokens\":4,\"cost\":0.031}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	m := NewSpendMeter()
	c := llm.NewClient(srv.URL, "k", "m")
	c.OnUsage = m.Note
	ch, err := c.ChatStream(context.Background(), []llm.Message{{Role: "user", Content: "x"}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}
	s := m.Snapshot()
	if s.Session != 0.031 || s.Currency != llm.CurrencyUSD {
		t.Errorf("session = %v %s", s.Session, s.Currency)
	}
	if s.Budget.Spend != 4.2 || s.Budget.Limit != 5 {
		t.Errorf("budget = %+v", s.Budget)
	}
	if s.Limits.Requests.Remaining != 99 {
		t.Errorf("limits = %+v", s.Limits)
	}
}

// The interval is the key budget's period when the gateway states its reset
// and length, else the last 7 days (USER 2026-10-28).
func TestBudgetInterval(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	reset := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		b     llm.KeyBudget
		start time.Time
		name  string
		ofKey bool
	}{
		{llm.KeyBudget{Reported: true, ResetAt: reset, Interval: "30d"}, reset.AddDate(0, 0, -30), "30d", true},
		{llm.KeyBudget{Reported: true, ResetAt: reset, Interval: "monthly"}, reset.AddDate(0, -1, 0), "monthly", true},
		{llm.KeyBudget{Reported: true, ResetAt: reset, Interval: "1mo"}, reset.AddDate(0, -1, 0), "1mo", true},
		{llm.KeyBudget{Reported: true, ResetAt: now.Add(time.Hour), Interval: "24h"}, now.Add(-23 * time.Hour), "24h", true},
		// No length, an unknown one, a past reset, or a period that would
		// start after now: the default.
		{llm.KeyBudget{Reported: true, ResetAt: reset}, now.Add(-7 * 24 * time.Hour), "7d", false},
		{llm.KeyBudget{Reported: true, ResetAt: reset, Interval: "fortnightly"}, now.Add(-7 * 24 * time.Hour), "7d", false},
		{llm.KeyBudget{Reported: true, ResetAt: now.Add(-time.Hour), Interval: "1d"}, now.Add(-7 * 24 * time.Hour), "7d", false},
		{llm.KeyBudget{Reported: true, ResetAt: reset, Interval: "1h"}, now.Add(-7 * 24 * time.Hour), "7d", false},
		{llm.KeyBudget{}, now.Add(-7 * 24 * time.Hour), "7d", false},
	} {
		start, name, ofKey := BudgetInterval(c.b, now)
		if !start.Equal(c.start) || name != c.name || ofKey != c.ofKey {
			t.Errorf("%+v: %v %q %v, want %v %q %v", c.b, start, name, ofKey, c.start, c.name, c.ofKey)
		}
	}
}

// The session's hour, today and interval rollups, and the ledger's
// summed over the same interval.
func TestSpendMeterRollups(t *testing.T) {
	now := time.Date(2026, 10, 28, 15, 30, 0, 0, time.Local)
	m := NewSpendMeter()
	at := now
	m.now = func() time.Time { return at }
	m.SetBudget(llm.KeyBudget{Reported: true, ResetAt: now.Add(48 * time.Hour), Interval: "3d"}, nil)
	for _, c := range []struct {
		ago  time.Duration
		cost float64
	}{{10 * time.Minute, 0.25}, {3 * time.Hour, 0.5}, {20 * time.Hour, 1}, {30 * time.Hour, 2}} {
		at = now.Add(-c.ago)
		m.Note(costed(c.cost, llm.CurrencyUSD))
	}
	at = now
	s := m.Snapshot()
	if s.SessionHour != 0.25 || s.SessionToday != 0.75 || s.SessionInterval != 1.75 || s.Session != 3.75 {
		t.Errorf("hour %v today %v interval %v total %v", s.SessionHour, s.SessionToday, s.SessionInterval, s.Session)
	}
	if !s.IntervalOfKey || s.IntervalLen != "3d" || !s.IntervalStart.Equal(now.Add(-24*time.Hour)) {
		t.Errorf("interval %v %q %v", s.IntervalStart, s.IntervalLen, s.IntervalOfKey)
	}
}
