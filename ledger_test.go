package dun

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iodesystems/agentkit/llm"
)

// Lines from several PROCESSES and goroutines at once land whole: every
// line parses, none is lost. The writers share nothing but the file.
func TestLedger_ConcurrentAppendsAreWhole(t *testing.T) {
	const perWriter = 300
	if dir := os.Getenv("DUN_LEDGER_CHILD"); dir != "" {
		l := NewSpendLedger(dir)
		for i := 0; i < perWriter; i++ {
			if err := l.Append(SpendEntry{Provider: "p", Key: "k", Model: strings.Repeat("m", 150) + fmt.Sprint(os.Getpid()), Cost: 0.01, Currency: "USD"}); err != nil {
				t.Fatal(err)
			}
		}
		return
	}
	dir := t.TempDir()
	const procs, routines = 4, 4
	var wg sync.WaitGroup
	errs := make(chan error, procs+routines)
	for i := 0; i < procs; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := exec.Command(os.Args[0], "-test.run=^TestLedger_ConcurrentAppendsAreWhole$", "-test.count=1")
			cmd.Env = append(os.Environ(), "DUN_LEDGER_CHILD="+dir)
			if out, err := cmd.CombinedOutput(); err != nil {
				errs <- fmt.Errorf("child: %v\n%s", err, out)
			}
		}()
	}
	for i := 0; i < routines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			l := NewSpendLedger(dir) // its own open per write, like another session
			for j := 0; j < perWriter; j++ {
				if err := l.Append(SpendEntry{Provider: "p", Model: fmt.Sprint("g", i), Cost: 0.01, Currency: "USD"}); err != nil {
					errs <- err
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "spend-*.jsonl"))
	n := 0
	for _, f := range files {
		fh, err := os.Open(f)
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(fh)
		for sc.Scan() {
			var e SpendEntry
			if err := json.Unmarshal(sc.Bytes(), &e); err != nil || e.Cost != 0.01 {
				t.Fatalf("torn line %q: %v", sc.Text(), err)
			}
			n++
		}
		fh.Close()
		if st, _ := os.Stat(f); st.Mode().Perm() != 0o600 {
			t.Errorf("%s is %v, want 0600", f, st.Mode().Perm())
		}
	}
	if want := (procs + routines) * perWriter; n != want {
		t.Errorf("lines %d, want %d", n, want)
	}
	l := NewSpendLedger(dir)
	if got := l.Summary("", time.Time{}).Today["USD"]; got < 23.99 || got > 24.01 {
		t.Errorf("today %v, want 24", got)
	}
}

// The periods around now, and a second read sees only what was appended
// since (by anyone), added to what it had.
func TestLedger_PeriodsAndIncrementalRead(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 23, 15, 0, 0, 0, time.Local) // a Friday
	l := NewSpendLedger(dir)
	l.now = func() time.Time { return now }
	add := func(at time.Time, model string, cost float64, cur string) {
		if err := l.Append(SpendEntry{Time: at, Provider: "work", Key: "fp1", Model: model, Cost: cost, Currency: cur}); err != nil {
			t.Fatal(err)
		}
	}
	add(now.Add(-time.Hour), "a", 0.50, "USD")                              // today
	add(now.AddDate(0, 0, -3), "b", 1.00, "USD")                            // Tuesday: this week
	add(now.AddDate(0, 0, -10), "a", 2.00, "USD")                           // this month
	add(time.Date(2026, 2, 1, 12, 0, 0, 0, time.Local), "a", 4.00, "USD")   // this year
	add(time.Date(2025, 12, 30, 12, 0, 0, 0, time.Local), "a", 8.00, "USD") // last year: in no period
	add(now.Add(-2*time.Hour), "c", 3, "credits")
	s := l.Summary("fp1", time.Time{})
	for name, got := range map[string]float64{"today": s.Today["USD"], "week": s.Week["USD"], "month": s.Month["USD"], "year": s.Year["USD"]} {
		want := map[string]float64{"today": 0.5, "week": 1.5, "month": 3.5, "year": 7.5}[name]
		if got != want {
			t.Errorf("%s %v, want %v", name, got, want)
		}
	}
	if s.Today["credits"] != 3 || s.ByModel["a"]["USD"] != 2.5 || s.ByModel["c"]["credits"] != 3 {
		t.Errorf("credits / by model: %+v %+v", s.Today, s.ByModel)
	}
	if s.Last24h["USD"] != 0.5 || s.Last24h["credits"] != 3 {
		t.Errorf("last 24h %+v", s.Last24h)
	}
	if !s.Entries {
		t.Error("entries")
	}
	// Another process appends; this ledger's next read adds it once.
	other := NewSpendLedger(dir)
	if err := other.Append(SpendEntry{Time: now.Add(-time.Minute), Provider: "work", Key: "fp2", Model: "a", Cost: 0.25, Currency: "USD"}); err != nil {
		t.Fatal(err)
	}
	s = l.Summary("fp1", time.Time{})
	if s.Today["USD"] != 0.75 || s.Last24h["USD"] != 0.5 {
		t.Errorf("after another writer: today %v (want 0.75), fp1's 24h %v (want 0.5)", s.Today["USD"], s.Last24h["USD"])
	}
	if s = l.Summary("fp1", time.Time{}); s.Today["USD"] != 0.75 {
		t.Errorf("a read twice counts twice: %v", s.Today["USD"])
	}
	// A partial last line (a write in flight) waits for its newline.
	f, _ := os.OpenFile(l.file(now), os.O_WRONLY|os.O_APPEND, 0)
	f.WriteString(`{"t":"2026-10-23T14:00:00Z","cost":1`)
	if s = l.Summary("", time.Time{}); s.Today["USD"] != 0.75 {
		t.Errorf("a partial line was read: %v", s.Today["USD"])
	}
	f.WriteString(`,"cur":"USD"}` + "\n")
	f.Close()
	if s = l.Summary("", time.Time{}); s.Today["USD"] != 1.75 {
		t.Errorf("the finished line was not read: %v", s.Today["USD"])
	}
}

