package dun

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/iodesystems/agentkit/llm"
)

// Money and limits — what the session has spent, what the key has left, and how
// close the rate limits are.
//
// agentkit reads what a gateway reports (llm.Usage.Cost / Limits / KeyBudget,
// llm.Client.KeyBudget) and calls llm.Client.OnUsage once per response. This is
// the session's total of it. cmd/dun points OnUsage at Note on the main client
// AND on every client its per-model cache builds, so the total covers the turn's
// rounds, the side calls that borrow the session's runner (suggest, rephrase,
// commit, rescue, the memory fork) and every sub-agent, whichever model it runs
// on. A call to a runner that is not an *llm.Client is not seen.
//
// "Not reported" is kept distinct from zero everywhere: a provider that says
// nothing about money must not read as free.

// SpendMeter totals reported cost and keeps the latest limits and key budget.
// Safe for concurrent use: Note runs on each response's reading goroutine.
type SpendMeter struct {
	mu sync.Mutex

	currency string // of the first reported cost; later ones in another are counted as mixed
	mixed    bool
	session  float64
	calls    int // responses seen
	costed   int // responses that reported a cost

	// Tokens over every response: the prompt, of it what the provider
	// reported as served from its cache (cacheSeen: some response reported
	// the field at all; a provider that does not must not read as 0% cached),
	// and the completion.
	prompt, cached, completion int
	cacheSeen                  bool

	hours map[int64]float64 // the session's cost by unix hour, in its currency

	turn       float64 // since the last EndTurn
	turnCalls  int
	turnCosted int
	last       turnSpend // the turn EndTurn closed

	limits   llm.RateLimits
	limitsAt time.Time

	budget      llm.KeyBudget
	budgetAt    time.Time
	unsupported bool   // the endpoint has no budget this client can read
	budgetErr   string // the last poll's failure, when it failed

	onRound func(SpendSource, llm.Usage) // SetRoundHook

	poke chan struct{}
	now  func() time.Time

	// The spend ledger every response with a cost is appended to (NoteAs),
	// and this session's key fingerprint, for its trailing-24-hour rate.
	ledger    *SpendLedger
	ledgerKey string
}

type turnSpend struct {
	amount        float64
	calls, costed int
	ended         bool
}

func NewSpendMeter() *SpendMeter {
	return &SpendMeter{poke: make(chan struct{}, 1), now: time.Now}
}

// Note folds one response's Usage in. It is the llm.Client.OnUsage hook.
func (m *SpendMeter) Note(u llm.Usage) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	m.calls++
	m.turnCalls++
	m.prompt += u.PromptTokens
	m.completion += u.CompletionTokens
	m.cached += u.CachedPromptTokens()
	if u.PromptTokensDetails != nil || u.CacheReadInputTokens > 0 || u.CacheCreationInputTokens > 0 {
		m.cacheSeen = true
	}
	if u.Cost.Reported {
		switch {
		case m.costed == 0:
			m.currency = u.Cost.Currency
		case u.Cost.Currency != m.currency:
			m.mixed = true
		}
		m.costed++
		m.turnCosted++
		m.session += u.Cost.Amount
		m.turn += u.Cost.Amount
		if m.hours == nil {
			m.hours = map[int64]float64{}
		}
		m.hours[now.Unix()/3600] += u.Cost.Amount
	}
	if u.Limits.Reported {
		m.limits, m.limitsAt = u.Limits, now
	}
	if b := u.KeyBudget; b.Reported {
		// The per-response figure is fresher than the last poll but carries no
		// reset; keep the poll's reset when it described the same key budget.
		if m.budget.Reported && m.budget.Currency == b.Currency {
			if b.ResetAt.IsZero() {
				b.ResetAt = m.budget.ResetAt
			}
			if b.Interval == "" {
				b.Interval = m.budget.Interval
			}
		}
		m.budget, m.budgetAt = b, now
		m.unsupported, m.budgetErr = false, ""
	}
}

