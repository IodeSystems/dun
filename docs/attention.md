# Attention: notify, aside, and settle

Status: design, 2026-11-07. Part of it is on branch `idle-check` and is marked so.
The rest is a proposal and waits for the owner's decision (see "Open decisions").

The question this document answers: when something happens in a session (a job
ends, a child reports, the plan changes, the user goes away), does dun **wake** the
model, or only **update what the model will see** the next time it runs? A wake
costs a model request. A missed wake costs the work: an agent sleeps while a job is
wedged or a plan node waits for an owner.

`docs/mid-turn-messages.md` explains how a buffered message rides into a tool
result. This document is about which messages exist, when each one may start a
turn, and what changes.

## Principles

1. **Wake only for a decision.** A notify means "the model has something to
   decide now": a job ended, a child asked a question, work is ready with no
   owner. Everything else is information for the next time the model thinks.
2. **State is keyed; events are not.** Information that describes a current state
   (a job is quiet, the plan board, the open paths) is an aside with a key: a newer
   one replaces the older one. An event (a job finished) is a notify, and a
   notify can carry the state with it.
3. **Never wake to say that nothing happened.** A heartbeat about a silent job is
   not news, unless the agent is waiting on that job and no turn comes to carry
   it (see escalation).
4. **The user's message always wins.** It lifts into the running turn, or it
   starts the next one. Queued notifies ride with it; they do not take a turn of
   their own in front of it.
