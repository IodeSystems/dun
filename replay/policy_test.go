package replay

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agent "github.com/iodesystems/agentkit/agent"
)

// The candidate-policy curves spec, fully in-tree: a synthetic recorded
// session run through two candidate shaping policies, asserting the per-round
// prompt size, the cost at ledger prices, and the prefix kept are reported for
// each. No real session logs, no model calls.
const policyTestModel = "accounts/fireworks/models/test-model"

// policyTestSession writes a synthetic four-round dun.jsonl stream whose
// history grows by a bounded amount each round. The first user entry is
// oversized (4000 chars) so the bounded policies restructure from round 1;
// every other entry is short.
func policyTestSession(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	var lines []string
	add := func(v any) { lines = append(lines, mustJSON(v)) }
	add(map[string]any{"type": "user", "id": "u1", "content": strings.Repeat("x", 4000)})
	for i := 1; i <= 4; i++ {
		if i > 1 {
			add(map[string]any{"type": "user", "id": fmt.Sprintf("u%d", i), "content": fmt.Sprintf("turn %d", i)})
		}
		add(map[string]any{"type": "request", "phase": "start"})
		add(map[string]any{"type": "tool_call", "call_id": fmt.Sprintf("c%d", i), "tool": "exec", "args": map[string]any{"cmd": fmt.Sprintf("echo %d", i)}})
		add(map[string]any{"type": "tool_result", "call_id": fmt.Sprintf("c%d", i), "tool": "exec", "result": fmt.Sprintf("ok %d", i)})
		add(map[string]any{"type": "request", "phase": "done", "prompt": 0, "cached": 0, "gen": 10})
		add(map[string]any{"type": "message", "role": "assistant", "id": fmt.Sprintf("a%d", i), "content": fmt.Sprintf("reply %d", i)})
	}
	path := filepath.Join(dir, "s-policy.dun.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// policyCurves runs the session through both candidate policies and returns
// the two report series.
func policyCurves(t *testing.T) (unbudgeted, bounded []RoundReport) {
	t.Helper()
	entries, rounds, err := Load(policyTestSession(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(rounds) != 4 {
		t.Fatalf("rounds = %d, want 4", len(rounds))
	}
	cfg := Config{
		System:    "policy curve test",
		Estimator: agent.CharsByThree{},
		Prices:    defaultPrices,
		Model:     policyTestModel,
	}
	cfg.Policy = agent.ShaperPolicy{} // BudgetTokens 0: unbudgeted
	unbudgeted = Run(context.Background(), entries, rounds, cfg)
	cfg.Policy = agent.ShaperPolicy{
		BudgetTokens:          1000,
		PreserveLastMessages:  2,
		PreserveLastToolCalls: 1,
		MaxToolResultChars:    agent.DefaultMaxToolResultChars,
	}
	bounded = Run(context.Background(), entries, rounds, cfg)
	return unbudgeted, bounded
}

// assertCurvesReported checks the spec's three clauses for one policy: the
// prompt size, the cost at ledger prices, and the prefix kept are reported
// per round, and are internally consistent.
func assertCurvesReported(t *testing.T, name string, reports []RoundReport) {
	t.Helper()
	if len(reports) != 4 {
		t.Fatalf("%s: reports = %d, want 4", name, len(reports))
	}
	var totalCost float64
	for i, r := range reports {
		if r.PromptTokens <= 0 {
			t.Errorf("%s round %d: PromptTokens = %d, want > 0", name, r.Round, r.PromptTokens)
		}
		if r.Cost <= 0 {
			t.Errorf("%s round %d: Cost = %v, want > 0 at ledger prices", name, r.Round, r.Cost)
		}
		if r.Model != policyTestModel {
			t.Errorf("%s round %d: Model = %q, want %q", name, r.Round, r.Model, policyTestModel)
		}
		if r.PrefixKept < 0 || r.PrefixKept > r.PromptTokens {
			t.Errorf("%s round %d: PrefixKept = %d out of range [0, %d]", name, r.Round, r.PrefixKept, r.PromptTokens)
		}
		// Round 1 has no previous round: the prefix kept is 0.
		if r.Round == 1 && r.PrefixKept != 0 {
			t.Errorf("%s round 1: PrefixKept = %d, want 0", name, r.PrefixKept)
		}
		totalCost += r.Cost
		_ = i
	}
	if totalCost <= 0 {
		t.Errorf("%s: total cost = %v, want > 0", name, totalCost)
	}
}

// TestPolicyCurves is the OR spec, in-tree: two candidate policies over the
// same synthetic session, each reporting per-round prompt size, cost at
// ledger prices, and prefix kept.
func TestPolicyCurves(t *testing.T) {
	unbudgeted, bounded := policyCurves(t)

	assertCurvesReported(t, "unbudgeted", unbudgeted)
	assertCurvesReported(t, "bounded", bounded)

	// The unbudgeted policy renders the whole conversation: the prompt size
	// grows exactly with the history, never shrinks, and nothing folds.
	for i, r := range unbudgeted {
		if r.Shrink != nil {
			t.Errorf("unbudgeted round %d: unexpected shrink %+v", r.Round, r.Shrink)
		}
		if i > 0 && r.PromptTokens < unbudgeted[i-1].PromptTokens {
			t.Errorf("unbudgeted round %d: prompt shrank without compaction: %d < %d",
				r.Round, r.PromptTokens, unbudgeted[i-1].PromptTokens)
		}
	}

	// The bounded policy restructures the oversized history: it folds, and
	// every round's prompt is smaller than the unbudgeted policy's.
	var sawShrink bool
	for i, r := range bounded {
		if r.Shrink != nil {
			sawShrink = true
			if r.Shrink.Subsumed <= 0 || r.Shrink.Summary == "" {
				t.Errorf("bounded round %d: shrink subsumed nothing: %+v", r.Round, r.Shrink)
			}
		}
		if r.PromptTokens >= unbudgeted[i].PromptTokens {
			t.Errorf("bounded round %d: PromptTokens = %d, want < %d (unbudgeted)",
				r.Round, r.PromptTokens, unbudgeted[i].PromptTokens)
		}
	}
	if !sawShrink {
		t.Error("bounded: no shrink event over 4 rounds, the budget never fired")
	}

	// The two curves are distinct candidates: they must diverge in at least
	// one round's prompt size, or the sweep would be measuring one policy
	// twice.
	differ := false
	for i := range unbudgeted {
		if unbudgeted[i].PromptTokens != bounded[i].PromptTokens {
			differ = true
			break
		}
	}
	if !differ {
		t.Error("the two candidate policies produced identical prompt-size curves")
	}
}

// TestPolicyCurves_CostMatchesLedgerPrices pins the cost clause against a
// hand-computed ledger price: the driver prices each round from the
// provider's reported counts (prompt, cached, gen), so the reported cost is
// the shim's estimate at defaultPrices for those counts, not an echo of the
// prompt size.
func TestPolicyCurves_CostMatchesLedgerPrices(t *testing.T) {
	entries, rounds, err := Load(policyTestSession(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Give the provider's counts real values: 1000 prompt, 200 cached, 10 gen
	// per round.
	for i := range rounds {
		rounds[i].PromptTokens = 1000
		rounds[i].CachedTokens = 200
		rounds[i].GenTokens = 10
	}
	reports := Run(context.Background(), entries, rounds, Config{
		System:    "policy curve test",
		Estimator: agent.CharsByThree{},
		Prices:    defaultPrices,
		Model:     policyTestModel,
	})
	if len(reports) != len(rounds) {
		t.Fatalf("reports = %d, want %d", len(reports), len(rounds))
	}
	// Fireworks is a flat-rate provider in the default table: 3.0 in, 0.3
	// read, 15 out per 1M. (1000-200)*3 + 200*0.3 + 10*15 = 2460/1e6 USD.
	want := (800*3.0 + 200*0.3 + 10*15.0) / 1e6
	for _, r := range reports {
		if r.Cost <= 0 {
			t.Fatalf("round %d: Cost = %v, want > 0", r.Round, r.Cost)
		}
		if r.Cost < want*0.99 || r.Cost > want*1.01 {
			t.Errorf("round %d: Cost = %v, want ~%v", r.Round, r.Cost, want)
		}
	}
}

// TestPolicyCurves_PrefixKeptShrinksWithRebuild checks the prefix-kept
// clause on the bounded policy: once a fold rewrites the history, the
// longest common message prefix against the previous round drops below the
// previous round's whole prompt.
func TestPolicyCurves_PrefixKeptShrinksWithRebuild(t *testing.T) {
	_, bounded := policyCurves(t)
	var sawDrop bool
	for i, r := range bounded {
		if i > 0 && bounded[i-1].PromptTokens > 0 && r.PrefixKept < bounded[i-1].PromptTokens {
			sawDrop = true
		}
	}
	if !sawDrop {
		t.Error("bounded: PrefixKept never dropped below the previous round's prompt size, no rebuild visible")
	}
}
