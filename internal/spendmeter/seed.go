package spendmeter

// A session's spend continues across engines (owner, 2026-11-14: stats,
// usage totals and spend must not reset when the engine restarts or the
// model changes). An engine that starts on a session seeds its meter
// before its first request: the money from the session's ledger lines (the
// accurate money record, ledger.go), and what the meter counts that is not
// money (calls, tokens, turns ended) from the session's stats file, which
// cmd/dun keeps beside the session (cmd/dun/tally.go).

// Counts is what the meter counted that is not money.
type Counts struct {
	Calls      int  `json:"calls,omitempty"`
	Prompt     int  `json:"prompt,omitempty"`
	Cached     int  `json:"cached,omitempty"`
	Completion int  `json:"completion,omitempty"`
	CacheSeen  bool `json:"cache_seen,omitempty"`
	// Ended is how many turns have ended (EndTurn); TurnCalls the calls
	// since the last EndTurn, LastTurnCalls the calls of the turn it closed.
	Ended         int `json:"ended,omitempty"`
	TurnCalls     int `json:"turn_calls,omitempty"`
	LastTurnCalls int `json:"last_turn_calls,omitempty"`
}

// Counts is the meter's Counts now.
func (m *Meter) Counts() Counts {
	if m == nil {
		return Counts{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return Counts{Calls: m.calls, Prompt: m.prompt, Cached: m.cached, Completion: m.completion,
		CacheSeen: m.cacheSeen, Ended: m.ended, TurnCalls: m.turnCalls, LastTurnCalls: m.last.calls}
}

// SetSession names the session the ledger lines are written for.
func (m *Meter) SetSession(id string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.sessionID = id
	m.mu.Unlock()
}

// Seed sets the meter to what the session had before this process: the
// money from entries (the session's ledger lines, Ledger.SessionEntries)
// and the rest from c (nil: none was saved). Called once, before the first
// response. Without c (a stats file lost, or never written), every line's
// turn counts as ended and the newest one is the last turn; calls are at
// least the priced ones.
func (m *Meter) Seed(entries []Entry, c *Counts) {
	if m == nil {
		return
	}
	var cs Counts
	if c != nil {
		cs = *c
	} else {
		for _, e := range entries {
			cs.Ended = max(cs.Ended, e.Turn+1)
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range entries {
		switch {
		case m.costed == 0:
			m.currency = e.Currency
		case e.Currency != m.currency:
			m.mixed = true
		}
		m.costed++
		m.session += e.Cost
		if m.hours == nil {
			m.hours = map[int64]float64{}
		}
		m.hours[e.Time.Unix()/3600] += e.Cost
		switch {
		case e.Turn >= cs.Ended: // the turn in flight when the last engine stopped
			m.turn += e.Cost
			m.turnCosted++
		case e.Turn == cs.Ended-1:
			m.last.amount += e.Cost
			m.last.costed++
		}
	}
	m.calls = max(cs.Calls, m.costed)
	m.prompt, m.cached, m.completion, m.cacheSeen = cs.Prompt, cs.Cached, cs.Completion, cs.CacheSeen
	m.ended = cs.Ended
	m.turnCalls = max(cs.TurnCalls, m.turnCosted)
	m.last.calls = max(cs.LastTurnCalls, m.last.costed)
	m.last.ended = cs.Ended > 0
}