// NoteAs writes a line only for a response that reported a cost, and the
// line has the key's fingerprint, never the key.
func TestLedger_NoteAsWritesCostedResponsesWithoutTheKey(t *testing.T) {
	dir := t.TempDir()
	m := NewSpendMeter()
	const key = "sk-live-secret-1234"
	m.SetLedger(NewSpendLedger(dir), KeyFingerprint(key))
	note := m.NoteAs(SpendSource{Provider: "work", Key: KeyFingerprint(key), Model: "chat"})
	note(llm.Usage{})                                                              // no cost: no line
	note(llm.Usage{Cost: llm.Cost{Amount: 0.02, Currency: "USD", Reported: true}}) // a line
	note(llm.Usage{Cost: llm.Cost{Amount: 0, Currency: "USD", Reported: true}})    // a real zero: a line
	files, _ := filepath.Glob(filepath.Join(dir, "*"))
	if len(files) != 1 {
		t.Fatalf("files %v", files)
	}
	b, _ := os.ReadFile(files[0])
	if strings.Count(string(b), "\n") != 2 {
		t.Errorf("want 2 lines:\n%s", b)
	}
	if strings.Contains(string(b), key) || strings.Contains(string(b), "secret") {
		t.Errorf("the key is in the ledger:\n%s", b)
	}
	if !strings.Contains(string(b), KeyFingerprint(key)) || len(KeyFingerprint(key)) != 16 {
		t.Errorf("no fingerprint:\n%s", b)
	}
	s := m.Snapshot()
	if !s.LedgerOn || s.Ledger.Today["USD"] != 0.02 || s.Calls != 3 || s.CostedCalls != 2 {
		t.Errorf("snapshot %+v", s)
	}
}

// This hour, all time (last year's line too) and an interval, over every key.
func TestLedger_HourTotalAndInterval(t *testing.T) {
	now := time.Date(2026, 10, 23, 15, 30, 0, 0, time.Local)
	l := NewSpendLedger(t.TempDir())
	l.now = func() time.Time { return now }
	for _, e := range []struct {
		at   time.Time
		key  string
		cost float64
	}{{now.Add(-10 * time.Minute), "fp1", 0.125}, {now.Add(-2 * time.Hour), "fp2", 0.5},
		{now.AddDate(0, 0, -3), "fp1", 1}, {time.Date(2025, 12, 30, 12, 0, 0, 0, time.Local), "fp1", 8}} {
		if err := l.Append(SpendEntry{Time: e.at, Provider: "work", Key: e.key, Model: "a", Cost: e.cost, Currency: "USD"}); err != nil {
			t.Fatal(err)
		}
	}
	s := l.Summary("fp1", now.AddDate(0, 0, -4))
	if s.Hour["USD"] != 0.125 || s.Total["USD"] != 9.625 || s.Interval["USD"] != 1.625 {
		t.Errorf("hour %v total %v interval %v", s.Hour, s.Total, s.Interval)
	}
	if s = l.Summary("fp1", time.Time{}); len(s.Interval) != 0 {
		t.Errorf("no interval asked, got %v", s.Interval)
	}
}

// The key's last budget is kept per key fingerprint, and a new meter on the
// same ledger starts with it, dated as read.
func TestSpendLedger_KeyBudgetCarriesToTheNextSession(t *testing.T) {
	dir := t.TempDir()
	at := time.Now().Add(-2 * time.Hour)
	m := NewSpendMeter()
	m.SetLedger(NewSpendLedger(dir), "fp1")
	m.now = func() time.Time { return at }
	m.SetBudget(llm.KeyBudget{Spend: 1, Remaining: 9, HasRemaining: true, Currency: "USD", Reported: true}, nil)

	next := NewSpendMeter()
	next.SetLedger(NewSpendLedger(dir), "fp1")
	s := next.Snapshot()
	if !s.Budget.Reported || s.Budget.Remaining != 9 || !s.BudgetAt.Equal(at.UTC()) {
		t.Errorf("budget %+v at %v", s.Budget, s.BudgetAt)
	}
	other := NewSpendMeter()
	other.SetLedger(NewSpendLedger(dir), "fp2")
	if other.Snapshot().Budget.Reported {
		t.Error("another key's budget was taken")
	}
}
