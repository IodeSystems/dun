package harness

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/dun/internal/spendmeter"
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
	// Kind is empty on a round; a line with one is another record in the
	// file (an event, a shrink, an outcome) and readers of rounds skip it.
	Kind     string    `json:"kind,omitempty"`
	Time     time.Time `json:"t"`
	Session  string    `json:"session,omitempty"`
	Role     string    `json:"role,omitempty"` // spendmeter.Source.Role: main, model, vision
	Provider string    `json:"provider,omitempty"`
	Model    string    `json:"model,omitempty"`
	// Served is the model the response body named (llm.Usage.Model), when it
	// is not Model: an alias resolved, or a fallback.
	Served string `json:"served,omitempty"`
	// Finish and NativeFinish are the round's finish_reason and OpenRouter's
	// native_finish_reason, verbatim.
	Finish       string `json:"finish,omitempty"`
	NativeFinish string `json:"native_finish,omitempty"`
	// Trace is the id dun sent with the request (trace.go); ProviderID is
	// the response's request id header (llm.Usage.RequestID). Either is
	// empty when unknown.
	Trace      string `json:"trace,omitempty"`
	ProviderID string `json:"provider_id,omitempty"`

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
	// Repair is set when the round was a resend or a repair round: what
	// caused it (a RoundEvent's kind or cause: provider_fail, empty-reply,
	// length_cut, a retry's kind, …). What the repairs cost (repairs.go).
	Repair string `json:"repair,omitempty"`
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

// ShrinkEvent is one line in the rounds ledger: a main round whose prompt
// dropped below the session's previous main round's. kind is the build's
// rewrite cause when dun knows it (fold, boundary, rehead, recap, tools,
// other) — the round's BUILD was what shrank the prompt, and the cause
// cachestats attributes it to is the answer to "why did it drop". KeptChars
// is the leading prefix the build kept from the previous one, and KeptTokens
// the same at the meter's ratio (Estimated true: the ratio is the
// provider-measured one, the estimate is arithmetic on it). The line is a
// SIBLING of the round stat that carries the drop, written to the same file
// just before it, so a reader pairing lines by order sees the drop and its
// attribution together.
// TurnOutcome is one line in the rounds ledger: a finished turn's outcome —
// the commits it made and its last test verdict — and the user's reaction to
// it, at the next user message (correction or acceptance). Written when that
// message arrives: the reaction is unknowable before it, and a reader pairing
// lines by order sees the outcome and the reaction together. The line is a
// SIBLING of the round stats in the same file, the way a ShrinkEvent is.
type TurnOutcome struct {
	Kind    string    `json:"kind"` // "outcome"
	Time    time.Time `json:"t"`
	Session string    `json:"session,omitempty"`
	Turn    int       `json:"turn,omitempty"` // the ask turn the outcome is of
	// Commits are the turn's commit hashes, short; NCommits counts them past
	// the cap, and NFiles / TestVerdict the rest of the turn's outcome.
	Commits     []string `json:"commits,omitempty"`
	NCommits    int      `json:"n_commits,omitempty"`
	NFiles      int      `json:"n_files,omitempty"`
	TestVerdict string   `json:"test,omitempty"` // lastTestVerdict: "", "pass", "fail (…)"
	// Reaction is the next user message, labeled: "correction" (it says the
	// work was wrong) or "acceptance" (it moves on). Empty when the line was
	// written by a non-person turn (the reaction is left to the person's).
	Reaction string `json:"reaction,omitempty"`
	Message  string `json:"message,omitempty"` // its text, one line, capped
}

// TurnOutcomeCommits is how many commit hashes a TurnOutcome line carries;
// past it the line keeps the count (NCommits) and drops the names.
const TurnOutcomeCommits = 10

// TurnOutcomeMessageCap caps the reaction's message text in a line.
const TurnOutcomeMessageCap = 300

// reactionLabel labels a user message that follows a finished turn: a
// correction says the work was wrong (and names it), acceptance moves on.
// The label is mechanical — the user's words, not a model's read of them —
// so it is stable for a reader that counts corrections per turn.
func reactionLabel(text string) string {
	s := strings.ToLower(strings.TrimSpace(text))
	s = strings.TrimLeft(s, "!. \t-")
	for _, w := range []string{"no", "not ", "nope", "wrong", "that's not", "thats not", "didn't", "didnt", "doesn't", "doesnt", "isn't", "isnt", "wasn't", "wasnt", "it broke", "breaks", "fails", "failing", "still fails", "still broken"} {
		if strings.HasPrefix(s, w) {
			return "correction"
		}
	}
	return "acceptance"
}

