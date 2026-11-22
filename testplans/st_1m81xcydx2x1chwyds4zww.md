# Test plan: st_1m81xcydx2x1chwyds4zww

Confirm the testplan sh block is a real shell in the checkout (the load path the previous session reported as missing).

```sh
uname -a; echo "nproc=$(nproc)"; echo "loadavg: $(cat /proc/loadavg)"; go version
```

Reproduce the three harness flakes under load with the issue's exact command: `GOWORK=off go test -race -count=5 .` in `internal/harness`, with 40 busy-loop load generators in the background (the issue's load ~80). Full output is saved to `$OUT/load-run.log`. The block exits with the test's own code: if a flake fires the block fails and the output is the evidence for the fix tasks; if the round is clean the block passes.

```sh produces=load-run.log
set +e
cd /home/nthalk/local/src/iodesystems/dun.wt0/internal/harness

# --- start 40 busy-loop load generators (the issue's load ~80 on this box) ---
PIDS=""
for i in $(seq 1 40); do
  nohup sh -c 'while :; do :; done' >/dev/null 2>&1 &
  PIDS="$PIDS $!"
done
echo "baseline loadavg: $(cat /proc/loadavg)"
sleep 3
echo "after-load loadavg: $(cat /proc/loadavg)"

# --- run the issue's prescribed command, capturing FULL output to $OUT ---
mkdir -p "$OUT"
{
  echo "=== START $(date -u +%FT%TZ) ==="
  echo "loadavg at start: $(cat /proc/loadavg)"
  echo "cmd: GOWORK=off go test -race -count=5 ."
  echo "----"
} > "$OUT/load-run.log"
GOWORK=off go test -race -count=5 . >> "$OUT/load-run.log" 2>&1
RC=$?
{
  echo "----"
  echo "go test exit code: $RC"
  echo "loadavg at end: $(cat /proc/loadavg)"
  echo "=== END $(date -u +%FT%TZ) ==="
} >> "$OUT/load-run.log"

# --- kill the load generators ---
for p in $PIDS; do kill "$p" 2>/dev/null; done
echo "load killed. loadavg now: $(cat /proc/loadavg)"
echo "go test exit code: $RC"
echo "log: $(wc -l < "$OUT/load-run.log") lines, $(wc -c < "$OUT/load-run.log") bytes"
grep -E '^(--- FAIL|    --- FAIL|FAIL|ok  )' "$OUT/load-run.log" || true
exit $RC
```

Reproduce the three harness flakes under load, scoped to just those three tests with `-count=5` (the full package run exceeds the 6-min block bound under load; the three tests are the fast, timing-sensitive targets). 40 busy-loop load generators in the background. Full output saved to `$OUT/three.log`. The block exits with the test's own code: a fired flake (or a DATA RACE) fails the block and the saved log is the evidence; a clean round passes.

```sh produces=three.log
set +e
cd /home/nthalk/local/src/iodesystems/dun.wt0/internal/harness

PIDS=""
for i in $(seq 1 40); do
  nohup sh -c 'while :; do :; done' >/dev/null 2>&1 &
  PIDS="$PIDS $!"
done
echo "loadavg at start: $(cat /proc/loadavg)"
sleep 3
echo "loadavg settled: $(cat /proc/loadavg)"

mkdir -p "$OUT"
RUN="TestIndex_BudgetExcludesAndRetries|TestIndexAnyway_PersonOnly|TestJobs_AMessageEndsTheRun"
{
  echo "=== START $(date -u +%FT%TZ) ==="
  echo "loadavg at start: $(cat /proc/loadavg)"
  echo "cmd: GOWORK=off go test -race -count=5 -run 'TestIndex_BudgetExcludesAndRetries|TestIndexAnyway_PersonOnly|TestJobs_AMessageEndsTheRun' ."
  echo "----"
} > "$OUT/three.log"
GOWORK=off go test -race -count=5 -run "$RUN" . >> "$OUT/three.log" 2>&1
RC=$?
{
  echo "----"
  echo "go test exit code: $RC"
  echo "loadavg at end: $(cat /proc/loadavg)"
  echo "=== END $(date -u +%FT%TZ) ==="
} >> "$OUT/three.log"

for p in $PIDS; do kill "$p" 2>/dev/null; done
echo "load killed. loadavg now: $(cat /proc/loadavg)"
echo "go test exit code: $RC"
echo "log: $(wc -l < "$OUT/three.log") lines, $(wc -c < "$OUT/three.log") bytes"
grep -E '^(--- (FAIL|PASS)|    --- (FAIL|PASS)|FAIL|ok  |panic|WARNING: DATA RACE)' "$OUT/three.log" || true
exit $RC
```

