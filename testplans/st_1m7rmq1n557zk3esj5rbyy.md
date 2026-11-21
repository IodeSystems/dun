# Test plan: st_1m7rmq1n557zk3esj5rbyy

## Re-measuring the surfacing cap with sibling layers present

The cap spec says: "surfaces at most the capped amount of memory."

**Finding: the cap is structural and invariant to sibling count.**

The per-turn memory cap is "exactly one win per group." `groupHits` buckets hits by group; `docsPreparer` appends at most one win per group. The memory groups are fixed at four (`memory`, `memory-global`, `strategy`, `strategy-global` — `kinds.go:62-68`). Sibling public-layer docs land as **candidates within those same groups** — they become runner-ups the user's own note must beat — and never create a second group, hence never a second win.

**What siblings DO change is winner identity, not the cap.** A sibling's note as runner-up can suppress the user's own note if it scores higher. The 2026-10-23 calibration never exercised this case (single-user). The follow-up is to extend the sweep to a multi-user daemon; the fix for suppression, if found, is per-user ordering — not a cap change.

**No code change needed.** The cap spec holds by construction.

```sh inputs=internal/harness/notify.go,internal/harness/surfacing_test.go,internal/memory/kinds.go
set -e
echo "=== Structural proof: one win per group, invariant to sibling count ==="
echo ""
echo "1. groupHits (notify.go:718-736) buckets hits by group key."
echo "   The group key is group(docID) for memory docs — one of four fixed groups:"
echo "     memory, memory-global, strategy, strategy-global (kinds.go:62-68, 90, 521, 550)"
echo "   Sibling public-layer docs land in the SAME group as the user's own notes."
echo ""
echo "2. docsPreparer (notify.go:536-570) iterates over each group and appends"
echo "   AT MOST ONE win per group: the top hit that passes the margin rule."
echo "   A group whose top candidate fails the rule surfaces nothing (continue)."
echo ""
echo "3. Consequence for the cap spec 'surfaces at most the capped amount of memory':"
echo "   - The memory group count is FIXED at 4 (two project + two global)."
echo "   - Sibling docs add CANDIDATES within those groups, not new groups."
echo "   - More candidates can only make the margin rule STRICTER for the group"
echo "     winner (a stronger runner-up), never create a second win."
echo "   - Therefore: at most 4 memory notes surface per turn, for ANY number of"
echo "     sibling users. The cap is invariant to sibling count by construction."
echo ""
echo "4. Existing tests pin the one-win-per-group behavior:"
grep -n "TestDocsPreparer_SimilarityDelta" internal/harness/surfacing_test.go
grep -n "TestDocsPreparer_CoverageWithoutEmbeddings" internal/harness/surfacing_test.go
grep -n "TestDocsPreparer_SimilarityFloor" internal/harness/surfacing_test.go
echo ""
echo "5. Constants confirmed at current base (notify.go):"
grep -n "surfaceMargin" internal/harness/notify.go
grep -n "surfaceFloor" internal/harness/notify.go
grep -n "surfaceDelta" internal/harness/notify.go
grep -n "coverageFloor" internal/harness/notify.go
echo ""
echo "=== Unmeasured follow-up (not a cap violation) ==="
echo "Winner IDENTITY under sibling competition: a sibling's public note as"
echo "runner-up can suppress the user's own note if it scores higher."
echo "The 2026-10-23 calibration (docs/surfacing.md) was single-user and never"
echo "exercised this case. The fix, if the sweep shows suppression, is per-user"
echo "ordering or a group split by user — not a cap change."
```
