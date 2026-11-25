# Test plan: st_1m873b27q7fjfa3t2gmpk7

## Block 1 — load verification: harness -count=20 -race under two concurrent full suites

The fix (pgidCh event-wait replacing the 3s/10ms wall-clock PGID poll, plus
load-tolerant cpu-0% assertion) is on base. This block runs the condition that
exposed the flake: internal/harness with -count=20 -race targeting the two
affected tests, while two full make test suites run concurrently as the
load source.

Before (from prior spike, pre-fix):
- Original failure: 2026-11-18/19 full-suite runs at load-avg ~81.
- Prior spike (old code): 50/50 clean at 3x make test load — the flake did
  not reproduce at achievable load on this box.

After (this block, post-fix):
- 20 runs of each target test under 2x make test concurrent load.
- Pass = fix holds at achievable load. Fail = flake still fires.

The block is red if any test fails; green means all 40 (20x2) passed.

```sh produces=summary.txt inputs=internal/harness/bgjob_test.go,internal/harness/jobs_test.go,internal/harness/exec.go
set -eu

LOG="$OUT/load"
mkdir -p "$LOG"

echo "== machine =="
nproc > "$LOG/ncpu"
uptime >> "$LOG/ncpu"

echo "load start: $(date)" >> "$LOG/ncpu"
uptime >> "$LOG/ncpu"
GOFLAGS=-count=1 make test > "$LOG/make1.log" 2>&1 &
M1=$!
GOFLAGS=-count=1 make test > "$LOG/make2.log" 2>&1 &
M2=$!
trap 'kill "$M1" "$M2" 2>/dev/null || true' EXIT

t0=$(date +%s)
RC=0
go test -race -count=20 ./internal/harness/ \
  -run 'TestJobs_ReplyCarriesProcessUsage|TestJobs_NoJobNamesTheRunningOnes' \
  -v > "$LOG/harness-count20.log" 2>&1 || RC=$?
t1=$(date +%s)

for i in $(seq 1 600); do
  if ! kill -0 "$M1" 2>/dev/null && ! kill -0 "$M2" 2>/dev/null; then
    break
  fi
  sleep 1
done
kill "$M1" "$M2" 2>/dev/null || true
wait "$M1" 2>/dev/null || true
wait "$M2" 2>/dev/null || true

uptime >> "$LOG/ncpu"

PASS=$(grep -c '^--- PASS' "$LOG/harness-count20.log" || true)
FAIL=$(grep -c '^--- FAIL' "$LOG/harness-count20.log" || true)
{
  echo "rc=$RC"
  echo "pass=$PASS"
  echo "fail=$FAIL"
  echo "duration=$((t1-t0))s"
  echo "load_start_uptime:"
  grep 'load-avg' "$LOG/ncpu" | head -1
  echo "end_uptime:"
  tail -1 "$LOG/ncpu"
  echo ""
  echo "deadline text if any:"
  grep -i 'never learned\|i/o timeout\|deadline' "$LOG/harness-count20.log" || echo "(none)"
} > "$OUT/summary.txt"

cat "$OUT/summary.txt"

if [ "$FAIL" -gt 0 ] || [ "$RC" -ne 0 ]; then
  echo "LOAD VERIFICATION FAILED: $FAIL test(s) failed, rc=$RC"
  exit 1
fi
echo "LOAD VERIFICATION PASSED: $PASS pass, rc=0"
```
