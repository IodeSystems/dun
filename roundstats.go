package dun

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/iodesystems/agentkit/llm"
)

// Round stats: what each model round cost in tokens, time and money, kept so
// a turn can say what it cost and so dun can show what a tool server costs a
// session (USER 2026-10-24: "justify how much a mcp server costs in ctx,
// cost, and time").
//
// Every response from every client the engine builds reaches the spend
// meter (NoteAs); the root session's collector gets each one there, so a
// turn's totals include its sub-agents and side calls, as the turn's spend
// does. Children share the parent's client, so a round cannot be told apart
// by who asked; the turn says how many sub-agents ran instead.
//
// Time comes from two places. LatencyMS is agentkit's client-side wall clock
// for the round. Timings is llama.cpp's own report (corrallm relays it):
// prompt_n tokens evaluated in prompt_ms — the prompt-processing speed this
// endpoint really has, which is what turns saved tokens into saved seconds.
// An endpoint that sends no timings gets no speed: dun does not guess one.
//
// Each round is also one line in $DUN_HOME/stats/rounds-YYYY-MM.jsonl, written
// the way the spend ledger is (ledger.go: O_APPEND, one write per line), with
// the request's prefix (system prompt and tool schemas) and each server's
// share of it, so a later reader can total what a server's schemas cost over
// the rounds that processed them rather than reused them from the cache.

// RoundStat is one model round; one stats-ledger line.
type RoundStat struct {
	Time     time.Time `json:"t"`
	Session  string    `json:"session,omitempty"`
	Role     string    `json:"role,omitempty"` // SpendSource.Role: main, model, vision
	Provider string    `json:"provider,omitempty"`
	Model    string    `json:"model,omitempty"`

	Prompt     int     `json:"prompt"`                // the provider's prompt tokens
	Cached     int     `json:"cached,omitempty"`      // of them, reused from its cache
	CacheWrite int     `json:"cache_write,omitempty"` // written to its cache (Anthropic bills these above the base rate)
	Completion int     `json:"completion"`            // generated, reasoning included
	LatencyMS  int64   `json:"latency_ms"`            // the round's wall clock, client side
	PromptN    int     `json:"prompt_n,omitempty"`    // llama.cpp: prompt tokens evaluated
	PromptMS   float64 `json:"prompt_ms,omitempty"`
	GenN       int     `json:"gen_n,omitempty"` // llama.cpp: tokens generated
	GenMS      float64 `json:"gen_ms,omitempty"`

	Cost     float64 `json:"cost,omitempty"`
	Currency string  `json:"cur,omitempty"`
	Costed   bool    `json:"costed,omitempty"` // the endpoint said what it cost

	// Prefix is the system prompt plus tool schemas in tokens, as dun last
	// measured them, and Schemas each MCP server's share ("mcp: <id>"
	// parts); Scripted is what each server hosted in the shell would have
	// added standalone. Main-session rounds only.
	Prefix   int            `json:"prefix,omitempty"`
	Schemas  map[string]int `json:"schemas,omitempty"`
	Scripted map[string]int `json:"scripted,omitempty"`
	// Recaps is how many recaps were applied since the session's previous
	// round: this round's prompt is after them, the previous one's before.
	// When a session first recaps, and at what size (USER 2026-10-27: keep
	// stats about the first recap). Main-session rounds only.
	Recaps int `json:"recaps,omitempty"`
}

// PrefixProcessed reports whether this round processed its prefix rather than
// reusing it: the cache covered less than the prefix. The prefix comes first
// in a request, so a cached count past it means the schemas were reused.
func (r RoundStat) PrefixProcessed() bool { return r.Prefix > 0 && r.Cached < r.Prefix }

// TurnStats is what one turn cost: every round between its start and its end.
type TurnStats struct {
	Rounds     int   `json:"rounds"`
	WallMS     int64 `json:"wall_ms"`  // the turn's own wall clock
	ModelMS    int64 `json:"model_ms"` // the rounds' wall clocks, summed
	Prompt     int   `json:"prompt"`
	Cached     int   `json:"cached"`
	CacheWrite int   `json:"cache_write,omitempty"`
	Completion int   `json:"completion"`

	// From llama.cpp's timings, when every round had them (Timed = Rounds):
	// what was evaluated and generated, and how long each took.
	Timed    int     `json:"timed,omitempty"`
	PromptN  int     `json:"prompt_n,omitempty"`
	PromptMS float64 `json:"prompt_ms,omitempty"`
	GenN     int     `json:"gen_n,omitempty"`
	GenMS    float64 `json:"gen_ms,omitempty"`

	Cost     float64 `json:"cost,omitempty"`
	Currency string  `json:"cur,omitempty"`
	Costed   int     `json:"costed,omitempty"` // rounds whose cost was reported
	Mixed    bool    `json:"mixed,omitempty"`  // costs in more than one currency

	SubAgents int `json:"sub_agents,omitempty"` // started during the turn
}

// roundStats is the root session's collector.
type roundStats struct {
	mu     sync.Mutex
	turn   TurnStats
	begun  time.Time
	agents int // the session's sub-agent count at the turn's start
	speeds []float64
	ledger *StatsLedger
}

