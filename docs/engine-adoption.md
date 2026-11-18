# Engine adoption: engines outlive a dund restart

Why and how a running `dun --engine` survives dund's own restart, and how the
next dund takes it back. Decided by the USER 2026-11-17 (aw4
i_1m7pe91wn18bq6j9m61cv1). Supervision in general: `sessions-and-dund.md`;
the coordinated upgrade this replaces as the normal path: "Coordinated
upgrade" there and `internal/dund/upgrade.go`.

## Why

Before this, every dund restart ended every engine. Four things did it:

- `svc.Close()` stopped each engine on every path out of `serve`, the
  in-place re-exec and the service handover included.
- Under systemd, the unit's default `KillMode=control-group` kills every
  process in the service's cgroup when dund exits (75, the handover). The
  engines' own process group (`Setpgid`) does not leave the cgroup.
- The engine wrote its events to fd 1, a pipe whose reader was dund: a dead
  dund meant EPIPE (SIGPIPE on fd 1 kills a Go program).
- dund was the engine's stdin: EOF closed the input and the engine drained
  and exited.

So an upgrade had to wait for every turn, job and sub-agent to end
(`upgrade_hold`, drain) or cut them and resume them from the transcript.
Adoption makes the restart cost nothing a person sees: turns and jobs run on.

## Shape (USER 2026-11-17)

1. Both restarts adopt: the in-place re-exec (`syscall.Exec`, a dund a client
   started) and the service handover (exit 75; systemd or launchd starts the
   new build).
2. The engine is not dund's stdio child any more. dund starts it with
   `--listen <home>/dund/engines`, stdin `/dev/null`, stdout and stderr to a
   per-engine log (`engines/<pid>.log`); the engine listens on
   `engines/<pid>.sock` and dund dials it.
3. Under systemd each engine runs in a transient scope of its own
   (`systemd-run --user --scope --quiet --collect --expand-environment=no`),
   so the service's cgroup does not hold it. When `systemd-run` or the user
   manager is missing, dund spawns it plainly and logs a warning once: such an
   engine dies with a handover and is resumed by the fallback.
   Under launchd the engine's own process group already keeps it out of
   launchd's clean-up; the plist also sets `AbandonProcessGroup` (see
   "launchd").
4. SIGTERM (`systemctl stop`, `launchctl bootout`, a person's kill) is still
   a stop: dund records and interrupts (`PrepareStop`) and kills every engine
   (`Service.Close`). Only an upgrade's restart leaves them (`Service.Detach`).
5. An adopted engine of an older build moves to the new build at its next
   idle (no turn, question, job or sub-agent): dund stops it and starts it
   again with `--resume` (the restart an idle stop already does). Also an
   engine that holds shared MCP servers through dund's gateway
   (`hello.shared`): the gateway died with the old dund.
6. During the gap turns and self-wakes run on. The engine keeps its events
   in a bounded ring (16 MB) with sequence numbers and replays them after the
   next dund's `link_resume{after}`. A ring that overflowed while no dund was
   there is a gap: the new dund sends `history{reason:"reattach"}` built from
   the transcript (the snapshot path) and then what the ring still holds.
7. Requests to dund in flight across the gap (`peer_request`: dun_list,
   dun_msg, schedule, OAuth, `mcp_share`) are sent again when a dund attaches;
   one still unanswered at its timeout fails with "dund is restarting".
8. The drain stays as the fallback. When adoption fails (proto or link
   version mismatch, socket gone, hello names another session) the engine is
   killed when it is provably ours and the session resumes as before: the
   old dund wrote the resume record for every working adoptable session
   before it left (`record.fallback`), and the new dund applies it.

## Wire format

One unix stream socket per engine, 0600, in a 0700 directory. Lines, `\n`
terminated. One connection at a time: a second dialer gets
`{"type":"link_busy"}` and is closed.

Engine to dund:

| line | meaning |
| --- | --- |
| `{"type":"link_hello",...}` | first line on every connection (below) |
| `<seq> <event-json>` | an event, `seq` ≥ 1, counting from the engine's start |
| `0 <event-json>` | a state event resent on a resume (not in seq) |
| `{"type":"link_gap","after":A,"first":F}` | events A+1..F-1 are gone (ring overflow) |
| `{"type":"link_live"}` | the replay is over; what follows is live |

dund to engine:

| line | meaning |
| --- | --- |
| `{"type":"link_resume","after":N,"state":B}` | first line; replay after N; B: resend state first |
| `{"type":"link_ack","seq":N}` | dund has taken every event to N; the ring drops them |
| any other line | a command, as on stdin before |

`link_hello` (`proto.LinkHello`): `link` (wire version, 1), `proto`
(`proto.Version`), `version` (the engine's build), `pid`, `session` (empty
until the engine named it), `seq` (last written), `acked` (last acked),
`first` (oldest kept), `shared` (gateway grants held).

`engines/<pid>.json` (`proto.EngineInfo`), written by the engine when it
listens and again when it names its session, removed when it exits: `pid`,
`sock`, `session`, `version`, `proto`, `link`, `started`.

## Sequence numbers and acks

- The engine numbers every event line it writes, from 1, for its life.
  dund's `proto.Log` numbers events for clients as before; the two are
  unrelated. A restarted dund has a new Log epoch, so clients get a snapshot
  as after any dund restart.
- dund acks every 64 events it takes, and exactly on detach: it stops reading
  (a read deadline), lets the session's reader append everything it was
  handed, then sends `link_ack` of the last one and closes. The next dund
  resumes after `hello.acked`, so a clean restart loses and repeats nothing.
  After a crash of dund the last periodic ack is behind by up to 64 events:
  those are sent again, to a new Log that never had them.
- While a dund is attached and the ring is over 16 MB the engine waits
  (backpressure, as a full pipe did). With none attached the oldest events
  go and the next resume gets `link_gap`.
- The state on resume: the engine keeps the latest event of each state type
  (`proto.IsStateType`, the Log's list) and, when asked (`state:true`, an
  adoption), sends those at or before `after` first, as `0 <event>`. The new
  dund's Log then holds the session's state (commands, settings, turn_state,
  jobs, agents …) and dund reads busy and idle from it as before.
- No dund for 2 minutes (`linkOrphanWait`): the engine stops as on a `stop`
  command. A dund that never comes back must not leave engines running; the
  resume record (or the crash check) wakes the session in the next one.

## Registry and adoption at start

`dund.New` scans `engines/*.json`, dials each socket and reads the hello
before it resumes sessions (`resumeFlagged`) and before the crash check:

- hello `pid` must be the file's and `session` a session in the registry.
  `link` and `proto` must be this build's. Then dund sends
  `link_resume{after: acked, state: true}` and the session runs on that
  engine; its fallback record is dropped.
- Dial refused or no hello: the socket is dead. The files are removed; the
  process is left alone (nothing proves the pid is still an engine).
- Version mismatch, or a session not in the registry: the hello came over
  the engine's own socket, so the process is ours; it is killed (its process
  group) and the files removed. The session takes its fallback record.
