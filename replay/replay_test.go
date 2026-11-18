package replay

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	agent "github.com/iodesystems/agentkit/agent"
)

// TestComparePromptSize is the in-tree fixture for the 5% rule: the exported
// comparison the env-gated test uses, exercised here against synthetic pairs
// so the rule itself is tested without real sessions or a ledger.
func TestComparePromptSize(t *testing.T) {
	cases := []struct {
		name     string
		replayed []int
		ledger   []int
		wantOK   bool
		wantMax  float64
	}{
		{"exact", []int{1000, 2000}, []int{1000, 2000}, true, 0},
		{"within five percent", []int{1000, 2050}, []int{1000, 2000}, true, 0.025},
		{"at the boundary", []int{1050}, []int{1000}, true, 0.05},
		{"over five percent", []int{1051}, []int{1000}, false, 0.051},
		{"under by five percent", []int{950}, []int{1000}, true, 0.05},
		{"round mismatch", []int{1000, 2000}, []int{1000}, false, 0},
		{"no rounds", []int{}, []int{1000}, false, 0},
		{"no ledger", []int{1000}, []int{}, false, 0},
	}
	for _, c := range cases {
		ok, maxRel := ComparePromptSize(c.replayed, c.ledger, 0.05)
		if ok != c.wantOK {
			t.Errorf("%s: ok = %v, want %v", c.name, ok, c.wantOK)
		}
		if c.wantOK {
			if maxRel < c.wantMax {
				t.Errorf("%s: maxRel = %v, want at least %v", c.name, maxRel, c.wantMax)
			}
			if maxRel > 0.05 {
				t.Errorf("%s: maxRel = %v over the tolerance", c.name, maxRel)
			}
		}
	}
}

// TestSessionSplit is decided from the id alone: the same id always lands on
// the same side, and the fraction controls how often.
func TestSessionSplit(t *testing.T) {
	ids := make([]string, 1000)
	for i := range ids {
		ids[i] = strings.Repeat("s", 8) + strconv.Itoa(i)
	}
	sort.Strings(ids)
	var held int
	for _, id := range ids {
		got := SessionSplit(id, 0.5)
		if got != SessionSplit(id, 0.5) {
			t.Fatalf("split is not deterministic for %q", id)
		}
		if got {
			held++
		}
	}
	// 1000 ids at 0.5: the count must sit near half. A wide band is enough
	// to catch a constant or a hash that ignores its input.
	if held < 400 || held > 600 {
		t.Errorf("held %d of 1000 at fraction 0.5, want near 500", held)
	}
	if SessionSplit("anything", 0) {
		t.Error("fraction 0 puts nothing in held-out")
	}
	if !SessionSplit("anything", 1) {
		t.Error("fraction 1 puts everything in held-out")
	}
}



// TestParseExcludedSessionIDs reads the explicit keep-out list (the two long
// sessions the plan names are not derivable from the repo).
func TestParseExcludedSessionIDs(t *testing.T) {
	got := ParseExcludedSessionIDs(" a , b ,,c")
	if !got["a"] || !got["b"] || !got["c"] {
		t.Errorf("got %v", got)
	}
	if len(got) != 3 {
		t.Errorf("got %d entries: %v", len(got), got)
	}
	if len(ParseExcludedSessionIDs("")) != 0 {
		t.Error("empty list parses to nothing")
	}
}