// speedWindow is how many recent rounds the prefill speed is the median of,
// and speedMinTokens the smallest evaluation that counts: a few hundred
// tokens are dominated by per-request overhead and read as a slow server.
const (
	speedWindow    = 32
	speedMinTokens = 512
)

// noteRound folds one response in. It is the spend meter's round hook.
func (h *Harness) noteRound(src SpendSource, u llm.Usage) {
	r := RoundStat{Time: time.Now(), Session: h.sessionName(), Role: src.Role, Provider: src.Provider, Model: src.Model,
		Prompt: u.PromptTokens, Cached: u.CachedPromptTokens(), CacheWrite: u.CacheCreationInputTokens,
		Completion: u.CompletionTokens, LatencyMS: u.LatencyMS}
	if t := u.Timings; t != nil {
		r.PromptN, r.PromptMS, r.GenN, r.GenMS = t.PromptN, t.PromptMS, t.PredictedN, t.PredictedMS
	}
	if u.Cost.Reported {
		r.Cost, r.Currency, r.Costed = u.Cost.Amount, u.Cost.Currency, true
	}
	if src.Role == RoleMain {
		sp := h.SystemParts()
		r.Prefix = sp.Total
		for _, p := range sp.Parts {
			if id, ok := strings.CutPrefix(p.Name, "mcp: "); ok {
				if r.Schemas == nil {
					r.Schemas = map[string]int{}
				}
				r.Schemas[id] = p.Tokens
			}
		}
		r.Scripted = h.scriptedStandalone()
		r.Recaps = int(h.recapsSince.Swap(0))
	}
	rs := &h.rounds
	rs.mu.Lock()
	rs.add(r)
	l := rs.ledger
	rs.mu.Unlock()
	if l != nil {
		_ = l.Append(r)
	}
}

// add folds r into the open turn and the speed window. Under mu.
func (rs *roundStats) add(r RoundStat) {
	t := &rs.turn
	t.Rounds++
	t.ModelMS += r.LatencyMS
	t.Prompt += r.Prompt
	t.Cached += r.Cached
	t.CacheWrite += r.CacheWrite
	t.Completion += r.Completion
	if r.PromptMS > 0 || r.GenMS > 0 {
		t.Timed++
		t.PromptN += r.PromptN
		t.PromptMS += r.PromptMS
		t.GenN += r.GenN
		t.GenMS += r.GenMS
	}
	if r.Costed {
		switch {
		case t.Costed == 0:
			t.Currency = r.Currency
		case r.Currency != t.Currency:
			t.Mixed = true
		}
		t.Costed++
		t.Cost += r.Cost
	}
	if r.PromptN >= speedMinTokens && r.PromptMS > 0 {
		rs.speeds = append(rs.speeds, float64(r.PromptN)/(r.PromptMS/1000))
		if len(rs.speeds) > speedWindow {
			rs.speeds = rs.speeds[len(rs.speeds)-speedWindow:]
		}
	}
}

// BeginTurnStats opens a turn: what comes in from now is its.
func (h *Harness) BeginTurnStats() {
	n := h.agentCount()
	rs := &h.rounds
	rs.mu.Lock()
	rs.turn, rs.begun, rs.agents = TurnStats{}, time.Now(), n
	rs.mu.Unlock()
}

// EndTurnStats closes the turn and returns what it cost. A turn that was
// never begun (a side path) is measured from the previous end.
func (h *Harness) EndTurnStats() TurnStats {
	n := h.agentCount()
	rs := &h.rounds
	rs.mu.Lock()
	defer rs.mu.Unlock()
	t := rs.turn
	if !rs.begun.IsZero() {
		t.WallMS = time.Since(rs.begun).Milliseconds()
	}
	t.SubAgents = max(0, n-rs.agents)
	rs.turn, rs.begun, rs.agents = TurnStats{}, time.Now(), n
	return t
}

// PrefillSpeed is the endpoint's measured prompt-processing speed in tokens
// a second — the median of the last speedWindow rounds that evaluated at
// least speedMinTokens — and how many rounds it is from; 0, 0 when no round
// had llama.cpp's timings.
func (h *Harness) PrefillSpeed() (float64, int) {
	rs := &h.rounds
	rs.mu.Lock()
	s := slices.Clone(rs.speeds)
	rs.mu.Unlock()
	if len(s) == 0 {
		return 0, 0
	}
	slices.Sort(s)
	return s[len(s)/2], len(s)
}

// agentCount is how many sub-agents this session has started.
func (h *Harness) agentCount() int {
	h.agMu.Lock()
	defer h.agMu.Unlock()
	return h.agSeq
}

// sessionName is the session's id as its file names it, for a ledger line.
func (h *Harness) sessionName() string {
	if h.cfg.SessionFile == "" {
		return ""
	}
	return strings.TrimSuffix(filepath.Base(h.cfg.SessionFile), filepath.Ext(h.cfg.SessionFile))
}