type ShrinkEvent struct {
	Kind       string    `json:"kind"` // "shrink"
	Time       time.Time `json:"t"`
	Session    string    `json:"session,omitempty"`
	Model      string    `json:"model,omitempty"`
	Cause      string    `json:"cause,omitempty"` // the build's rewrite cause
	Before     int       `json:"before"`          // the previous main round's prompt tokens
	After      int       `json:"after"`           // this round's prompt tokens
	KeptChars  int       `json:"kept_chars,omitempty"`
	KeptTokens int       `json:"kept_tokens,omitempty"`
	Estimated  bool      `json:"estimated,omitempty"` // KeptTokens is at the meter's ratio
	Window     int       `json:"window,omitempty"`    // the context window in force
	Budget     int       `json:"budget,omitempty"`    // the shaping target in force
}

// roundStats is the root session's collector.
type roundStats struct {
	mu     sync.Mutex
	turn   TurnStats
	begun  time.Time
	agents int // the session's sub-agent count at the turn's start
	speeds []float64
	ledger *StatsLedger
	// nextRepair is the cause of the last resend or repair: the next round
	// is that repair's round (RoundStat.Repair). Under mu. Rounds are not
	// told apart by who asked, so a sub-agent's round can take it.
	nextRepair string
	// prevMainPrompt is each session's last main round's prompt tokens: the
	// baseline a shrink event's "before" is measured against. Under mu.
	prevMainPrompt map[string]int
}

// speedWindow is how many recent rounds the prefill speed is the median of,
// and speedMinTokens the smallest evaluation that counts: a few hundred
// tokens are dominated by per-request overhead and read as a slow server.
const (
	speedWindow    = 32
	speedMinTokens = 512
)

// noteRound folds one response in. It is the spend meter's round hook.
func (h *Harness) noteRound(src spendmeter.Source, u llm.Usage) {
	r := RoundStat{Time: time.Now(), Session: h.sessionName(), Role: src.Role, Provider: src.Provider, Model: src.Model,
		Prompt: u.PromptTokens, Cached: u.CachedPromptTokens(), CacheWrite: u.CacheCreationInputTokens,
		Completion: u.CompletionTokens, LatencyMS: u.LatencyMS,
		Finish: u.FinishReason, NativeFinish: u.NativeFinishReason, ProviderID: u.RequestID}
	if u.Model != "" && u.Model != src.Model {
		r.Served = u.Model
	}
	if t := u.Timings; t != nil {
		r.PromptN, r.PromptMS, r.GenN, r.GenMS = t.PromptN, t.PromptMS, t.PredictedN, t.PredictedMS
	}
	if u.Cost.Reported {
		r.Cost, r.Currency, r.Costed = u.Cost.Amount, u.Cost.Currency, true
	}
	var shrink *ShrinkEvent
	if src.Role == spendmeter.RoleMain {
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
		shrink = h.shrinkOf(r)
	}
	rs := &h.rounds
	rs.mu.Lock()
	r.Repair, rs.nextRepair = rs.nextRepair, ""
	rs.add(r)
	l := rs.ledger
	rs.mu.Unlock()
	if l == nil {
		return
	}
	// A drop in the prompt is one line before the round that carries it, so
	// a reader pairing lines by order sees the shrink and the round it
	// landed on together.
	if shrink != nil {
		_ = l.AppendShrink(*shrink)
	}
	// Written when the traced stream that carried it names it (trace.go).
	var hub *traceHub
	if h.trace != nil {
		hub = h.trace.hub
	}
	if !hub.park(roundKeyOf(u), r, func(r RoundStat) { _ = l.Append(r) }) {
		_ = l.Append(r)
	}
}