5. **No guessing from text.** Triggers come from facts dun has (a timer, a job's
   process group, the plan graph, the user's input), not from parsing commands or
   the model's prose. A replay on 2026-11-07 showed how brittle the text route is:
   a "changed but never ran it" detector over shell commands first misread
   `2>&1` as a file write, and after the fix it still nagged 22% of passing turns
   to catch 62% of failing ones.

## What exists

### The queue (`queue.go`)

Every message for the model goes into one buffer, `h.queue`. The kinds and
whether each may start a turn (`queuedPolicies`, field `causesTurn`):

| Kind | Who queues it | Starts a turn |
|---|---|---|
| `Say` (queuedUser) | the user, mid-turn | yes |
| `Notify` (queuedNotification) | jobs, children, the plan, MCP out-of-band | yes |
| `Aside` (queuedAside) | the session itself: tool set changed, recap nudge, heartbeats, plan line | **no** |
| peer, peer quiet, service | other dun sessions, dund | yes, no, yes |

Delivery, cheapest first:

1. **Lift.** `withLiftedQueue` → `liftQueued` appends everything queued to the
   next tool result the model reads. This needs no extra request.
2. **Turn injection.** `prepareTurn` publishes what is left at the start of the
   next turn: notifies as inbox arrivals (`flushQueued`), asides silently
   (`flushSilent`).
3. **Wait.** An aside with no tool result to ride and no turn coming waits
   indefinitely.

A notify starts a turn through `h.wake`: the engine's loop, and in `-p` mode
`drainStep` (cmd/dun/engine.go), block on it while background work runs.

### Keyed, escalating and turn-scoped asides (branch `idle-check`, `asidettl.go`)

`AsideWith(text, AsideOpts{Key, EscalateAfter, TurnScoped})`:

- **Key.** A newer aside with the same key replaces the queued one. In use for
  job and child heartbeats, the plan line, the recap nudge, the turn-limit
  warning. A replacement keeps the earlier escalation time, so a stream of
  updates cannot postpone a wake forever.
- **EscalateAfter.** If the aside is still undelivered after this long, it
  becomes a notify and wakes the agent. Used only by the job and child
  heartbeats (`--aside-escalate-ms`, default 5 s). If it is delivered first
  (lifted mid-turn), it never escalates. This closed a gap measured on the
  design: an agent that ended its turn to wait on a job that then went silent
  was never told, because "still running, quiet for 5m" could not wake it.
- **TurnScoped.** Dropped when the next turn starts. Used by the turn-limit
  warning, which describes only the turn that queued it.

### Background jobs (`bgjob.go`, `jobs.go`)

| Trigger | The model gets | Kind |
|---|---|---|
| the job ends | exit status, duration, log path | notify |
| `every_bytes: N` | "+N bytes, +M lines written since the last report", no output text | notify |
| `every_secs: N` | the same, at most every N seconds | notify |
| quiet for 1m, 5m, ... | "STILL RUNNING, quiet for 5m", plus a process sample: CPU and IO rates and a verdict from `procstat.Usage.Verdict` (waiting on disk or IO, only zombies left, nothing running, asleep) | keyed aside, escalates |
| `mute`, `kill` | no more reports; end it | |

Gaps: the stall verdict is computed only on the quiet heartbeat, so a job that
keeps printing (a spinner) while doing no work is never flagged. There is no
trigger on an error line; the pattern filter was removed in the jobs rework, and
`every_bytes` is the coarse stand-in.

### Child agents (`subagent.go`)

- A child's report (it finished, or it asks something) is a notify to the parent.
- While it works, a quiet reminder with its status is a keyed aside that
  escalates, like a job's.
- `agent({id, tell})` steers it.

### The plan (`plan.go`, `docs/taskplan.md`)

- At the start of each turn, the open plan is an aside, "[plan] ...", keyed
  "plan" (on `idle-check`).
- A child's change to nodes the main agent owns is an aside to the main agent
  (`planTell`). The user's change to a child's nodes is a notify to that child.
- `/plan approve`, edit and drop are notifies: the user asks the model to act.
- When a child publishes plan nodes, the main agent gets a notify that names
  the nodes now ready and says to hand them to an agent (`planFromChild`). So
  part of "the frontier moved" exists. Nothing wakes the main agent when a node
  becomes ready in another way (a dependency done by the main agent or a child
  that publishes nothing), when a node becomes blocked, or when the whole plan
  is done.
- The user's `/plan edit` and `/plan drop` are said to the main agent as an
  event beside the board (keyed per node and field, so a second edit replaces
  the first): the board cannot show what the user changed, and a dropped node
  only disappears from it.

### End of turn (`idlecheck.go`, agentkit `Session.OnIdle`; branch `idle-check`)

- `--non-interactive`: when a turn that used tools tries to end, the model gets
  the checklist once: list each stated requirement, check each by running it
  where it can run, and keep working while one is unmet and can still be
  fixed. The wording matters: "check, then give your final message" made a
  TB2.1 trial stop while it admitted a failing requirement.
- Both modes, main agent: open plan items at the end of a turn get one note.
- Interactive: no checklist.

### Other session messages

- Slash-command echoes to the model (`[control/...]`) are gone. A setting is
  not a request; the plan gate's refusal says what plan mode means.
- `/file` changes are an aside listing every open path and its mode, because a
  model told only "on" could not tell read from write.

## What would change

### 1. Delayed settle (interactive)

Built on branch `settle` (`settle.go`; `--settle-delay`, `DUN_SETTLE_DELAY`, control `presence`).

When a turn that used tools ends, a settle timer starts (proposed: 2 minutes).

- **The user replies first:** the timer is cancelled. The user is the check, and
  nothing is spent.
- **The user is active in the TUI:** the timer restarts. Typing in the composer,
  scrolling, opening a pane or `/plan` all mean the user is present and reading
  or about to answer. The TUI already tracks stillness for suggestions ("the
  input box is empty and the person has sat still"); it sends the engine a
  presence ping at most every 10 s or so while the user is active, and the
  engine only restarts the timer. The engine never sees keystrokes. A client with
  no keyboard (a `-p` script, a peer, dund) sends no pings, and only a reply
  cancels the timer.
- **Typing does not cut a settle turn that has started.** Only a sent message
  does (head chop, below): keystrokes are not a reply.
- **The timer fires:** one settle turn. The model compares what it did with the
  request, finishes what it can without the user, and ends with a status report
  that offers four outcomes:
  - **done:** what was done;
  - **remaining:** what is left, and that it continues;
  - **needs you:** a decision only the user can make;
  - **waiting on:** a job or an agent by id, and what for. The model sets that
    job's reports (`every_secs`, `every_bytes`) to the rate it wants, and ends
    the turn. The job's own notify wakes it.
- The timer does not wait for jobs or children. Long-running work is normal, and
  "waiting on" covers it.
- **Once per user message.** The settle turn never schedules another one.
- **Head chop.** If the user writes during the settle turn, it is cut at the next
  tool boundary (the interrupt that exists). Its work stays in the history. The
  settle prompt is turn-scoped and is dropped.
- **Resume.** A settle that was owed when the session stopped is recorded in the
  session file. On resume the first user message carries one line: say briefly
  what was done and what remains. No extra request is sent for it.
- `--non-interactive` keeps its immediate checklist: no one is coming back.

### 2. The plan board, and plan notifies

- The plan board is **one keyed aside**, replaced on every change by anyone,
  never a pile of change notes. Built on branch `settle` (`planBoard`, `plan.go`).
- These plan events become **notifies**, because each needs a decision:
  - a node became ready and no child owns it (the frontier moved);
  - a node is blocked;
  - the whole plan is done (this ends in the settle report).
- Everything else about the plan stays an aside.
- Children never notify the user. The user hears from the orchestrator, through
  its settle report and its "needs you" outcome.

This is the orchestrator pattern: the main agent may delegate planning to a
child, hands out the nodes, and talks with the user. It must know when there is
work to hand out or a decision to make, and it must not be woken by every node
a child marks done.

### 3. Notify debounce

Notifies that arrive within a few seconds of each other go out as one wake.
Three children that finish together cost one turn, not three. Build this only if
bursts show up in practice.

### 4. Notify plus state

A notify can carry the current state as a keyed aside. For example, "job #3
ended" (notify) arrives together with the jobs board (aside, keyed "jobs"), so the
model sees the event and where everything stands, in one delivery, and the board
does not pile up.

### 5. Stall verdict in progress reports

The process sample already taken for the quiet heartbeat also goes into the
`every_bytes` and `every_secs` reports, so "+2 KB written; CPU 0%, IO idle 4m"
tells the model that output continues but work does not. The sample exists, so
the cost is small.

## Not doing

- **A text detector for "changed but not checked".** It is brittle (see Principle 5).
  The reliable signal is a wrapped file system: `git status` in a worktree,
  `docker diff` under Docker isolation, a watcher or a capped mtime scan
  otherwise. That is a separate design.
- **A general "asides wake after N ms on idle".** It would buy a request to read
  slash-command echoes or recap nudges. Escalation per aside replaces it.
- **A trigger on error lines in job output.** It was removed in the jobs rework
  on purpose; it is not brought back here.

## Is it a good idea

Yes, as two steps.

1. **Settle and the plan board as one keyed aside.** These are small. They use
   pieces that exist (timer, interrupt, turn-scoped and keyed asides, the session
   file). They give the interactive user the end-of-turn report that the
   benchmark showed is needed: on TB2.1 at GLM-5.3 low effort, models in both dun
   and pi declared victory after a few steps. pi's failing trials ended after 2 to
   6 rounds, and dun's end-of-turn check turned some of those into passes.
2. **Plan notifies, debounce and the stall verdict,** only after step 1 is
   measured.

Risks:

- **Cost.** One settle turn per user message the user walks away from. Most are
  short: a few hundred tokens, mostly cached.
- **A settle that fires while the user reads.** Two minutes may be too short for
  a long reply. The delay must be easy to change, and measured.
- **The orchestrator woken too often.** That is why only actionable plan events
  notify, and debounce exists.
- **Complexity.** Each piece is small, but together they are a policy. That is why
  this document exists, and why the measurements below decide step 2.

## Measurements

- **Settle yield.** The share of settle turns that lead to more work (tool calls)
  instead of only a report. Near 0%, the delay is too short or the turns are
  already complete. High, the settle is worth its cost.
- **Wakes per orchestrated task,** and how many of them led to a dispatch or a
  decision.
- **Notify bursts:** how often two or more notifies arrive within 5 s. This
  decides whether debounce is built.

## Open decisions (owner)

1. **The settle delay:** 2 minutes of no reply and no TUI activity is a guess.
2. **Is a settle turn allowed to start a turn?** The rule is: only a user message,
   a dun message or an MCP out-of-band event may start a turn. A settle is a
   dun-originated wake. This document counts it as a dun message.
3. **Does the settle report stay in the context?** It is a summary for the user.
   It can be folded like any other entry, or dropped after the user's next message.
4. **Children and the user:** children never notify the user directly; only the
   orchestrator does. This document assumes that.