// EndTurn closes the current turn's spend: everything since the previous
// EndTurn, which includes side calls made between the two turns.
func (m *SpendMeter) EndTurn() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.last = turnSpend{amount: m.turn, calls: m.turnCalls, costed: m.turnCosted, ended: true}
	m.turn, m.turnCalls, m.turnCosted = 0, 0, 0
	m.mu.Unlock()
	m.Poke()
}

// SetBudget records a poll's answer.
func (m *SpendMeter) SetBudget(b llm.KeyBudget, err error) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case errors.Is(err, llm.ErrBudgetUnsupported):
		m.unsupported, m.budgetErr = true, ""
	case err != nil:
		m.budgetErr = err.Error()
	default:
		m.budget, m.budgetAt = b, m.now()
		m.unsupported, m.budgetErr = false, ""
	}
}

// SpendSnapshot is the meter's state at one moment.
type SpendSnapshot struct {
	// Currency of Session and LastTurn; "" when no cost was reported or the
	// source named none. Mixed: some costs came in another currency and were
	// added anyway.
	Currency string
	Mixed    bool

	Session float64
	// The session's spend this hour, today (local) and over the interval
	// (IntervalStart on, counted from the start of that hour).
	SessionHour, SessionToday, SessionInterval float64
	// IntervalStart is when the interval began: the key budget's period
	// when the gateway states its reset and its length, else the last 7
	// days (USER 2026-10-28). IntervalLen names it ("30d", "7d");
	// IntervalOfKey: it is the key's period.
	IntervalStart time.Time
	IntervalLen   string
	IntervalOfKey bool

	Calls       int
	CostedCalls int // 0 → the session's spend is not reported

	// Prompt, Cached and Completion are the session's tokens over all calls;
	// CacheReported: the provider reported cached tokens at least once.
	Prompt, Cached, Completion int
	CacheReported              bool

	LastTurn       float64
	LastTurnCalls  int
	LastTurnCosted int
	TurnEnded      bool // false until the first EndTurn

	Budget            llm.KeyBudget
	BudgetAt          time.Time
	BudgetUnsupported bool
	BudgetErr         string

	Limits   llm.RateLimits
	LimitsAt time.Time

	// Ledger is the machine's spend over time (ledger.go); LedgerOn says a
	// ledger is wired at all.
	Ledger   LedgerSummary
	LedgerOn bool
}

func (m *SpendMeter) Snapshot() SpendSnapshot {
	if m == nil {
		return SpendSnapshot{}
	}
	m.mu.Lock()
	s := SpendSnapshot{
		Currency: m.currency, Mixed: m.mixed,
		Session: m.session, Calls: m.calls, CostedCalls: m.costed,
		Prompt: m.prompt, Cached: m.cached, Completion: m.completion, CacheReported: m.cacheSeen,
		LastTurn: m.last.amount, LastTurnCalls: m.last.calls, LastTurnCosted: m.last.costed, TurnEnded: m.last.ended,
		Budget: m.budget, BudgetAt: m.budgetAt, BudgetUnsupported: m.unsupported, BudgetErr: m.budgetErr,
		Limits: m.limits, LimitsAt: m.limitsAt,
	}
	now := m.now()
	s.IntervalStart, s.IntervalLen, s.IntervalOfKey = BudgetInterval(m.budget, now)
	y, mo, d := now.Date()
	today := time.Date(y, mo, d, 0, 0, 0, 0, now.Location()).Unix() / 3600
	for h, v := range m.hours {
		if h == now.Unix()/3600 {
			s.SessionHour += v
		}
		if h >= today {
			s.SessionToday += v
		}
		if h >= s.IntervalStart.Unix()/3600 {
			s.SessionInterval += v
		}
	}
	l, key := m.ledger, m.ledgerKey
	m.mu.Unlock()
	// The ledger is read off the meter's lock: it is file IO, and Note must
	// never wait for it.
	if l != nil {
		s.Ledger, s.LedgerOn = l.Summary(key, s.IntervalStart), true
	}
	return s
}

// defaultInterval is the interval when the key states no period of its own.
const defaultInterval = 7 * 24 * time.Hour

