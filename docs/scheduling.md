# Scheduled messages and loops

Why `/schedule`, `/loop` and `loop_ctl` are built the way they are. Code:
`internal/dund/schedule.go` (the scheduler), `cmd/dun/schedule.go` (the
commands, the time parser), `loopctl.go` (the tool), `proto/schedule.go`
(the wire types).

Request (USER, 2026-10-27): "managers might need to push scheduled events or
change messaging if needed" → "Push the send date, alter message"; "same
with /loop (like claude it installs a loop_ctl() tool)".

## Where it lives: dund

- **The scheduler is in dund, not in an engine or a client.** A message set
  for tomorrow 9:00 is set so the person can leave: no client is attached
  when it fires, and the engine has been stopped by the idle reaper (30
  minutes of nothing) long before. dund outlives both, and already starts an
  engine again on a command (`session.send` → `ensureRunning` with
  `--resume`). So a firing is just a command, and nothing new is needed to
  wake a session.
- Entries are kept in `<dund dir>/schedule.json`, beside `sessions.json`,
  written whole to a temp file and renamed, as the registry is. They survive
  a dund restart. A session with an entry scheduled into it is not forgotten
  for being empty (`dropIfEmpty`, `New`): that message is what will be asked
  in it. Forgetting a session drops its entries.
- **One goroutine, one timer on the earliest entry**, and every operation a
  scan. Entries are typed by a person one at a time; an index would be a
  second thing to keep right.
- **The timer is never trusted past a minute** (`schedMaxWait`). A monotonic
  timer does not count time a laptop slept, and does not see the wall clock
  move (NTP, DST). Waking once a minute to read the wall clock makes "15:00"
  mean the wall clock's 15:00.
- The scheduler takes no other lock while holding its own; `busy` and
  `deliver` reach into sessions and are called with it released.

## Delivery

- **A firing is a user message** sent with `session.send`, the path every
  typed message takes. A busy session therefore queues it into the running
  turn (mid-turn messages, `mid-turn-messages.md`); dund adds no queue of its
  own.
- **It is marked by a text prefix**, as dund's `[automatic] ` resume is:
  `[scheduled 14:02 for 15:00] <text>`, with ` from session <name>` when it
  was set in another session, and `due 15:00, sent 15:42` when it went more
  than a minute late (dund was down): the model must not take "the deploy at
  15:00" to be now. The TUI shows such a row with a ⏲ mark (`userRowAt`).
  The bracket is one short line because every first request is measured.
- A firing whose engine would not start is put back a minute later
  (`schedRetry`); one whose session is gone is dropped and logged.
- Target: this session by default; `--to <name|id>` names another of the
  owner's sessions. Names are dun_list's (`<folder>:<hhmmss>`), computed over
  every session of the owner, not only the loaded ones (a message may be for
  a session asleep until it arrives); the live-only names are tried too.
- Times are parsed in the engine (`parseWhen`), which runs as the person, in
  the local zone: `10m`, `2h30m`, `15:00` (tomorrow once passed), `tomorrow
  9:00`, RFC3339. dund only sees instants. `push <id> +1h` is relative to
  the entry's current time; `push <id> 1h` to now.

## Loops

- **A loop is a repeating entry**, not a second mechanism: `Every > 0` moves
  its time on by `Every` each run; `Every = -1` (`proto.SelfPaced`) is paced
  by the model. `/schedule list|push|edit|cancel` apply to loops as well.
- The first run is at once (as Claude Code's `/loop`). The next is counted
  from when a run actually went, so a dund that was down for an hour fires
  one late run, not a burst.
- **A loop run waits while its session is in a turn** (rechecked a minute
  later): a loop prompt stacked on its own previous run, still going, is the
  one message nobody wants. A one-shot message goes anyway; it is news.
- **Self-paced**: a run leaves the entry with no time (`Awaiting`) until the
  session's turn ends (dund sees `done`/`error` in the event reader). If the
  model called `loop_ctl` in that turn, its time is set already; if not, the
  default (`selfPaceDefault`, 20m) applies and a `schedule` notification in
  the session says so. A dund restart while one is awaiting applies the
  default, since the new dund will never see that turn end. So does an
  engine that exits mid-turn (`/exit` stops it, or it crashes): found live,
  a `/loop` typed just before `/exit` was left waiting with no time at all.
- "Busy" is in a turn AND loaded. dund reads busy from the engine's events,
  and an engine stopped mid-turn sends no `done`; found live, a loop was
  deferred twice for a session with no engine.
- Interval bounds 1m..24h (`loopMin`, `loopMax`), also the clamp on
  `loop_ctl`'s `next_in`: faster is a busy-wait paid in model calls, slower
  is a reminder, which `/schedule` is for.

## loop_ctl

- **Listed only while the session has a loop.** Every first request is
  measured; a tool nobody can use is tokens in every one. The same rule as
  `jobs` (listed after the first background job). The engine learns the
  count from dund: a `loops {active}` command precedes every loop firing (the
  engine reads stdin in order, so the tool is listed for the turn the firing
  starts), every scheduler reply carries the asking session's count, a
  change made from another session is pushed to a loaded engine, and an
  engine started for a session with a loop is told at once (`attach`). The
  dispatch is always wired, so a late call still answers.
- Args: `next_in` (a duration string), `stop`, `reason`. It acts on the
  session's loop that fired last; no id argument, because a session with two
  loops is rare and an id is one more thing in the schema. The reason goes to
  the person as a ⏲ line.
- A child (sub-agent) never has it, as it never has dun_list: loops fire into
  the root.
- The firing line tells the model what it needs and no more: `[loop 3, run
  2, every 10m: loop_ctl can stop it]` or `[loop 3, run 2, self-paced:
  loop_ctl sets the next run (else 20m) or stops it]`. Nothing is added to
  the system prompt.

## Plumbing

- `/schedule` and `/loop` are engine slash commands (`routeSlash`), so the
  TUI, the web palette and the phone all have them. The engine reaches dund
  the way dun_list and dun_msg do: a `peer_request` with op `sched`, answered
  by `peer_reply`. No new channel. The rest of the line goes as typed (case,
  spacing, newlines): the TUI hands the raw line for these two
  (`slashLine`), since its fields have lost them.
- `dun -p` with no dund has no peer link; both commands say they need dund.
- `/schedule edit <id>` with no text puts `/schedule edit <id> <text>` in the
  composer of the TUI that asked (control `schedule-edit`, like `unqueue`);
  other clients see the text as a line.
