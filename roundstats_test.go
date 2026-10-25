package dun

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/agentkit/llm"
)

// Every response a meter sees reaches the root session's round stats with
// its source: a turn sums them, llama.cpp's timings give the prefill speed,
// and each round is one ledger line with the prefix and each server's share.
func TestRoundStats_TurnSpeedAndLedger(t *testing.T) {
	dir := t.TempDir()
	h := &Harness{cfg: Config{SessionFile: "/x/20261024-1.jsonl"}}
	h.rounds.ledger = NewStatsLedger(dir)
	h.systemParts = SystemBreakdown{Total: 9000, Parts: []ContextPart{{Name: "system", Tokens: 3000}, {Name: "mcp: vay", Tokens: 6000}}}
	h.hosted = map[string]hostedState{"notes": {Connected: true, SchemaBytes: 4000}}
	m := NewSpendMeter()
	m.SetRoundHook(h.noteRound)
	main := m.NoteAs(SpendSource{Provider: "p", Model: "m", Role: RoleMain})
	child := m.NoteAs(SpendSource{Provider: "p", Model: "small", Role: RoleModel})

	h.BeginTurnStats()
	main(llm.Usage{PromptTokens: 10000, CompletionTokens: 200, LatencyMS: 3000,
		Timings: &llm.Timings{PromptN: 10000, PromptMS: 2000, PredictedN: 200, PredictedMS: 900},
		Cost:    llm.Cost{Amount: 0.01, Currency: "USD", Reported: true}})
	main(llm.Usage{PromptTokens: 10400, CompletionTokens: 50, LatencyMS: 600,
		PromptTokensDetails: &llm.PromptTokensDetails{CachedTokens: 10000},
		Timings:             &llm.Timings{CacheN: 10000, PromptN: 400, PromptMS: 100, PredictedN: 50, PredictedMS: 400}})
	child(llm.Usage{PromptTokens: 800, CompletionTokens: 20, LatencyMS: 300})
	ts := h.EndTurnStats()

	if ts.Rounds != 3 || ts.Prompt != 21200 || ts.Cached != 10000 || ts.Completion != 270 || ts.ModelMS != 3900 {
		t.Errorf("turn: %+v", ts)
	}
	if ts.Timed != 2 || ts.PromptN != 10400 || ts.GenMS != 1300 || ts.Costed != 1 || ts.Cost != 0.01 || ts.Currency != "USD" {
		t.Errorf("timings/cost: %+v", ts)
	}
	// 400 evaluated tokens are under speedMinTokens: only the first round counts.
	if s, n := h.PrefillSpeed(); n != 1 || s != 5000 {
		t.Errorf("speed %v from %d rounds", s, n)
	}
	b, err := os.ReadFile(filepath.Join(dir, "rounds-"+time.Now().UTC().Format("2006-01")+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 3 {
		t.Fatalf("ledger: %d lines\n%s", len(lines), b)
	}
	var first, second, third RoundStat
	for i, p := range []*RoundStat{&first, &second, &third} {
		if err := json.Unmarshal([]byte(lines[i]), p); err != nil {
			t.Fatal(err)
		}
	}
	if first.Session != "20261024-1" || first.Role != RoleMain || first.Prefix != 9000 || first.Schemas["vay"] != 6000 || first.Scripted["notes"] != 1000 {
		t.Errorf("first: %+v", first)
	}
	if !first.PrefixProcessed() || second.PrefixProcessed() {
		t.Errorf("prefix processed: first %v, second %v (cached %d of %d)", first.PrefixProcessed(), second.PrefixProcessed(), second.Cached, second.Prefix)
	}
	if third.Role != RoleModel || third.Prefix != 0 || third.Schemas != nil {
		t.Errorf("a side model's round carries no prefix: %+v", third)
	}
	// The next turn starts empty.
	if ts := h.EndTurnStats(); ts.Rounds != 0 {
		t.Errorf("not reset: %+v", ts)
	}
}

// The ledger's summary: per server, the rounds that processed its schemas
// and what scripted servers would have cost; side models do not count.
func TestStatsLedger_Summary(t *testing.T) {
	l := NewStatsLedger(t.TempDir())
	now := time.Now()
	for _, r := range []RoundStat{
		{Time: now.Add(-30 * 24 * time.Hour), Role: RoleMain, Prefix: 100, Schemas: map[string]int{"a": 50}}, // too old
		{Time: now, Role: RoleMain, Prompt: 1000, Prefix: 100, Schemas: map[string]int{"a": 50}, Scripted: map[string]int{"s": 400},
			PromptN: 1000, PromptMS: 200, Costed: true, Cost: 0.2, Currency: "USD", Completion: 0},
		{Time: now, Role: RoleMain, Prompt: 1000, Cached: 900, Prefix: 100, Schemas: map[string]int{"a": 50}, Scripted: map[string]int{"s": 400}},
		{Time: now, Role: RoleModel, Prompt: 300},
	} {
		if err := l.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	s := l.Summary(now.Add(-7 * 24 * time.Hour))
	a, sc := s.Servers["a"], s.Servers["s"]
	if s.Rounds != 2 || s.Processed != 1 || a == nil || a.Rounds != 2 || a.Processed != 1 || a.Tokens != 50 {
		t.Errorf("summary %+v, a %+v", s, a)
	}
	if sc == nil || sc.SavedRounds != 1 || sc.Saved != 400 {
		t.Errorf("scripted %+v", sc)
	}
	if s.Speed != 5000 || s.SpeedRounds != 1 || s.Cost != 0.2 || s.CostTokens != 1000 {
		t.Errorf("speed/cost %+v", s)
	}
}
