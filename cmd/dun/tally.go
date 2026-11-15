package main

import (
	"encoding/json"
	"os"
	"strings"
	"sync"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/dun/internal/fsutil"
	"github.com/iodesystems/dun/internal/spendmeter"
	"github.com/iodesystems/dun/proto"
)

// The session's totals continue across engines (owner, 2026-11-14): an
// engine restart (a crash, an idle stop, a model switch) does not start the
// usage totals, the request figures or the spend at 0. An engine seeds them
// at start, before its first request, so its first usage and request events
// already carry the session's whole count and no client adds anything up
// (docs/session-events.md, "Usage", "Request stats").
//
// Sources:
//   - money: the spend ledger's lines for this session (spendmeter.Seed);
//   - token totals, request figures and the meter's other counts: the
//     session's stats file (<id>.stats.json, dun.StatsFile), rewritten after
//     every round and every turn. agentkit's tally counts this process
//     only; the file holds the sum the next engine starts from. It is kept
//     beside the session, not in it, because a fold or a recap removes the
//     rounds it summarizes from the conversation, and their per-entry usage
//     with them;
//   - fallback, a session from an engine before the stats file: the per-entry
//     usage in its history (Harness.RoundUsages). Exact for the tokens of the
//     rounds still in it; the averages only from rounds with the server's
//     timings (the client's ttft and stream times were never saved);
//   - fallback, neither: 0.

// tallyFile is the stats file.
type tallyFile struct {
	V       int                    `json:"v"`
	Usage   tallyUsage             `json:"usage"`
	Request proto.RequestFoldState `json:"request"`
	Meter   spendmeter.Counts      `json:"meter"`
}

// tallyUsage is agent.TokenUsage's cumulative part.
type tallyUsage struct {
	Total              int `json:"total,omitempty"`
	Cached             int `json:"cached,omitempty"`
	Processed          int `json:"processed,omitempty"`
	Generated          int `json:"generated,omitempty"`
	Turns              int `json:"turns,omitempty"`
	Reasoning          int `json:"reasoning,omitempty"`
	ReasoningReported  int `json:"reasoning_reported,omitempty"`
	ReasoningEstimated int `json:"reasoning_estimated,omitempty"`
}

func (t tallyUsage) token() agent.TokenUsage {
	return agent.TokenUsage{Total: t.Total, Cached: t.Cached, Processed: t.Processed, Generated: t.Generated,
		Turns: t.Turns, Reasoning: t.Reasoning, ReasoningReported: t.ReasoningReported,
		ReasoningEstimated: t.ReasoningEstimated}
}

func tallyOf(u agent.TokenUsage) tallyUsage {
	return tallyUsage{Total: u.Total, Cached: u.Cached, Processed: u.Processed, Generated: u.Generated,
		Turns: u.Turns, Reasoning: u.Reasoning, ReasoningReported: u.ReasoningReported,
		ReasoningEstimated: u.ReasoningEstimated}
}

// addUsage is a+b's cumulative counts, and b's Active (the window now).
func addUsage(a, b agent.TokenUsage) agent.TokenUsage {
	return agent.TokenUsage{Total: a.Total + b.Total, Active: b.Active, Cached: a.Cached + b.Cached,
		Processed: a.Processed + b.Processed, Generated: a.Generated + b.Generated, Turns: a.Turns + b.Turns,
		Reasoning: a.Reasoning + b.Reasoning, ReasoningReported: a.ReasoningReported + b.ReasoningReported,
		ReasoningEstimated: a.ReasoningEstimated + b.ReasoningEstimated}
}

// Seed sources, as seedTally reports them.
const (
	seedStats   = "stats"   // the stats file
	seedHistory = "history" // fallback: the history's per-entry usage
	seedNone    = "none"    // fallback: nothing saved; from 0
)

// sessionTally saves the stats file. One per engine (the root session's).
type sessionTally struct {
	mu    sync.Mutex
	path  string // "" = not saved
	reqs  *proto.RequestFold
	spend *spendmeter.Meter
}

var tally = sessionTally{reqs: &proto.RequestFold{}}

// statsPath is the stats file beside a session file (dun.StatsFile).
func statsPath(sessionFile string) string {
	if sessionFile == "" {
		return ""
	}
	return strings.TrimSuffix(sessionFile, ".jsonl") + ".stats.json"
}

// loadTally reads the stats file; ok=false when there is none (or it is
// unreadable).
func loadTally(path string) (tallyFile, bool) {
	var f tallyFile
	if path == "" {
		return f, false
	}
	b, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(b, &f) != nil || f.V != 1 {
		return tallyFile{}, false
	}
	return f, true
}

// seedTally seeds the usage base, the request fold and the meter for the
// session at path (its stats file), and says from what. ledger is the
// session's ledger lines; rounds its history's per-entry usage (the
// fallback). Called once, before the engine's first request.
func seedTally(t *sessionTally, u *usageTally, path string, ledger []spendmeter.Entry, rounds []agent.EntryUsage) string {
	t.mu.Lock()
	t.path = path
	t.mu.Unlock()
	if f, ok := loadTally(path); ok {
		u.seed(f.Usage.token())
		t.reqs.Seed(f.Request)
		t.spend.Seed(ledger, &f.Meter)
		return seedStats
	}
	if len(rounds) == 0 {
		// Fallback: nothing saved. The money is the ledger's still (none for
		// a session no engine with the stats file has run).
		t.spend.Seed(ledger, nil)
		return seedNone
	}
	// Fallback: an older engine's session, from its rounds' saved usage.
	var base agent.TokenUsage
	var c spendmeter.Counts
	for _, r := range rounds {
		base.Cached += r.Cached
		base.Processed += r.Processed
		base.Generated += r.Generated
		base.Turns++
		switch r.ReasoningSource {
		case agent.ReasoningReported:
			base.Reasoning += r.Reasoning
			base.ReasoningReported++
		case agent.ReasoningEstimated:
			base.Reasoning += r.Reasoning
			base.ReasoningEstimated++
		}
		req := &proto.Request{Phase: "done", Prompt: r.Cached + r.Processed, Cached: r.Cached,
			CacheReported: r.Cached > 0, Gen: r.Generated, Exact: r.Generated > 0}
		if r.PromptMS > 0 {
			req.PPS, req.PPSServer = float64(r.PromptN)/(r.PromptMS/1000), true
		}
		if r.PredictedMS > 0 {
			req.TGS, req.TGSServer = float64(r.PredictedN)/(r.PredictedMS/1000), true
		}
		t.reqs.Note(req)
		c.Calls++
		c.Prompt += r.Cached + r.Processed
		c.Cached += r.Cached
		c.Completion += r.Generated
		c.CacheSeen = c.CacheSeen || r.Cached > 0
	}
	base.Total = base.Cached + base.Processed + base.Generated
	u.seed(base)
	t.spend.Seed(ledger, &c)
	return seedHistory
}

// save rewrites the stats file with the session's totals now: u is the
// cumulative tally (seed included).
func (t *sessionTally) save(u agent.TokenUsage) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.path == "" {
		return
	}
	b, err := json.Marshal(tallyFile{V: 1, Usage: tallyOf(u), Request: t.reqs.State(), Meter: t.spend.Counts()})
	if err != nil {
		return
	}
	_ = fsutil.WriteAtomic(t.path, append(b, '\n'), 0o600)
}
