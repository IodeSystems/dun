# Dispatch-queue idle decomposition — sprint_dun_15

Measured from the aw4 event log (sprint window 2026-11-20T02:45:25Z–03:52:05Z;
ledger dispatch_wait 46.2 min = 24.3 min queue + 7.6 min sibling gap), with the
aw4 source mechanism confirmed by reading the live repo
(~/local/src/iodesystems/aw4) via researcher sessions s_1m85jakbe36aqsn0fxy55v
and s_1m85jr5g47qrhvzwcktdmv.

## The lever: the `--every` daemon flag

The dispatch cadence in steady ready state is **pass wall duration + `every`**,
where `every` is the `--every` CLI flag on the aw4 daemon
(`aw4/internal/cli/daemon.go:992`: `c.Flags().DurationVar(&every, "every",
time.Minute, ...)`; wired to the dispatcher at `daemon.go:539`:
`newDispatcher(sessions.ended, every, ...)`; the tick fires at
`dispatcher.go:107`: `case <-time.After(d.every):`).

The timer is re-created inside the select on each loop iteration, after
`d.once()` has already run the pass (`dispatcher.go:89-108`), so in a steady
ready state (no session end, no poke) the next pass starts ~`every` after the
previous one finishes.

**The ready-queue recompute timer is NOT the cadence lever.** The prior
measurement named `internal/cli/readyq.go` as the place to shorten. A source
read shows `readyFloor` (60 s, `readyq.go:150`) is a per-event staleness bound
on how often the ready queue recomputes (`readyq.go:373-388`: `if !q.dirty ||
(q.started && now.Sub(q.lastStart) < q.floor) { return }`). In steady ready
state with no new events the queue is not dirty, so it does not recompute at
all, and the recompute path never pokes the dispatcher
(`readyq.go:476-484`: poke fires only when `owed || newOwed`). Shortening
`readyFloor` changes nothing about pass cadence.

## Measurement

From the event log (11 `dispatch.passed` events in the window):

| Component | Value | Source |
|---|---|---|
| Pass wall duration | ~86 s median (range 77–127 s) | prior: "record at − pass_at" |
| Idle gap to next pass start | ~236 s (derived: 322 − 86) | derived |
| Pass-start-to-pass-start interval | ~322 s median (range 302–396 s) | prior: 11 dispatch.passed events |

The idle gap is ~73% of the interval. With `--every` at its 1 m source
default, the idle gap should be ~60 s. The measured ~236 s gap is ~4× that.

## Two explanations for the ~236 s gap

1. **The daemon was launched with `--every` set to ~4 m** (a runtime flag, not
   in the repo). Most likely. Directly testable: check the daemon launch
   command for the dun sprint.
2. **The pass wall duration is longer than 86 s** (setup/teardown outside the
   event pair, or `d.mu` held by housekeeping). Would add to the interval
   without changing `every`.

Either way the lever is the same: reduce the idle gap between passes in
steady ready state.

## Decomposition of the 24.3 min queue component

| Component | Contribution | Mechanism |
|---|---|---|
| **Recompute cadence / `every` tick** | **Dominant** (~176 s of the ~236 s gap if `--every` is ~4 m; the full 24.3 min if the gap is the bulk) | `time.After(d.every)` in `dispatcher.go:107`; `--every` flag at `daemon.go:992` |
| Pass wall duration | ~86 s per pass (secondary) | fold-only, serialized on `d.mu` (`dispatcher.go:113-115`); housekeep-in-same-pass (i_1m83jds5f2kyg19hjbn707) |
| Oscillation (`releaseUnlisted`) | 81 stamps in window, ~7 per pass (a fraction of 24.3 min) | over-inclusive `movedSince`/`scanMoved` drop path (`readyq.go:646/673/762`); each dropped row re-offers at the next recompute |

## Expected wall-time saving

- **If `--every` is ~4 m at runtime:** changing it to 1 m (or omitting it,
  since 1 m is the source default) reduces the idle gap floor from ~4 min to
  ~1 min. ~176 s saved per interval × 11 intervals ≈ ~32 min, bounded by the
  24.3 min queue component — so roughly the full 24.3 min if the gap is the
  dominant contributor.
- **If `--every` is already 1 m:** the lever shifts to pass wall duration
  (housekeep split out of `sweepDispatch`/`sweepOnce`, `orchestrate.go:1681`).
  Expected saving: ~86 s × 11 ≈ ~16 min, bounded by 24.3 min.

## What the dev subtask should confirm first

The exact `--every` value the sprint_dun_15 daemon was launched with. This is
a runtime arg, not in the aw4 source. If it is >1 m, the fix is a
launch-command change, not a source edit. If it is 1 m, the lever shifts to
pass wall duration (housekeep split).

## Provenance

- aw4 `internal/cli/dispatcher.go:89-108` (run loop, timer re-created each
  iteration, `d.once` after) — researcher s_1m85jr5g47qrhvzwcktdmv
- aw4 `internal/cli/dispatcher.go:112-120` (`once`, `d.mu.Lock` at :115) —
  researcher s_1m85jr5g47qrhvzwcktdmv
- aw4 `internal/cli/daemon.go:99` (var `every`), `daemon.go:992` (flag
  default `time.Minute`), `daemon.go:539` (`newDispatcher(sessions.ended,
  every, ...)`) — researcher s_1m85jakbe36aqsn0fxy55v
- aw4 `internal/cli/readyq.go:150-152` (`readyPoll=2s`, `readyFloor=60s`,
  `readyMaxAge=2m`), `readyq.go:373-388` (step floor gate),
  `readyq.go:476-484` (poke site: `owed || newOwed`) — researcher
  s_1m85jakbe36aqsn0fxy55v
- Prior measurement (sprint_dun_15 event log, 11 dispatch.passed events,
  intervals 359.8/308.5/345.5/376.7/308.6/396.0/302.4/~302/~348/~323 s, pass
  wall ~77–127 s, 81 released stamps): tech_lead s_1m85h9h9zd8yr81dvt3c2w,
  room post seq 3688861