// sessionID is the id a ledger line names: the basename of the session file,
// as the harness records it (roundstats.go: sessionName), which is also the
// file's stem.
func sessionID(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// TestLedgerFixtureReplay is the in-tree end-to-end for the reproduction
// path: a recorded event stream and a synthetic ledger it was written for,
// through Load → LoadRoundStats → RoundsFor → Run → ComparePromptSize. The
// ledger numbers are the driver's own measurements of this stream, so the
// pair matches exactly; the rule under test is the wiring, not the 5%
// boundary (that is TestComparePromptSize).
func TestLedgerFixtureReplay(t *testing.T) {
	dir := t.TempDir()

	// A short session: one user turn, one tool call, one result, one reply.
	var lines []string
	add := func(v any) { lines = append(lines, mustJSON(v)) }
	add(map[string]any{"type": "user", "id": "u1", "content": "count the letters in this line and read the file"})
	add(map[string]any{"type": "request", "phase": "start"})
	add(map[string]any{"type": "tool_call", "call_id": "c1", "tool": "exec", "args": map[string]any{"cmd": "wc -c fixture.txt"}})
	add(map[string]any{"type": "tool_result", "call_id": "c1", "tool": "exec", "result": "23 fixture.txt"})
	add(map[string]any{"type": "request", "phase": "done", "prompt": 0, "cached": 0, "gen": 10})
	add(map[string]any{"type": "message", "role": "assistant", "id": "a1", "content": "23 letters"})

	stream := filepath.Join(dir, "s-fixture.dun.jsonl")
	if err := os.WriteFile(stream, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	entries, rounds, err := Load(stream)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(rounds) != 1 {
		t.Fatalf("rounds = %d, want 1", len(rounds))
	}

	// Drive it the way the env-gated test does.
	reports := Run(context.Background(), entries, rounds, Config{
		Policy:   CurrentPolicy(),
		System:   "you are a test fixture",
		Estimator: agent.CharsByThree{},
	})
	if len(reports) != len(rounds) {
		t.Fatalf("reports = %d, want %d", len(reports), len(rounds))
	}
	replayed := make([]int, len(reports))
	for i, r := range reports {
		replayed[i] = r.PromptTokens
	}

	// A ledger the stream's own measurements would have produced: one main
	// round for the session, with the driver's prompt size.
	statsDir := filepath.Join(dir, "stats")
	if err := os.MkdirAll(statsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	ledger := []RoundStat{{Time: "2026-01-15T00:00:00Z", Session: sessionID(stream), Role: "main", Prompt: replayed[0]}}
	var ledgerLines []string
	for _, l := range ledger {
		ledgerLines = append(ledgerLines, mustJSON(l))
	}
	if err := os.WriteFile(filepath.Join(statsDir, "rounds-2026-01.jsonl"), []byte(strings.Join(ledgerLines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	all, err := LoadRoundStats(statsDir)
	if err != nil {
		t.Fatalf("LoadRoundStats: %v", err)
	}
	led := RoundsFor(all, sessionID(stream))
	if len(led) != 1 {
		t.Fatalf("ledger rounds = %d, want 1", len(led))
	}
	ledgerPrompts := make([]int, len(led))
	for i, l := range led {
		ledgerPrompts[i] = l.Prompt
	}

	ok, maxRel := ComparePromptSize(replayed, ledgerPrompts, 0.05)
	if !ok {
		t.Fatalf("fixture does not reproduce: max relative difference %v", maxRel)
	}
}

// TestLedgerReplay is the reproduction the OR spec asks for, env-gated like
// TestLearn_Replay: DUN_LEDGER_REPLAY is a glob of recorded dun.jsonl streams,
// and the stats dir (default $DUN_HOME/stats) holds the rounds-*.jsonl the
// sessions' machine wrote.
//
// The anti-overfitting rules of the plan: the held-out split is decided from
// the session id's hash BEFORE any of the session's numbers are read, and
// DUN_LEDGER_EXCLUDE is a comma-separated list of session ids kept out of
// tuning entirely (the two long DeepSeek-flash sessions; the list is explicit
// because the ledger is not in the repo and the ids cannot be derived here).
//
//	DUN_LEDGER_REPLAY='~/.dun/sessions/*.dun.jsonl' DUN_HOME=~/.dun \
//	  go test ./replay -run TestLedgerReplay -v
func TestLedgerReplay(t *testing.T) {
	glob := os.Getenv("DUN_LEDGER_REPLAY")
	if glob == "" {
		t.Skip("DUN_LEDGER_REPLAY is not set")
	}
	files, err := filepath.Glob(glob)
	if err != nil || len(files) == 0 {
		t.Fatalf("no files: %q %v", glob, err)
	}
	sort.Strings(files)

	statsDir := os.Getenv("DUN_LEDGER_STATS")
	if statsDir == "" {
		if home := os.Getenv("DUN_HOME"); home != "" {
			statsDir = filepath.Join(home, "stats")
		} else {
			t.Skip("DUN_HOME is not set and DUN_LEDGER_STATS is not set: no ledger to compare against")
		}
	}
	all, err := LoadRoundStats(statsDir)
	if err != nil {
		t.Fatalf("LoadRoundStats(%s): %v", statsDir, err)
	}
	if len(all) == 0 {
		t.Fatalf("no ledger lines in %s", statsDir)
	}

	// Decided before looking: the split fraction and the keep-out list are
	// read once, up front.
	fraction := ParseFraction(os.Getenv("DUN_LEDGER_HELD_OUT"), 0.5)
	excluded := ParseExcludedSessionIDs(os.Getenv("DUN_LEDGER_EXCLUDE"))

	checked, skippedNoLedger, excludedN := 0, 0, 0
	for _, f := range files {
		id := sessionID(f)
		if excluded[id] {
			t.Logf("skipped (excluded from tuning) %s", id)
			excludedN++
			continue
		}
		if !SessionSplit(id, fraction) {
			t.Logf("tuning side, not checked %s", id)
			continue
		}
		entries, rounds, err := Load(f)
		if err != nil {
			t.Logf("%s: %v", id, err)
			skippedNoLedger++
			continue
		}
		led := RoundsFor(all, id)
		if len(led) == 0 {
			t.Logf("%s: no ledger lines for the session, skipped", id)
			skippedNoLedger++
			continue
		}
		reports := Run(context.Background(), entries, rounds, Config{
			Policy:    CurrentPolicy(),
			System:    "you are a test fixture",
			Estimator: agent.CharsByThree{},
		})
		if len(reports) == 0 {
			t.Logf("%s: no rounds replayed, skipped", id)
			skippedNoLedger++
			continue
		}
		replayed := make([]int, len(reports))
		for i, r := range reports {
			replayed[i] = r.PromptTokens
		}
		ledgerPrompts := make([]int, len(led))
		for i, l := range led {
			ledgerPrompts[i] = l.Prompt
		}
		ok, maxRel := ComparePromptSize(replayed, ledgerPrompts, 0.05)
		checked++
		if ok {
			t.Logf("OK  %s: %d rounds, max relative difference %v", id, len(replayed), maxRel)
		} else {
			t.Errorf("MISMATCH %s: %d vs %d rounds, max relative difference %v (tolerance 0.05)",
				id, len(replayed), len(ledgerPrompts), maxRel)
		}
	}
	t.Logf("%d files: %d checked, %d excluded, %d skipped", len(files), checked, excludedN, skippedNoLedger)
	if checked == 0 {
		t.Fatal("no session was checked: every file was excluded, on the tuning side, or had no ledger lines")
	}
}

// CurrentPolicy is the shaping policy the replay uses to reproduce the
// ledger: the current policy, unbudgeted, so the Shaper renders the
// conversation whole and the replay's prompt size is the size the live build
// would have produced before any budget rung fired. The fields the root
// package's shaperPolicy sets (which is unexported) are mirrored here;
// TestShaperPolicy_CoversPolicyFor guards that mirror in internal/harness.
func CurrentPolicy() agent.ShaperPolicy {
	return agent.ShaperPolicy{
		BudgetTokens: 0,
		// Preserve the tail the live policy preserves, so the rendered
		// messages line up with what the Shaper would have rendered.
		PreserveLastMessages:  6,
		PreserveLastToolCalls: 4,
		MaxToolResultChars:    agent.DefaultMaxToolResultChars,
	}
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}