Copy the saved load-reproduction log out of the ephemeral testplan output dir into the checkout (`testplans/st_1m81xcydx2x1chwyds4zww-repro.log`) so it is committed to the branch and durable for the fix-flakes task, which turns on this output.

```sh inputs=$OUT/three.log
set -e
cd /home/nthalk/local/src/iodesystems/dun.wt0
cp "$OUT/three.log" testplans/st_1m81xcydx2x1chwyds4zww-repro.log
echo "saved: $(wc -l < testplans/st_1m81xcydx2x1chwyds4zww-repro.log) lines, $(wc -c < testplans/st_1m81xcydx2x1chwyds4zww-repro.log) bytes"
head -3 testplans/st_1m81xcydx2x1chwyds4zww-repro.log
grep -E '^(--- FAIL|FAIL)' testplans/st_1m81xcydx2x1chwyds4zww-repro.log
```

Reproduce the three harness flakes under load, writing the full failure log directly into the checkout (`testplans/st_1m81xcydx2x1chwyds4zww-repro.log`) so it is committed to the branch and durable for fix-flakes. 40 busy-loop load generators; `-race -count=5` of the three timing-sensitive tests. Block exits 0 when a flake fires (reproduced — the log is the evidence) and 1 when the round is clean (run more rounds).

```sh
set +e
ROOT=/home/nthalk/local/src/iodesystems/dun.wt0
cd "$ROOT/internal/harness"

PIDS=""
for i in $(seq 1 40); do
  nohup sh -c 'while :; do :; done' >/dev/null 2>&1 &
  PIDS="$PIDS $!"
done
echo "loadavg settled: $(cat /proc/loadavg)"
sleep 3

RUN="TestIndex_BudgetExcludesAndRetries|TestIndexAnyway_PersonOnly|TestJobs_AMessageEndsTheRun"
LOG="$ROOT/testplans/st_1m81xcydx2x1chwyds4zww-repro.log"
{
  echo "=== START $(date -u +%FT%TZ) ==="
  echo "loadavg at start: $(cat /proc/loadavg)"
  echo "cmd: GOWORK=off go test -race -count=5 -run '<the three tests>' ."
  echo "----"
} > "$LOG"
GOWORK=off go test -race -count=5 -run "$RUN" . >> "$LOG" 2>&1
RC=$?
{
  echo "----"
  echo "go test exit code: $RC"
  echo "loadavg at end: $(cat /proc/loadavg)"
  echo "=== END $(date -u +%FT%TZ) ==="
} >> "$LOG"

for p in $PIDS; do kill "$p" 2>/dev/null; done
echo "load killed. loadavg now: $(cat /proc/loadavg)"
echo "log written to checkout: $LOG ($(wc -l < "$LOG") lines)"
echo "go test exit code: $RC"
grep -E '^(--- (FAIL|PASS)|FAIL|ok  |WARNING: DATA RACE)' "$LOG"
# exit 0 = reproduced (a FAIL line present, the log is the evidence); exit 1 = clean round.
grep -qE '^--- FAIL' "$LOG"
grep_rc=$?
if [ $grep_rc -eq 0 ]; then echo "REPRODUCED"; else echo "CLEAN ROUND"; fi
exit $grep_rc
```
