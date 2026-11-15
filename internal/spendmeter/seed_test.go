package spendmeter

import (
	"math"
	"testing"
	"time"

	"github.com/iodesystems/agentkit/llm"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// A second engine on the session seeds its money from the session's ledger
// lines, and it matches the ledger: the session, today, the last turn and
// the turn in flight; another session's lines are not its.
func TestSeed_SpendFromLedgerMatchesLedger(t *testing.T) {
	dir := t.TempDir()
	one := NewMeter()
	one.SetLedger(NewLedger(dir), "fp")
	one.SetSession("s1")
	note := one.NoteAs(Source{Provider: "p", Key: "fp", Model: "m1"})
	note(costed(0.10, "USD"))
	note(llm.Usage{}) // not priced: no line, but a call
	one.EndTurn()
	note(costed(0.20, "USD"))
	note(costed(0.05, "USD"))
	one.EndTurn()
	note(costed(0.01, "USD")) // in flight when the engine stopped

	other := NewMeter()
	other.SetLedger(NewLedger(dir), "fp")
	other.SetSession("s2")
	other.NoteAs(Source{Provider: "p", Key: "fp", Model: "m1"})(costed(9, "USD"))

	before := one.Snapshot()
	c := one.Counts()

	two := NewMeter() // the next engine, on another model
	l := NewLedger(dir)
	two.SetLedger(l, "fp")
	two.SetSession("s1")
	entries := l.SessionEntries("s1")
	if len(entries) != 4 {
		t.Fatalf("session lines = %d, want 4: %+v", len(entries), entries)
	}
	two.Seed(entries, &c)
	after := two.Snapshot()
	var sum float64
	for _, e := range entries {
		sum += e.Cost
	}
	if !near(after.Session, sum) || !near(after.Session, 0.36) || !near(after.Session, before.Session) {
		t.Errorf("session = %v, ledger %v, before %v", after.Session, sum, before.Session)
	}
	if !near(after.SessionToday, before.SessionToday) || !near(after.SessionInterval, before.SessionInterval) {
		t.Errorf("today %v/%v interval %v/%v", after.SessionToday, before.SessionToday, after.SessionInterval, before.SessionInterval)
	}
	if !near(after.LastTurn, 0.25) || after.LastTurnCosted != 2 || !after.TurnEnded {
		t.Errorf("last turn = %v (%d priced, ended %v)", after.LastTurn, after.LastTurnCosted, after.TurnEnded)
	}
	if after.Calls != 5 || after.CostedCalls != 4 || after.Currency != "USD" {
		t.Errorf("calls %d priced %d cur %q", after.Calls, after.CostedCalls, after.Currency)
	}
	// The turn in flight closes with its seeded part.
	two.NoteAs(Source{Provider: "p", Key: "fp", Model: "m2"})(costed(0.02, "USD"))
	two.EndTurn()
	if s := two.Snapshot(); !near(s.LastTurn, 0.03) || !near(s.Session, 0.38) {
		t.Errorf("after the turn: last %v session %v", s.LastTurn, s.Session)
	}
	// The new line carries the turn count the seed continued.
	if es := NewLedger(dir).SessionEntries("s1"); es[len(es)-1].Turn != 2 || es[len(es)-1].Model != "m2" {
		t.Errorf("new line = %+v", es[len(es)-1])
	}
}

// Without saved counts, every line's turn counts as ended and the newest is
// the last turn.
func TestSeed_WithoutCounts(t *testing.T) {
	m := NewMeter()
	now := time.Now()
	m.Seed([]Entry{{Time: now, Cost: 1, Currency: "USD", Turn: 0}, {Time: now, Cost: 2, Currency: "USD", Turn: 1}}, nil)
	s := m.Snapshot()
	if !near(s.Session, 3) || !near(s.LastTurn, 2) || s.Calls != 2 {
		t.Errorf("snapshot = %+v", s)
	}
}

// Lines from before the session field are no session's.
func TestSessionEntries_OldLinesAreNoSessions(t *testing.T) {
	dir := t.TempDir()
	l := NewLedger(dir)
	_ = l.Append(Entry{Provider: "p", Model: "m", Cost: 1, Currency: "USD"})
	if es := l.SessionEntries("s1"); len(es) != 0 {
		t.Errorf("old lines = %+v", es)
	}
	if es := l.SessionEntries(""); es != nil {
		t.Errorf("no id = %+v", es)
	}
}