- hello `session` is not the file's: left alone and logged (PID reuse, or a
  file of another engine).
- `Engine.Wait` of an adopted engine: socket EOF, then the pid gone
  (`wait4` when it is still dund's child after an in-place exec, else polled).
  `Kill`: `SIGKILL` to its process group when the group is its own.
- Logs of exited engines (`<pid>.log` with no `<pid>.json`) are pruned after
  7 days. Engines that died in the gap after an in-place exec are still
  dund's children: they are reaped first (`reapExited`).

## Upgrade with adoption

- An adoptable engine (`linkEngine`) is not waited for: `Waiting()` leaves it
  out, it is not held (`upgrade_hold`), it is not noticed, automatic messages
  to it are not queued. An upgrade with only adoptable engines restarts at
  once.
- `PrepareStop` for an upgrade records a fallback for each working adoptable
  session (what `recordStop` would write), interrupts nothing of theirs, and
  records and interrupts the others as before.
- `serve` then calls `Service.Detach` instead of `Close`: adoptable engines
  are detached (exact ack), the others stopped. SIGTERM still calls
  `PrepareStop` for everyone and `Close`.

## Version skew

- `link` changes only with an incompatible change of this wire; `proto` is
  `proto.Version`. Either differing: no adoption, fallback.
- A newer dund adopts an older engine of the same `link`/`proto` and moves it
  to its own build at the next idle (5.). An older dund that does not know
  adoption leaves the engines' files alone; the engines stop after
  `linkOrphanWait` and the old dund resumes the sessions from the record.
  Until then a session may run in two engines (a downgrade only).
- A dund before adoption spawns engines over stdio, as before: those are not
  adoptable and drain as before.

## launchd

Documented (launchd.plist(5), `AbandonProcessGroup`): "When a job dies,
launchd kills any remaining processes with the same process group ID as the
job. Setting this key to true disables that behavior." Engines start in a
process group of their own, so launchd's clean-up does not reach them; Apple
DTS (developer.apple.com/forums/thread/852832) calls that clean-up best
effort, "the process can avoid being killed simply by changing its process
group". The plist sets `AbandonProcessGroup` as well, so a child that did not
leave the group (a future change) is not killed either. Not verified on a
Mac: `launchctl kickstart -k` and a KeepAlive relaunch with a live engine
are the checks to run.

## Tests

- `cmd/dun/enginelink_test.go`: resume after seq N (no loss, no repeat), ring
  overflow (`link_gap`), state on resume, a second dialer refused,
  backpressure, orphan stop, flush at exit.
- `internal/dund/adopt_test.go` (fake engines behind a fake link): adoption
  across a Service restart, the three fallbacks, an engine with no session,
  `reattach`, the move of an older build at idle.
- `cmd/dun/adopt_integration_test.go`: two real builds, a dund that re-execs
  mid-turn against a slow fake LLM; tokens in order with none lost or
  repeated, the turn completes, the engine moves to the new build at idle.
  `DUN_TEST_SYSTEMD=1` runs the same as a transient systemd user service
  through the exit-75 handover (passed on systemd 259, 2026-11-18).

## Not covered

- Shared MCP servers (dund's gateway) die with the old dund; an adopted
  engine that holds some is restarted at its next idle (5.). Until then a
  call to such a server fails.
- The partial reply streamed before the restart is not in a reconnecting
  client's snapshot until its `message` comes (as after any dund restart).
- `--docker`: the outer process holds the link and pipes the container's
  stdio through it (`dropListen`); not tested.
- launchd: not run on a Mac (see "launchd").
