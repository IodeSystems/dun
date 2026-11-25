package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/dun/internal/spendmeter"
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
	m := spendmeter.NewMeter()
	m.SetRoundHook(h.noteRound)
	main := m.NoteAs(spendmeter.Source{Provider: "p", Model: "m", Role: spendmeter.RoleMain})
	child := m.NoteAs(spendmeter.Source{Provider: "p", Model: "small", Role: spendmeter.RoleModel})

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
	if first.Session != "20261024-1" || first.Role != spendmeter.RoleMain || first.Prefix != 9000 || first.Schemas["vay"] != 6000 || first.Scripted["notes"] != 1000 {
		t.Errorf("first: %+v", first)
	}
	if !first.PrefixProcessed() || second.PrefixProcessed() {
		t.Errorf("prefix processed: first %v, second %v (cached %d of %d)", first.PrefixProcessed(), second.PrefixProcessed(), second.Cached, second.Prefix)
	}
	if third.Role != spendmeter.RoleModel || third.Prefix != 0 || third.Schemas != nil {
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
		{Time: now.Add(-30 * 24 * time.Hour), Role: spendmeter.RoleMain, Prefix: 100, Schemas: map[string]int{"a": 50}}, // too old
		{Time: now, Role: spendmeter.RoleMain, Prompt: 1000, Prefix: 100, Schemas: map[string]int{"a": 50}, Scripted: map[string]int{"s": 400},
			PromptN: 1000, PromptMS: 200, Costed: true, Cost: 0.2, Currency: "USD", Completion: 0},
		{Time: now, Role: spendmeter.RoleMain, Prompt: 1000, Cached: 900, Prefix: 100, Schemas: map[string]int{"a": 50}, Scripted: map[string]int{"s": 400}},
		{Time: now, Role: spendmeter.RoleModel, Prompt: 300},
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

// By model: every role counts, the window cuts, the busiest model is first.
func TestStatsLedger_ByModel(t *testing.T) {
	l := NewStatsLedger(t.TempDir())
	now := time.Now()
	for _, r := range []RoundStat{
		{Time: now.Add(-30 * 24 * time.Hour), Role: spendmeter.RoleMain, Model: "big", Prompt: 5000}, // too old
		{Time: now, Role: spendmeter.RoleMain, Model: "big", Prompt: 1000, Cached: 900, Completion: 50},
		{Time: now, Role: spendmeter.RoleMain, Model: "big", Prompt: 2000, Cached: 1000, CacheWrite: 10, Completion: 70},
		{Time: now, Role: spendmeter.RoleModel, Model: "small", Prompt: 300, Completion: 5},
		{Time: now, Role: spendmeter.RoleVision, Prompt: 40},
	} {
		if err := l.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	got := l.ByModel(now.Add(-7 * 24 * time.Hour))
	want := []ModelUsage{
		{Model: "big", Requests: 2, Prompt: 3000, Cached: 1900, CacheWrite: 10, Completion: 120},
		{Model: "small", Requests: 1, Prompt: 300, Completion: 5},
		{Model: "(unknown)", Requests: 1, Prompt: 40},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
	if (*StatsLedger)(nil).ByModel(now) != nil {
		t.Error("nil ledger should have no models")
	}
}

// A main round whose prompt dropped below the session's previous main round's
// is a shrink: one ledger line before the round that carries it, with the
// prompt before, the prompt after, and the prefix the next build keeps — the
// drop's cause is the build that preceded the round.
func TestRoundStats_ShrinkEvent(t *testing.T) {
	dir := t.TempDir()
	h := &Harness{cfg: Config{SessionFile: "/x/20261024-2.jsonl"}, cache: &cacheStats{}}
	h.window.Store(188160)
	h.rounds.ledger = NewStatsLedger(dir)
	m := spendmeter.NewMeter()
	m.SetRoundHook(h.noteRound)
	main := m.NoteAs(spendmeter.Source{Provider: "p", Model: "m", Role: spendmeter.RoleMain})

	h.BeginTurnStats()
	// A build before the fold sets prevSigs so noteBuild can detect the rewrite.
	h.cache.noteBuild([]llm.Message{{Role: "user", Content: "first message"}, {Role: "user", Content: "second message"}}, nil, time.Now())
	// Two rounds with no drop: no shrink line.
	main(llm.Usage{PromptTokens: 10000, CompletionTokens: 200})
	main(llm.Usage{PromptTokens: 10400, CompletionTokens: 50})
	// A fold rewrote the prompt; the round after it carries the drop.
	// The kept prefix is the first message (13 chars), so KeptChars > 0.
	h.cache.noteFold()
	h.cache.noteBuild([]llm.Message{{Role: "user", Content: "first message"}, {Role: "user", Content: "folded"}}, nil, time.Now())
	main(llm.Usage{PromptTokens: 6000, CompletionTokens: 40})
	h.EndTurnStats()

	b, err := os.ReadFile(filepath.Join(dir, "rounds-"+time.Now().UTC().Format("2006-01")+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 4 {
		t.Fatalf("ledger: %d lines\n%s", len(lines), b)
	}
	var ev ShrinkEvent
	if err := json.Unmarshal([]byte(lines[2]), &ev); err != nil {
		t.Fatalf("line 3: %v\n%s", err, lines[2])
	}
	if ev.Kind != "shrink" || ev.Before != 10400 || ev.After != 6000 || ev.Cause != "fold" ||
		ev.Window != 188160 || ev.Budget != shapingBudget(188160) || ev.Session != "20261024-2" || ev.Model != "m" {
		t.Errorf("shrink: %+v", ev)
	}
	// The kept prefix is the first message (13 chars), recorded as-is, and
	// the token estimate is at the meter's fallback ratio (4 chars/token):
	// 13/4 = 3.25, rounds to 3. Estimated is true because the meter is not
	// calibrated.
	if ev.KeptChars != 13 || ev.KeptTokens != 3 || !ev.Estimated {
		t.Errorf("kept prefix: %+v", ev)
	}
	// The round that carries the drop is the line after.
	var r RoundStat
	if err := json.Unmarshal([]byte(lines[3]), &r); err != nil {
		t.Fatal(err)
	}
	if r.Prompt != 6000 {
		t.Errorf("carrying round: %+v", r)
	}
}

// Grows are not shrinks; a sub-agent's drop is not the session's.
func TestRoundStats_NoShrinkWithoutDrop(t *testing.T) {
	dir := t.TempDir()
	h := &Harness{cfg: Config{SessionFile: "/x/20261024-3.jsonl"}, cache: &cacheStats{}}
	h.rounds.ledger = NewStatsLedger(dir)
	m := spendmeter.NewMeter()
	m.SetRoundHook(h.noteRound)
	main := m.NoteAs(spendmeter.Source{Provider: "p", Model: "m", Role: spendmeter.RoleMain})
	child := m.NoteAs(spendmeter.Source{Provider: "p", Model: "s", Role: spendmeter.RoleModel})

	h.BeginTurnStats()
	main(llm.Usage{PromptTokens: 10000, CompletionTokens: 200})
	child(llm.Usage{PromptTokens: 8000, CompletionTokens: 100}) // a child's prompt is its own
	main(llm.Usage{PromptTokens: 11000, CompletionTokens: 30})  // a grow
	h.EndTurnStats()

	b, err := os.ReadFile(filepath.Join(dir, "rounds-"+time.Now().UTC().Format("2006-01")+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(b), `"kind":"shrink"`); got != 0 {
		t.Errorf("%d shrink lines:\n%s", got, b)
	}
}

// reactionLabel classifies a user message that follows a finished turn.
func TestReactionLabel(t *testing.T) {
	cases := []struct {
		text string
		want string
	}{
		{"No, that's wrong", "correction"},
		{"not quite", "correction"},
		{"nope", "correction"},
		{"Wrong, it should be X", "correction"},
		{"That's not what I asked", "correction"},
		{"It broke the build", "correction"},
		{"Still fails on CI", "correction"},
		{"Looks good, merge it", "acceptance"},
		{"thanks, that works", "acceptance"},
		{"ok", "acceptance"},
		{"", "acceptance"}, // empty is the default
	}
	for _, c := range cases {
		if got := reactionLabel(c.text); got != c.want {
			t.Errorf("reactionLabel(%q) = %q, want %q", c.text, got, c.want)
		}
	}
}

// A finished turn stashes its outcome (commits, test verdict, files) and the
// next person turn writes it to the rounds ledger with the reaction.
func TestRoundStats_TurnOutcome(t *testing.T) {
	dir := t.TempDir()
	h := &Harness{cfg: Config{SessionFile: "/x/20261024-4.jsonl"}}
	h.rounds.ledger = NewStatsLedger(dir)

	// Stash: a turn that made 2 commits, touched 3 files, last test passed.
	f := LedgerFacts{
		Commits:  []LedgerCommit{{Hash: "abcdef1234567890", Subject: "fix: thing"}, {Hash: "0987654321abcdef", Subject: "test: add"}},
		NCommits: 2, NFiles: 3,
		Cmds:     []LedgerCmd{{Command: "make test", Verdict: "pass", Exit: 0}},
	}
	h.noteTurnOutcome(f, 1)

	// The next person message triggers the write, with a correction.
	h.appendTurnOutcome("No, that's not right — the test should check X")

	b, err := os.ReadFile(filepath.Join(dir, "rounds-"+time.Now().UTC().Format("2006-01")+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 1 {
		t.Fatalf("ledger: %d lines\n%s", len(lines), b)
	}
	var o TurnOutcome
	if err := json.Unmarshal([]byte(lines[0]), &o); err != nil {
		t.Fatal(err)
	}
	if o.Kind != "outcome" || o.Session != "20261024-4" || o.Turn != 1 {
		t.Errorf("header: %+v", o)
	}
	if len(o.Commits) != 2 || o.Commits[0] != "abcdef12" || o.Commits[1] != "09876543" {
		t.Errorf("commits: %v", o.Commits)
	}
	if o.NCommits != 2 || o.NFiles != 3 {
		t.Errorf("counts: n_commits=%d n_files=%d", o.NCommits, o.NFiles)
	}
	if o.TestVerdict != "pass" {
		t.Errorf("test verdict: %q", o.TestVerdict)
	}
	if o.Reaction != "correction" {
		t.Errorf("reaction: %q", o.Reaction)
	}
	if !strings.Contains(o.Message, "not right") {
		t.Errorf("message: %q", o.Message)
	}
}

// A non-person turn writes the outcome without a reaction; the pending slot
// is cleared so a later person turn does not re-write it.
func TestRoundStats_TurnOutcomeNoReaction(t *testing.T) {
	dir := t.TempDir()
	h := &Harness{cfg: Config{SessionFile: "/x/20261024-5.jsonl"}}
	h.rounds.ledger = NewStatsLedger(dir)

	f := LedgerFacts{
		Commits:  []LedgerCommit{{Hash: "aaaabbbbccccdddd", Subject: "feat: x"}},
		NCommits: 1, NFiles: 1,
		Cmds:     []LedgerCmd{{Command: "make test", Verdict: "2 failed", Failed: true, Exit: 1}},
	}
	h.noteTurnOutcome(f, 2)

	// A non-person turn: empty text, no reaction.
	h.appendTurnOutcome("")

	b, err := os.ReadFile(filepath.Join(dir, "rounds-"+time.Now().UTC().Format("2006-01")+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 1 {
		t.Fatalf("ledger: %d lines\n%s", len(lines), b)
	}
	var o TurnOutcome
	if err := json.Unmarshal([]byte(lines[0]), &o); err != nil {
		t.Fatal(err)
	}
	if o.Reaction != "" || o.Message != "" {
		t.Errorf("should have no reaction: %+v", o)
	}
	if o.TestVerdict != "fail (2 failed)" {
		t.Errorf("verdict: %q", o.TestVerdict)
	}
	// Pending is cleared: a second append writes nothing.
	h.appendTurnOutcome("thanks")
	b2, _ := os.ReadFile(filepath.Join(dir, "rounds-"+time.Now().UTC().Format("2006-01")+".jsonl"))
	if n := len(strings.Split(strings.TrimSpace(string(b2)), "\n")); n != 1 {
		t.Errorf("second append wrote a line: %d total", n)
	}
}

// Commits past the cap: the line carries the count, not the names.
func TestRoundStats_TurnOutcomeCommitCap(t *testing.T) {
	dir := t.TempDir()
	h := &Harness{cfg: Config{SessionFile: "/x/20261024-6.jsonl"}}
	h.rounds.ledger = NewStatsLedger(dir)

	// 15 commits; the line keeps 10 hashes and the count.
	commits := make([]LedgerCommit, 15)
	for i := range commits {
		commits[i] = LedgerCommit{Hash: "0123456789abcdef", Subject: "commit"}
	}
	f := LedgerFacts{Commits: commits, NCommits: 15, NFiles: 5}
	h.noteTurnOutcome(f, 3)
	h.appendTurnOutcome("ok")

	b, err := os.ReadFile(filepath.Join(dir, "rounds-"+time.Now().UTC().Format("2006-01")+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var o TurnOutcome
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(b))), &o); err != nil {
		t.Fatal(err)
	}
	if len(o.Commits) != TurnOutcomeCommits {
		t.Errorf("carries %d commits, want %d", len(o.Commits), TurnOutcomeCommits)
	}
	if o.NCommits != 15 {
		t.Errorf("n_commits=%d, want 15", o.NCommits)
	}
}

// The reaction message is capped at TurnOutcomeMessageCap.
func TestRoundStats_TurnOutcomeMessageCap(t *testing.T) {
	dir := t.TempDir()
	h := &Harness{cfg: Config{SessionFile: "/x/20261024-7.jsonl"}}
	h.rounds.ledger = NewStatsLedger(dir)

	h.noteTurnOutcome(LedgerFacts{NCommits: 0, NFiles: 0}, 1)
	long := strings.Repeat("a", TurnOutcomeMessageCap+100)
	h.appendTurnOutcome(long)

	b, err := os.ReadFile(filepath.Join(dir, "rounds-"+time.Now().UTC().Format("2006-01")+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var o TurnOutcome
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(b))), &o); err != nil {
		t.Fatal(err)
	}
	// ClipRune cuts at n runes and appends "…" (3 bytes), so len in bytes is
	// at most cap + 3.
	if len(o.Message) > TurnOutcomeMessageCap+3 {
		t.Errorf("message is %d bytes, cap+3 is %d", len(o.Message), TurnOutcomeMessageCap+3)
	}
}