// BudgetInterval is when the current interval began: one period before the
// key budget's next reset, when the gateway states both the reset and the
// period's length; else the last 7 days. len names the period; ofKey says
// it is the key's.
func BudgetInterval(b llm.KeyBudget, now time.Time) (start time.Time, name string, ofKey bool) {
	if b.Reported && b.ResetAt.After(now) {
		if back, ok := intervalBack(b.Interval); ok {
			if st := back(b.ResetAt); !st.After(now) {
				return st, b.Interval, true
			}
		}
	}
	return now.Add(-defaultInterval), "7d", false
}

// intervalBack reads a gateway's period ("30d", "1mo", "12h", "monthly")
// as a step back from a time; ok=false for one it does not know.
func intervalBack(iv string) (func(time.Time) time.Time, bool) {
	iv = strings.ToLower(strings.TrimSpace(iv))
	switch iv {
	case "hourly":
		iv = "1h"
	case "daily":
		iv = "1d"
	case "weekly":
		iv = "1w"
	case "monthly":
		iv = "1mo"
	case "yearly", "annually":
		iv = "1y"
	}
	i := 0
	for i < len(iv) && iv[i] >= '0' && iv[i] <= '9' {
		i++
	}
	n, err := strconv.Atoi(iv[:i])
	if err != nil || n <= 0 {
		return nil, false
	}
	dur := func(d time.Duration) (func(time.Time) time.Time, bool) {
		return func(t time.Time) time.Time { return t.Add(-time.Duration(n) * d) }, true
	}
	switch iv[i:] {
	case "s":
		return dur(time.Second)
	case "m", "min":
		return dur(time.Minute) // LiteLLM's "30m" is minutes
	case "h":
		return dur(time.Hour)
	case "d":
		return dur(24 * time.Hour)
	case "w":
		return dur(7 * 24 * time.Hour)
	case "mo":
		return func(t time.Time) time.Time { return t.AddDate(0, -n, 0) }, true
	case "y":
		return func(t time.Time) time.Time { return t.AddDate(-n, 0, 0) }, true
	}
	return nil, false
}

// Poke asks the poller for a read soon (it still honours its minimum gap).
// Never blocks.
func (m *SpendMeter) Poke() {
	if m == nil {
		return
	}
	select {
	case m.poke <- struct{}{}:
	default:
	}
}

// BudgetSource is what PollBudget reads; *llm.Client is one.
type BudgetSource interface {
	KeyBudget(ctx context.Context) (llm.KeyBudget, error)
}

// Budget polling cadence: at start, every BudgetPollEvery, and after a turn
// when the last read is at least BudgetPollGap old.
const (
	BudgetPollEvery = 5 * time.Minute
	BudgetPollGap   = 30 * time.Second
)

// PollBudget reads the key budget now, then every `every`, and on Poke when the
// last read is older than `gap`, until ctx ends. It stops for good once the
// endpoint is known to have no budget. onChange runs after each read.
//
// Runs on its own goroutine, off the turn path: the read is a database query on
// the gateway and a turn must never wait for it.
func (m *SpendMeter) PollBudget(ctx context.Context, src BudgetSource, every, gap time.Duration, onChange func()) {
	if m == nil || src == nil {
		return
	}
	var last time.Time
	read := func() bool {
		last = m.now()
		b, err := src.KeyBudget(ctx)
		if ctx.Err() != nil {
			return false
		}
		m.SetBudget(b, err)
		if onChange != nil {
			onChange()
		}
		return !errors.Is(err, llm.ErrBudgetUnsupported)
	}
	if !read() {
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-m.poke:
			if m.now().Sub(last) < gap {
				continue
			}
		}
		if !read() {
			return
		}
	}
}

// Spend is the session's money and limits; zero-valued when no meter is wired.
func (h *Harness) Spend() SpendSnapshot { return h.cfg.Spend.Snapshot() }

// EndTurnSpend closes the turn's spend (see SpendMeter.EndTurn).
func (h *Harness) EndTurnSpend() { h.cfg.Spend.EndTurn() }