// shrinkOf says whether r is a main round whose prompt dropped below the
// session's previous main round's, and what to record about it. The
// previous round's prompt is the "before"; the build that preceded r is
// what r's prompt is, so its rewrite cause is the drop's cause and its
// kept prefix is what the next build keeps.
func (h *Harness) shrinkOf(r RoundStat) *ShrinkEvent {
	rs := &h.rounds
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.prevMainPrompt == nil {
		rs.prevMainPrompt = map[string]int{}
	}
	prev, seen := rs.prevMainPrompt[r.Session]
	if r.Prompt > 0 {
		rs.prevMainPrompt[r.Session] = r.Prompt
	}
	if !seen || r.Prompt >= prev {
		return nil
	}
	ev := &ShrinkEvent{
		Kind:    "shrink",
		Time:    r.Time,
		Session: r.Session,
		Model:   r.Model,
		Before:  prev,
		After:   r.Prompt,
		Window:  h.windowTokens(),
	}
	ev.Budget = shapingBudget(ev.Window)
	if b := h.cache.PeekBuild(); b.Cause != "" {
		ev.Cause = b.Cause
		ev.KeptChars = b.KeptChars
		if cpt := h.meter.CharsPerToken(); cpt > 0 {
			ev.KeptTokens = int(math.Round(float64(b.KeptChars) / cpt))
			ev.Estimated = !h.meter.Measured()
		}
	}
	return ev
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
// month. Safe across processes the way spendmeter.Ledger is.
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

// AppendShrink writes a shrink event as one line, the same file and the same
// append rule as Append: the readers glob rounds-*.jsonl and skip what does
// not parse as a round stat, and "kind":"shrink" is how a reader that DOES
// look for it picks the line out.
func (l *StatsLedger) AppendShrink(e ShrinkEvent) error {
	b, err := json.Marshal(e)
	if err != nil || len(b)+1 > statsLineMax {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := os.MkdirAll(l.dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(l.dir, "rounds-"+e.Time.UTC().Format("2006-01")+".jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

// RoundEvent is what happened to a round besides its usage: a repair, a
// resend, a retry, a fold, a cut, a cancel, a tool error, a command's time.
// The kinds and their fields are listed in docs/providers-and-cost.md
// ("Repairs in the stats ledger"). Finish and NativeFinish are the
// provider's words, verbatim.
type RoundEvent struct {
	Kind         string    `json:"kind"`
	Time         time.Time `json:"t"`
	Session      string    `json:"session,omitempty"`
	Provider     string    `json:"provider,omitempty"`
	Model        string    `json:"model,omitempty"`
	Finish       string    `json:"finish,omitempty"`
	NativeFinish string    `json:"native_finish,omitempty"`
	Attempt      int       `json:"attempt,omitempty"`
	Max          int       `json:"max,omitempty"`
	WaitMS       int64     `json:"wait_ms,omitempty"`
	Trace        string    `json:"trace,omitempty"`
	// Cause is what the event is of: a repair's notice tag, a retry's kind,
	// a fold's cause, who cancelled a turn, a tool error's class.
	Cause string `json:"cause,omitempty"`
	// Outcome: repaired|failed (repair_outcome), recovered|gave_up (a retry's
	// end), restarted|restart_failed|gave_up (server_restart).
	Outcome string `json:"outcome,omitempty"`
	Scope   string `json:"scope,omitempty"`  // retry: request|turn
	Status  int    `json:"status,omitempty"` // retry: the HTTP status
	Tool    string `json:"tool,omitempty"`   // tool_error
	Server  string `json:"server,omitempty"` // server_restart
	// cap_clamp: the field the client changed, what it sent and what was
	// asked; Reason its reason (also a fold_failed's or a cancel's text).
	Key    string `json:"key,omitempty"`
	Sent   string `json:"sent,omitempty"`
	Wanted string `json:"wanted,omitempty"`
	Reason string `json:"reason,omitempty"`
	// fold: the prompt's tokens before and after.
	Before int `json:"before,omitempty"`
	After  int `json:"after,omitempty"`
	// cmd_time: the command, how long it took, and where it was measured
	// (tui: keypress to the first frame that shows it; engine: handling).
	Cmd     string `json:"cmd,omitempty"`
	MS      int64  `json:"ms,omitempty"`
	Surface string `json:"surface,omitempty"`
}

// AppendEvent writes e as one line, the same file and append rule as Append.
func (l *StatsLedger) AppendEvent(e RoundEvent) error {
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	b, err := json.Marshal(e)
	if err != nil || len(b)+1 > statsLineMax {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := os.MkdirAll(l.dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(l.dir, "rounds-"+e.Time.UTC().Format("2006-01")+".jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

// noteRoundEvent files an event for the session's ledger; nil-safe. A
// sub-agent's go to its root's ledger, under its own session, provider and
// model: a repair a sub-agent's model needed is that model's repair.
func (h *Harness) noteRoundEvent(e RoundEvent) {
	root := h
	for root.parent != nil {
		root = root.parent
	}
	h.repairs.note(e)
	if c := repairRoundCause(e); c != "" {
		root.rounds.mu.Lock()
		root.rounds.nextRepair = c
		root.rounds.mu.Unlock()
	}
	l := root.rounds.ledger
	if l == nil {
		return
	}
	e.Session, e.Provider, e.Model = h.sessionName(), h.cfg.Provider, h.cfg.Endpoint.Model
	_ = l.AppendEvent(e)
}

// AppendOutcome writes a turn outcome as one line, the same file and the
// same append rule as AppendShrink: the readers glob rounds-*.jsonl and skip
// what does not parse as a round stat, and "kind":"outcome" is how a reader
// that DOES look for it picks the line out.
func (l *StatsLedger) AppendOutcome(o TurnOutcome) error {
	if o.Time.IsZero() {
		o.Time = time.Now()
	}
	b, err := json.Marshal(o)
	if err != nil || len(b)+1 > statsLineMax {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := os.MkdirAll(l.dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(l.dir, "rounds-"+o.Time.UTC().Format("2006-01")+".jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
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
			if line == "" || json.Unmarshal([]byte(line), &r) != nil || r.Kind != "" || r.Time.Before(since) {
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
			if r.Role != spendmeter.RoleMain {
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

// ModelUsage is one model's rounds in the stats ledger over a window, every
// role (main, other-model sub-agents and side calls, vision): the tokens a
// model handled, whoever asked. Prompt includes Cached, as in RoundStat.
type ModelUsage struct {
	Model                                  string
	Requests                               int
	Prompt, Cached, CacheWrite, Completion int
	// Closed is the plan items the model closed as done in the window
	// (closed-YYYY-MM.jsonl; items closed before it was kept are not in it).
	Closed int
}

// ByModel totals the ledger's rounds since t by model, the most prompt and
// completion tokens first. Read the way Summary reads: this month's file and
// the one before.
func (l *StatsLedger) ByModel(since time.Time) []ModelUsage {
	if l == nil {
		return nil
	}
	by := map[string]*ModelUsage{}
	l.each(since, func(r RoundStat) {
		name := r.Model
		if name == "" {
			name = "(unknown)"
		}
		u := by[name]
		if u == nil {
			u = &ModelUsage{Model: name}
			by[name] = u
		}
		u.Requests++
		u.Prompt += r.Prompt
		u.Cached += r.Cached
		u.CacheWrite += r.CacheWrite
		u.Completion += r.Completion
	})
	l.eachClosed(since, func(c closedRecord) {
		u := by[c.Model]
		if u == nil {
			u = &ModelUsage{Model: c.Model}
			by[c.Model] = u
		}
		u.Closed++
	})
	out := make([]ModelUsage, 0, len(by))
	for _, u := range by {
		out = append(out, *u)
	}
	slices.SortFunc(out, func(a, b ModelUsage) int {
		if x, y := a.Prompt+a.Completion, b.Prompt+b.Completion; x != y {
			return y - x
		}
		return strings.Compare(a.Model, b.Model)
	})
	return out
}

// closedRecord is one plan item a model closed as done: a line of
// closed-YYYY-MM.jsonl, beside the rounds file (a line in the rounds file
// would count as a round for every reader of it).
type closedRecord struct {
	Time    time.Time `json:"t"`
	Session string    `json:"session,omitempty"`
	Model   string    `json:"model"`
}

// AppendClosed records that model closed a plan item as done at t.
func (l *StatsLedger) AppendClosed(session, model string, t time.Time) error {
	if l == nil || model == "" {
		return nil
	}
	b, err := json.Marshal(closedRecord{Time: t, Session: session, Model: model})
	if err != nil || len(b)+1 > statsLineMax {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := os.MkdirAll(l.dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(l.dir, "closed-"+t.UTC().Format("2006-01")+".jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

// eachClosed is each for the close records.
func (l *StatsLedger) eachClosed(since time.Time, fn func(closedRecord)) {
	now := time.Now().UTC()
	prev := time.Date(now.Year(), now.Month()-1, 1, 0, 0, 0, 0, time.UTC)
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, mo := range []string{prev.Format("2006-01"), now.Format("2006-01")} {
		b, err := os.ReadFile(filepath.Join(l.dir, "closed-"+mo+".jsonl"))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			var c closedRecord
			if line == "" || json.Unmarshal([]byte(line), &c) != nil || c.Time.Before(since) || c.Model == "" {
				continue
			}
			fn(c)
		}
	}
}

// each calls fn on every round at or after since, in file order, from this
// month's file and the one before.
func (l *StatsLedger) each(since time.Time, fn func(RoundStat)) {
	now := time.Now().UTC()
	prev := time.Date(now.Year(), now.Month()-1, 1, 0, 0, 0, 0, time.UTC)
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, mo := range []string{prev.Format("2006-01"), now.Format("2006-01")} {
		b, err := os.ReadFile(filepath.Join(l.dir, "rounds-"+mo+".jsonl"))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			var r RoundStat
			if line == "" || json.Unmarshal([]byte(line), &r) != nil || r.Kind != "" || r.Time.Before(since) {
				continue
			}
			fn(r)
		}
	}
}

// ModelUsage is the session's stats ledger by model since t; nil without one.
func (h *Harness) ModelUsage(since time.Time) []ModelUsage {
	return h.rounds.ledger.ByModel(since)
}

// StatsSummary is the session's stats ledger since t; empty without one.
func (h *Harness) StatsSummary(since time.Time) StatsSummary {
	return h.rounds.ledger.Summary(since)
}
