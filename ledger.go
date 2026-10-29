package dun

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/iodesystems/agentkit/llm"
)

// The spend ledger: what every session and sub-agent on this machine spent,
// one line per response that reported a cost, under $DUN_HOME/spend/.
//
//	{"t":"2026-10-23T14:02:11Z","provider":"work","key":"9f86d081884c7d65","model":"chat-standard","cost":0.0042,"cur":"USD"}
//
// key is a FINGERPRINT of the API key (the first 8 bytes of its SHA-256, in
// hex) — never the key. It tells two keys on one provider apart, which is
// what a key budget is per; it cannot be turned back into the key.
//
// Safe across processes without a lock: every writer opens the file
// O_APPEND and writes one whole line in one write(2), and a line is held
// under ledgerLineMax (< PIPE_BUF, 4096), so on a local filesystem two
// writers' lines interleave whole, never torn. A reader that meets a partial
// last line (a write in flight) leaves it for the next read.
//
// Size: one file per month (spend-YYYY-MM.jsonl), so no file grows past a
// month of responses — about 150 bytes a line, so 10,000 responses a day
// is ~45 MB a month. Summary reads every month (the all-time total needs
// them), each file only past what it read last time. Old months are never
// deleted by dun; deleting one only takes it out of the totals.

const ledgerLineMax = 1024

// SpendEntry is one ledger line.
type SpendEntry struct {
	Time     time.Time `json:"t"`
	Provider string    `json:"provider"`
	Key      string    `json:"key,omitempty"`
	Model    string    `json:"model"`
	Cost     float64   `json:"cost"`
	Currency string    `json:"cur,omitempty"`
}

// KeyFingerprint is what the ledger stores for a key: never the key.
// "" for no key.
func KeyFingerprint(key string) string {
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:8])
}

// SpendLedger appends to and sums the ledger in dir.
type SpendLedger struct {
	dir string
	now func() time.Time

	mu    sync.Mutex
	read  map[string]int64                         // file → bytes consumed
	days  map[string]map[string]float64            // "2006-01-02" (local) → currency → amount
	model map[string]map[string]map[string]float64 // "2006-01" → model → currency → amount
	hours map[string]map[int64]map[string]float64  // key fingerprint → unix hour → currency → amount
	total map[string]float64                       // currency → amount, all time
}

// NewSpendLedger is the ledger in dir ($DUN_HOME/spend).
func NewSpendLedger(dir string) *SpendLedger {
	return &SpendLedger{dir: dir, now: time.Now, read: map[string]int64{},
		days: map[string]map[string]float64{}, model: map[string]map[string]map[string]float64{},
		hours: map[string]map[int64]map[string]float64{}, total: map[string]float64{}}
}

// DefaultLedgerDir is $DUN_HOME/spend.
func DefaultLedgerDir() string { return filepath.Join(DunHome(), "spend") }

func (l *SpendLedger) file(t time.Time) string {
	return filepath.Join(l.dir, "spend-"+t.UTC().Format("2006-01")+".jsonl")
}