// scriptedStandalone is each running hosted server's standalone schema cost:
// what it would add to this request were it not scripted.
func (h *Harness) scriptedStandalone() map[string]int {
	h.srvMu.Lock()
	defer h.srvMu.Unlock()
	var out map[string]int
	for id, hs := range h.hosted {
		if !hs.Connected || hs.SchemaBytes == 0 {
			continue
		}
		if out == nil {
			out = map[string]int{}
		}
		out[id] = h.bytesToTokens(hs.SchemaBytes)
	}
	return out
}

// StatsLedger appends round stats under dir ($DUN_HOME/stats), one file a
// month. Safe across processes the way SpendLedger is.
type StatsLedger struct {
	dir string
	mu  sync.Mutex
}

// statsLineMax bounds a line: a write under PIPE_BUF stays whole across
// processes. A line over it (hundreds of servers) is dropped, not torn.
const statsLineMax = 4000

// NewStatsLedger is the ledger in dir.
func NewStatsLedger(dir string) *StatsLedger { return &StatsLedger{dir: dir} }

// DefaultStatsDir is $DUN_HOME/stats.
func DefaultStatsDir() string { return filepath.Join(DunHome(), "stats") }

// Append writes r as one line.
func (l *StatsLedger) Append(r RoundStat) error {
	b, err := json.Marshal(r)
	if err != nil || len(b)+1 > statsLineMax {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := os.MkdirAll(l.dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(l.dir, "rounds-"+r.Time.UTC().Format("2006-01")+".jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

// ServerUsage is what one server's schemas cost over the ledger's rounds.
type ServerUsage struct {
	Rounds    int // main rounds whose request carried the server's schemas
	Processed int // of them, rounds that processed the prefix (not cached)
	Tokens    int // schema tokens in those processed rounds
	// For a scripted server: what it would have cost standalone over the
	// processed rounds — what scripting saved.
	SavedRounds int
	Saved       int
}

// StatsSummary is the ledger since a time: per server, and the totals a
// price per token is read from.
type StatsSummary struct {
	Since     time.Time
	Rounds    int // main rounds
	Processed int // main rounds that processed their prefix
	// Prompt and Cached are the main rounds' prompt tokens and, of them, the
	// ones reused from the cache: the conversation's hit rate.
	Prompt, Cached int
	Servers        map[string]*ServerUsage
	// Money: the cost of the rounds that reported one, over the tokens they
	// processed (new prompt tokens plus completion), for a price per token.
	Cost       float64
	Currency   string
	CostTokens int
	// Speed is the prefill speed over the window's rounds that had
	// llama.cpp's timings (median of the last speedWindow), as PrefillSpeed.
	Speed       float64
	SpeedRounds int
}

// Summary reads the ledger's main-session rounds since t (this month's file
// and the one before, which covers any window up to a month).
func (l *StatsLedger) Summary(since time.Time) (s StatsSummary) {
	s = StatsSummary{Since: since, Servers: map[string]*ServerUsage{}}
	if l == nil {
		return s
	}
	now := time.Now().UTC()
	// From the 1st: AddDate(0, -1, 0) on the 31st lands in THIS month
	// (Oct 31 → "Sep 31" → Oct 1), and the month was read twice.
	prev := time.Date(now.Year(), now.Month()-1, 1, 0, 0, 0, 0, time.UTC)
	months := []string{prev.Format("2006-01"), now.Format("2006-01")}
	var speeds []float64
	defer func() {
		if len(speeds) > speedWindow {
			speeds = speeds[len(speeds)-speedWindow:]
		}
		if len(speeds) > 0 {
			slices.Sort(speeds)
			s.Speed, s.SpeedRounds = speeds[len(speeds)/2], len(speeds)
		}
	}()
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, mo := range months {
		b, err := os.ReadFile(filepath.Join(l.dir, "rounds-"+mo+".jsonl"))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			var r RoundStat
			if line == "" || json.Unmarshal([]byte(line), &r) != nil || r.Time.Before(since) {
				continue
			}
			if r.PromptN >= speedMinTokens && r.PromptMS > 0 {
				speeds = append(speeds, float64(r.PromptN)/(r.PromptMS/1000))
			}
			if r.Costed && (s.Currency == "" || s.Currency == r.Currency) {
				s.Currency = r.Currency
				s.Cost += r.Cost
				s.CostTokens += r.Prompt - r.Cached + r.Completion
			}
			if r.Role != RoleMain {
				continue
			}
			s.Rounds++
			s.Prompt += r.Prompt
			s.Cached += r.Cached
			proc := r.PrefixProcessed()
			if proc {
				s.Processed++
			}
			use := func(id string) *ServerUsage {
				u := s.Servers[id]
				if u == nil {
					u = &ServerUsage{}
					s.Servers[id] = u
				}
				return u
			}
			for id, tok := range r.Schemas {
				u := use(id)
				u.Rounds++
				if proc {
					u.Processed++
					u.Tokens += tok
				}
			}
			for id, tok := range r.Scripted {
				u := use(id)
				if proc {
					u.SavedRounds++
					u.Saved += tok
				}
			}
		}
	}
	return s
}

// StatsSummary is the session's stats ledger since t; empty without one.
func (h *Harness) StatsSummary(since time.Time) StatsSummary {
	return h.rounds.ledger.Summary(since)
}
