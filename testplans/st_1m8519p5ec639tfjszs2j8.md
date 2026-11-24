# Test plan: st_1m8519p5ec639tfjszs2j8

## Block 1 — reproduce both flakes under load

The failure on 2026-11-18 was both tests in full `make test` runs while
several full suites ran at once. This block builds that load — three full
`make test` suites started back-to-back — and, while it runs, takes each
flaky test from its own package with `-race -count=50 -v`:
`TestServerOverAUnixSocket` in `./internal/dund/` and
`TestJobs_ReplyCarriesProcessUsage` in `./internal/harness/`.

The block's verdict is the tests' verdict, captured (`|| RC=$?`) and
re-raised on the final lines: a flake firing in either package makes the
run red, and that red run is the reproduction this spike exists to show.
All 50 passing makes it green — the "did not reproduce at this load"
finding for the next step (raise the load), not a green to be trusted.

Per-run timing lines and any deadline text (`i/o timeout`,
`never learned its process group`) are extracted, so a slow or failed
run is attributable to the specific wait that fired:
`client.until`'s flat 10 s `SetReadDeadline`
(internal/dund/server_test.go:47) for dund, and the 3 s / 10 ms PGID poll
(internal/harness/bgjob_test.go:593-599) for harness.

```sh produces=load/summary.txt inputs=internal/dund/server_test.go,internal/harness/bgjob_test.go
set -eu

LOG="$OUT/load"
mkdir -p "$LOG"

echo "== machine =="
nproc > "$LOG/ncpu"
uptime >> "$LOG/ncpu"

# Load of the 2026-11-18 failure: several full suites at once on a shared
# box. Three `make test` suites back-to-back, with GOFLAGS=-count=1 so the
# go test cache cannot make them finish in seconds: the load has to be real.
echo "load start: $(date)" >> "$LOG/ncpu"
uptime >> "$LOG/ncpu"
GOFLAGS=-count=1 make test > "$LOG/make1.log" 2>&1 &
M1=$!
GOFLAGS=-count=1 make test > "$LOG/make2.log" 2>&1 &
M2=$!
GOFLAGS=-count=1 make test > "$LOG/make3.log" 2>&1 &
M3=$!
trap 'kill "$M1" "$M2" "$M3" 2>/dev/null || true' EXIT

# While they run: each flaky test 20 times, race on, verbose per-run lines.
# The block's verdict is the tests' verdict, captured and re-raised below:
# a flake firing is a red run, which is the reproduction this spike shows.
# While they run: each flaky test 50 times, race on, verbose per-run lines.
t0=$(date +%s)
DUND_RC=0
go test -race -count=50 ./internal/dund/ -run 'TestServerOverAUnixSocket' \
  -v > "$LOG/dund-count50.log" 2>&1 || DUND_RC=$?
HARNESS_RC=0
go test -race -count=50 ./internal/harness/ -run 'TestJobs_ReplyCarriesProcessUsage' \
  -v > "$LOG/harness-count50.log" 2>&1 || HARNESS_RC=$?
t1=$(date +%s)
{
  echo "count50 wall: $((t1 - t0)) s (dund rc=$DUND_RC, harness rc=$HARNESS_RC)"
  wait "$M1" 2>/dev/null && echo "make1 rc: 0" || echo "make1 rc: $?"
  wait "$M2" 2>/dev/null && echo "make2 rc: 0" || echo "make2 rc: $?"
  wait "$M3" 2>/dev/null && echo "make3 rc: 0" || echo "make3 rc: $?"
} >> "$LOG/count20-summary"

# Per-run timing lines, and any deadline text: which wait fired.
grep -E '^(--- (PASS|FAIL|SKIP))|^ok|^FAIL|^panic' "$LOG/dund-count50.log" \
  > "$LOG/dund-lines" || true
grep -E '^(--- (PASS|FAIL|SKIP))|^ok|^FAIL|^panic' "$LOG/harness-count50.log" \
  > "$LOG/harness-lines" || true
grep -h 'i/o timeout\|deadline\|never learned' "$LOG"/*.log 2>/dev/null \
  > "$LOG/fired-deadlines" || true

# The artifact: one summary file.
{
  cat "$LOG/count20-summary"
  echo "--- dund runs ---"
  cat "$LOG/dund-lines"
  echo "--- harness runs ---"
  cat "$LOG/harness-lines"
  echo "--- fired deadlines ---"
  cat "$LOG/fired-deadlines"
} > "$LOG/summary.txt"

# Red when a flake fired in either package: that is the reproduction.
if [ "$DUND_RC" -ne 0 ]; then
  echo "DUND FLAKED (rc=$DUND_RC)"
  cat "$LOG/summary.txt"
  exit "$DUND_RC"
fi
if [ "$HARNESS_RC" -ne 0 ]; then
  echo "HARNESS FLAKED (rc=$HARNESS_RC)"
  cat "$LOG/summary.txt"
  exit "$HARNESS_RC"
fi
echo "no flake at this load: 50/50 clean for both"
```