// Append writes one entry as one line, in one write.
func (l *SpendLedger) Append(e SpendEntry) error {
	if l == nil {
		return nil
	}
	clipField := func(s string) string {
		if len(s) > 200 {
			return s[:200]
		}
		return s
	}
	e.Provider, e.Model, e.Currency = clipField(e.Provider), clipField(e.Model), clipField(e.Currency)
	if e.Time.IsZero() {
		e.Time = l.now()
	}
	e.Time = e.Time.UTC()
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if len(b) > ledgerLineMax {
		return nil // cannot happen with the fields clipped; never write a line that could tear
	}
	if err := os.MkdirAll(l.dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(l.file(e.Time), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(b)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return werr
}

// LedgerSummary is the ledger's totals as of one moment, by currency.
type LedgerSummary struct {
	// Hour is since the hour began (local); Total is all time.
	Hour, Today, Week, Month, Year, Total map[string]float64
	// Interval is every key's spend since the interval began (Summary's
	// since), counted from the start of that hour.
	Interval map[string]float64
	// ByModel is this month's spend by model.
	ByModel map[string]map[string]float64
	// Last24h is the trailing 24 hours' spend of the key asked about.
	Last24h map[string]float64
	Entries bool // the ledger has any line at all this year
}

// Summary scans what was appended since the last call (by any process) and
// sums the periods around now: this hour, today (since local midnight), this
// week (since Monday), this month, this year, all time, the interval since
// `since` (zero: none); key's trailing 24 hours.
func (l *SpendLedger) Summary(key string, since time.Time) LedgerSummary {
	if l == nil {
		return LedgerSummary{}
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.scan(now)
	loc := now.Location()
	y, mo, d := now.Date()
	today := time.Date(y, mo, d, 0, 0, 0, 0, loc)
	week := today.AddDate(0, 0, -((int(today.Weekday()) + 6) % 7))
	month := time.Date(y, mo, 1, 0, 0, 0, 0, loc)
	year := time.Date(y, 1, 1, 0, 0, 0, 0, loc)
	s := LedgerSummary{Hour: map[string]float64{}, Today: map[string]float64{}, Week: map[string]float64{},
		Month: map[string]float64{}, Year: map[string]float64{}, Total: map[string]float64{},
		Interval: map[string]float64{}, ByModel: map[string]map[string]float64{}, Last24h: map[string]float64{}}
	add := func(m map[string]float64, by map[string]float64) {
		for c, v := range by {
			m[c] += v
		}
	}
	for day, by := range l.days {
		t, err := time.ParseInLocation("2006-01-02", day, loc)
		if err != nil || t.After(today) {
			continue
		}
		if !t.Before(year) {
			s.Entries = true
			add(s.Year, by)
		}
		if !t.Before(month) {
			add(s.Month, by)
		}
		if !t.Before(week) {
			add(s.Week, by)
		}
		if !t.Before(today) {
			add(s.Today, by)
		}
	}
	for mname, by := range l.model[now.Format("2006-01")] {
		s.ByModel[mname] = map[string]float64{}
		add(s.ByModel[mname], by)
	}
	from := now.Add(-24*time.Hour).Unix() / 3600
	for h, by := range l.hours[key] {
		if h >= from {
			add(s.Last24h, by)
		}
	}
	add(s.Total, l.total)
	thisHour := now.Unix() / 3600
	for _, hh := range l.hours {
		for h, by := range hh {
			if h == thisHour {
				add(s.Hour, by)
			}
			if !since.IsZero() && h >= since.Unix()/3600 {
				add(s.Interval, by)
			}
		}
	}
	return s
}

// scan reads every month's file, each from where it stopped.
func (l *SpendLedger) scan(now time.Time) {
	files, _ := filepath.Glob(filepath.Join(l.dir, "spend-*.jsonl"))
	for _, f := range files {
		l.scanFile(f, now)
	}
}

func (l *SpendLedger) scanFile(path string, now time.Time) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	off := l.read[path]
	if st, err := f.Stat(); err != nil || st.Size() < off {
		return // shrank: someone edited it; leave what was read
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return
	}
	r := bufio.NewReader(f)
	loc := now.Location()
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return // EOF, or a line still being written: read it next time
		}
		off += int64(len(line))
		l.read[path] = off
		var e SpendEntry
		if json.Unmarshal([]byte(strings.TrimSpace(line)), &e) != nil || e.Time.IsZero() {
			continue
		}
		lt := e.Time.In(loc)
		l.total[e.Currency] += e.Cost
		bump(l.days, lt.Format("2006-01-02"), e.Currency, e.Cost)
		mm := l.model[lt.Format("2006-01")]
		if mm == nil {
			mm = map[string]map[string]float64{}
			l.model[lt.Format("2006-01")] = mm
		}
		bump(mm, e.Model, e.Currency, e.Cost)
		hh := l.hours[e.Key]
		if hh == nil {
			hh = map[int64]map[string]float64{}
			l.hours[e.Key] = hh
		}
		bump(hh, e.Time.Unix()/3600, e.Currency, e.Cost)
	}
}

func bump[K comparable](m map[K]map[string]float64, k K, cur string, v float64) {
	by := m[k]
	if by == nil {
		by = map[string]float64{}
		m[k] = by
	}
	by[cur] += v
}

// SpendSource is who a client's responses are billed to: the ledger's
// provider, key fingerprint and model; Role is which client it is (RoleMain,
// RoleModel, RoleVision), for the round stats (roundstats.go).
type SpendSource struct {
	Provider, Key, Model string
	Role                 string
}

// A client's role: the session's own model, a model a sub-agent or side
// call named (the per-model cache), or the vision fallback.
const (
	RoleMain   = "main"
	RoleModel  = "model"
	RoleVision = "vision"
)

// SetRoundHook gives every response, with its source, to f as well: the root
// session's round stats (Harness.noteRound). One hook; a later call replaces
// it.
func (m *SpendMeter) SetRoundHook(f func(SpendSource, llm.Usage)) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.onRound = f
	m.mu.Unlock()
}

// SetLedger points the meter at a ledger; key is the session's key
// fingerprint (for the trailing-24-hour rate against its budget).
func (m *SpendMeter) SetLedger(l *SpendLedger, key string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.ledger, m.ledgerKey = l, key
	m.mu.Unlock()
}

// NoteAs is the llm.Client.OnUsage hook for a client billed to src: Note,
// and a ledger line when the response reported a cost.
func (m *SpendMeter) NoteAs(src SpendSource) func(u llm.Usage) {
	return func(u llm.Usage) {
		m.Note(u)
		if m == nil {
			return
		}
		m.mu.Lock()
		hook := m.onRound
		m.mu.Unlock()
		if hook != nil {
			hook(src, u)
		}
		if !u.Cost.Reported {
			return
		}
		m.mu.Lock()
		l, now := m.ledger, m.now()
		m.mu.Unlock()
		_ = l.Append(SpendEntry{Time: now, Provider: src.Provider, Key: src.Key, Model: src.Model,
			Cost: u.Cost.Amount, Currency: u.Cost.Currency})
	}
}
