# Decisions

Why the code is the way it is. Each entry is a rule plus the reason, usually a
measured failure. Entries marked (USER, date) are the owner's call; do not
reverse them without asking. Code paths are relative to the repo root.
UI rules are in `ui-decisions.md`, retrieval in `surfacing.md`.

## Working rules

- **Work that is not committed does not exist.** `git stash` skips untracked
  files and `git clean` deletes them; in a repo where other agents work, either
  can happen at any time. On 2026-08-01 a cherry-pick run from outside the
  session did both and destroyed ~3000 lines (sub-agent and background-job
  code, tests and the sub-agent spec). It was never in any branch or dangling
  object. Commit a new file when it compiles, before it is finished.
- **Design reasoning must live somewhere other than the code.** The sub-agent
  code was rewritten from its written decisions. The spec file itself was lost
  and could not be rewritten. Whether a feature survives a loss depends on
  whether its reasoning was written down. This file is that record.
- **Verify in the state that will ship.** Bugs below were found only by a live
  run (`ps --forest`, `/proc`, a counting stub, a pty capture), not by unit
  tests. Add the live check, and a test that fails without the fix.

## Bounding what the model reads

- **Bound what enters the window before managing what is in it.** One `cat`
  put 255,720 characters (~64k tokens) in the window to answer a question
  whose answer was "30", and every later turn paid again. An exec result over
  `execInlineMax` (20k characters, `exec.go`) is clipped to its start and its
  end; the whole output is spilled to a file whose path is in the gap. Both
  ends survive: a failure is at the bottom, a listing is useful from the top.
  Cuts land on line boundaries, or on rune boundaries when there are no
  newlines (invalid UTF-8 can break the transport). The clip runs after
  `Render`, so the `[exit: ...]` verdict stays in the tail. Measured on one
  task: 456,772 tokens became 32,916. Recap removes churn that already cost;
  the cap stops it being charged.
- **What the human sees must be what the model saw.** Tools report the call
  from inside their own wrapper, before the outer cap runs, so the TUI showed
  a quarter-megabyte the model never read. `cappedReporter` (`recap.go`) forces
  the clip to apply to the UI report too, and `capOnce` memoizes by content.
  Two clips would spill twice under two refs, and the human would be told to
  read a copy the model has never heard of.
- **A read that finds the answer must not clip the answer away.** Grep on a
  98,894-character single line returned the whole line, the cap took the
  middle, and the match was in the middle. A match inside a long line comes
  back as a window around it with its offset. The cap must hold through
  `recap`'s own door (`tail:100` once returned a whole blob).
- **Line-based readers are useless on output with no lines.** Minified
  documents defeat grep, head and tail. `recap({ref, at:N})` pages by offset
  and each page says where the next begins. "Unlined" means the longest line,
  not the line count.
- **An unset budget is unbudgeted, not a budget of zero.** agentkit's Shaper
  read `BudgetTokens: 0` as a zero-token ceiling, so the default
  (`DUN_CONTEXT_TOKENS` unset) compacted every turn: 45 folds in 29 minutes in
  one session, 5 of 50 entries surviving. A regression test pins it
  (`compaction_test.go`, `policy_test.go`). A shaping policy must set its own
  fields; setting only `BudgetTokens` skips the cheap rungs.
- **Compaction is always reported.** It is the only operation that destroys
  conversation, and it once ran unreported for three days. `OnCompaction`
  produces a TUI line, stderr and a `-p` event, with a per-turn counter so
  thrashing names itself.
- **The model may rewrite its own recent history (`recap`, `recap.go`),
  under three constraints.** It replaces a span with the account the
  conversation should have had: cheaper than compaction (no summarizer call),
  more accurate (the whole span is in view), and it fixes the record, so a
  misunderstanding stops misleading later turns.
  1. It is visible, not confirmed (USER, 2026-08-03). A prompt to approve a
     rewrite of a span the human would have to re-read is a delay, not a
     safeguard; the answer is always yes. Every recap applies and reports: a
     `▸` line that opens onto counts, what was kept and the replacement text.
     It never re-renders the removed content.
  2. Nothing is destroyed (USER). The span moves to `<session>.recapN.jsonl`,
     because churn is the evidence for fixing the tool that produced it. If
     the sidecar cannot be written, the recap aborts.
  3. Tool pairs stay whole. A call without its result is the one shape a
     provider rejects, so the span is planned before it is applied, and the
     live recap call is never inside its own span.
- **A `keep` term must mean what the model meant.** `keep:["exec"]` kept every
  exec call, including the 255k `cat` the recap existed to remove, because the
  model gives the tool name it knows. A bare tool name keeps only that tool's
  most recent call. The anchor prefers a user message; an assistant turn that
  echoes the phrase collapsed a span to 85 characters.
- **A context-management nudge must name what to remove and arrive when the
  churn happens.** A prompt line describing `recap` got zero uses. A
  window-size nudge was read and ignored: a threshold says "you spend a lot",
  not what to do. Firing on the shape of the churn worked first try (USER's
  idea): the same tool 3+ times in a row, or a result of 20k+ characters that
  the model has since called past. Each cue fires once; a repeated signal is
  one a model learns to skip. Measured, unprompted: 9 entries and 258,256
  characters removed, active context 65,056 to 1,057 tokens.
- **The system prompt must not describe a tool that is not there.** The prompt
  described `exec` with no backend, and quoted no deadline. Values (the exec
  grace) are interpolated from the constant, so the promise cannot drift from
  the code.
- **Tool docs prescribe only the habit that bites.** `toolDocs["node_query"]`
  (`mcp.go`) was written from a corpus of 39 distinct failing selectors: 25 were
  a bare file path where an id belongs, 8 a grep pattern quoted twice. Those
  two get the first lines of the cheat sheet. The corpus is a test in
  poly-lsp-mcp (`TestSelectorCorpus_*`, outside this repo, not checked here).
- **A tool must not index its own state.** Per-session worktrees live in
  `.dun/worktrees`, so indexing the workspace root indexed 16 copies of the
  repo, and workspace-wide queries returned stale duplicates. Both ends were
  fixed (poly-lsp-mcp skips `.dun`; raglit ingest skips the worktree dirs,
  `rag_ingest.go`). Watch for this whenever session state moves inside the tree.

## Delivering news to the model

- **News reaches the model inside a tool result, never as a new turn.**
  Background completions and mid-turn user messages share one buffer
  (`Harness.Say` / `Notify`, lifted by `withLiftedQueue`; see
  `mid-turn-messages.md`). A tool result the model is already reading is the
  only free slot, and an assistant message appended after another is what
  providers reject.
- **Three tiers, split by what a turn is worth.** `Say` (user) and `Notify`
  (background job) are news: if nothing picks them up, the driver runs a turn.
  `Aside` (tool set changed, reminders) is context: it rides a tool result or
  joins the next turn, is appended with `store.appendSilent`, and waits
  indefinitely otherwise. A turn whose only output is "ok, noted" is not worth
  buying.
- **A heartbeat reports the absence of news and must not buy a turn (USER,
  2026-08-03).** It went through `notifyAndWake`, so each reminder cost a full
  model call (and a suggestion call) to say nothing happened, starting at one
  minute. Reminders use `notifyQuietly` (`bgjob.go`): `Aside` plus the
  `onNotify` ping so the human still sees the line. One exception: a child
  blocked on `ask_parent` waits for an answer only the parent can give, so its
  note must wake the parent.
- **A request nobody asked for must be visible before it is defensible (USER,
  2026-08-03).** One user message cost two LLM calls. The second was
  `Suggestions()`, fired at the end of every turn. It used the default context
  builder instead of the session's `Build`, so it was never shaped (no stubs,
  no compaction) and never logged; every `built prompt` log line undercounted
  traffic. Now it goes through `h.Session.Build`, the engine no longer
  volunteers it, and the UI decides when to ask. `emitSuggestions`
  (`cmd/dun/main.go`) runs only for the `suggest` control command.
- **The suggestion trigger is an idle debounce, not a turn hook (USER,
  2026-08-03).** It fires only after `done`, after 3 s with no keystroke
  (`idleSuggestDelay`, `cmd/dun/tui.go`), with an empty input, at most once per
  idle. `len(suggestions) == 0` makes "once" hold although a keystroke
  restarts the clock. It is armed from `tuiModel.Update`, the funnel every key
  passes through, and never armed when it could not fire.
- **A control command that asks must not run on the reader goroutine.** That
  goroutine is the only reader of `answer` events; blocking it on a question
  means the answer can never arrive. `ctrlCmdAsks()` names the asking commands
  and `setCtrlCmd` detaches them (`cmd/dun/servers.go`). Slow round-trips
  (`suggest`, `remember`) are detached for the same reason. Peer requests in
  dund are answered on their own goroutine: delivery writes to another
  engine's stdin, and two readers waiting on each other's engines would
  deadlock.
- **A message typed during an exec wait frees the model (USER, 2026-10-23).**
  The owner sat behind a giant analysis and wanted the model to inspect it and
  do something else. `Say` closes `userWaitCh`; the exec grace wait, and
  `agent`/`agent_monitor` `wait:true`, also wait on it. On wake the job is
  promoted and the message rides the handoff. The handoff reports the time
  actually waited.
- **Soft turn budget: tell the model, then cut (USER, 2026-10-23).** Quote:
  "shouldn't we note this to the agent? instead of killing the flow." At
  `--timeout` the model is told (on its next call, after a tool result, never
  between a call and its result) to checkpoint and end the turn. After a grace
  (`--timeout-grace`, default 20% of the budget, 2m to 10m) the turn is cut
  through the interrupt path and the next turn gets one line naming what was
  running. Known: `dun.CompactionPause` is global, so a child's compaction
  pauses the parent's clock.
- **The turn budget is a pausable clock, not a context deadline.** Time a
  human spends in `ask_user` is not dun working (`cmd/dun/turnclock.go`,
  `withoutClock`). A deadline context cannot express that.
- **`--timeout` bounds a turn interactively, the whole run one-shot.** A wall
  clock on a session a human sits in front of killed the session context at
  30 minutes; every later turn failed instantly while the UI advised "send a
  message to retry". The engine now exits only on ctrl-C, an explicit stop or
  stdin EOF, and announces which (`exit` event). Auto-continue after a failed
  turn is disarmed until something new arrives, so a down provider cannot spin
  the loop.
- **A failed turn is recoverable, not terminal.** The conversation is on disk.
  `prepareTurn` pairs off orphan tool calls and re-kinds orphan results so
  the history is valid again; the next message resumes. Providers reject a
  history with an unanswered tool call, so every later request would fail the
  same way.
- **One retry policy, two scopes.** agentkit's `llm.Client` owns schedule and
  budget (`RetryPolicy()`). dun's turn-scope retry (`retry.go`) reuses them.
  Request scope covers everything up to response headers; turn scope covers
  what it cannot, a stream that died mid-generation. A cancelled request
  arrives wrapped in `*url.Error`, which satisfies `net.Error` and looks
  transient, so cancellation is checked explicitly.

## exec and background jobs

- **Every command starts as a job; nothing blocks the session
  (`exec.go`, `bgjob.go`).** History: a live session sat 14 minutes because the
  model ran `gh auth login` through `exec`; gh started an OAuth device flow and
  polled for a code printed into a pipe nobody read. A 5-minute kill was added,
  then replaced: a command now waits `execGrace` (30 s, the `timeout` argument
  overrides) and then is promoted to a numbered background job the model is
  told about. (A second cause confirmed it: with a persistent shell, `exit 7`
  exits the shell, so the sentinel line never prints, and the session sat
  "working" ten minutes with zero LLM requests. A job that is always tracked
  cannot hang that way.) There is no automatic kill of a wedged command, because kills
  throw away expensive work. A wedged job is caught by the heartbeat, which
  notifies instead. A caller with its own deadline (ship's `checkTimeout`)
  keeps it.
- **A command's outcome is its exit code; the text is for the model.**
  Pass/fail was `strings.Contains(out, "[exit:")`: a check that printed the
  marker failed spuriously, and a lost marker read as a pass.
  `ExecBackend.Run` returns `ExecResult`; `Failed()` is the only question
  callers ask. `Render()` still emits the marker for the model; nothing parses
  it. A missing backend is `Code:-1`. The TUI renderer matches the marker by
  position (last line) for the same reason.
- **A background job is silent until asked.** Streaming every job would narrate
  a build one line at a time. But silence until exit made a job that died in
  second 2 look like one still working. Resolution: opt-in per job
  (`exec_monitor` with `buffer_bytes`/`grep`/`ignore`) plus a log file whose
  path the model gets; a 50k-line log is for grep, not for the context. Reports
  carry only complete lines. Jobs are keyed by `#id`, not pid (the pid is an
  `sh` the model never sees). An explicit `exec_monitor` read flushes the
  buffer: asking is not a scheduled report.
- **Anything unbounded must say it is still there, debounced by whatever it
  already says (`heartbeat.go`).** Background jobs and resident sub-agents are
  unkillable by policy, so they announce themselves: 1m, 5m, 10m, 30m, 1h,
  then hourly (most wedges are immediate; late reminders cost the most). Every
  notification the thing sends resets the clock, hooked at the one place
  notifications leave (`bgJob.notify`, `subAgent.notify`). The reminder itself
  must not go through there, or it resets the silence it reports. It carries
  state and duration, no output.
- **exec runs a non-login, non-interactive shell** (`sh -c`, `shellFlags`).
  It was `sh -lc`, which sources the operator's profile; that is how
  `EDITOR=vim` reached the agent. The agent must not inherit the operator's
  aliases, PATH edits or editor: none is reproducible on another machine and
  all can block. Cost accepted: a tool installed only via a profile is not
  found, which fails loudly.
- **Nothing dun spawns may wait for a human, and a terminal is not the only
  way it can.** `detach` set `GIT_TERMINAL_PROMPT=0` but missed the editor:
  `git rebase --continue` launched `$EDITOR`, and vim held a rebase for ten
  minutes until Go's test timeout killed the tree. `setsid` does not save you;
  an editor with nowhere to draw may block somewhere else. `GIT_EDITOR=true`
  and `GIT_SEQUENCE_EDITOR=true` (`detach.go`). It is invisible in CI because,
  with `EDITOR` unset, git refuses with a readable error.
- **No child of dun may keep dun's controlling terminal.** `git push` to an
  https remote opens `/dev/tty` directly for credentials; stdin at `/dev/null`
  stops nothing. The Bubble Tea reader blocks on the same tty, so the kernel
  split keystrokes between the two (~40% reached the UI; a push parked 21
  minutes, prompt invisible under the alt screen). Every spawn goes through
  `detach()` (Setsid, `GIT_TERMINAL_PROMPT=0`). `killGroup()` is the other
  half: a detached child no longer dies with the terminal, so cancellation must
  signal the process group (SIGTERM, then SIGKILL after a grace). Not
  git-specific: ssh, sudo and docker login do this too.
  Not built: detecting a co-reader of dun's tty (`detach()` should make it
  impossible, and the 2026-07-31 hang was the other shape, a detached child
  that never wanted the tty). The technique, if a user reports dropped
  keystrokes again: a process whose `/proc/PID/fd` holds `/dev/tty`, whose
  `wchan` is `n_tty_read`, and whose `tty_nr` matches. Controlling tty alone
  false-positives on every child dun spawns; blocked-in-tty-read is the
  discriminator. Portable partial check: watch for termios drift from what
  Bubble Tea set (git's `disable_echo` calls `tcsetattr(TCSAFLUSH)`, which also
  discards queued input).
- **A spawned process must be stoppable by name, not just killable by pid.**
  Killing `docker run` does not stop the container; it runs to completion and
  then `--rm` removes it. Every run is `--name dun-<pid>-<n>` and the cancel
  path stops that name first. If a spawn delegates the real work to a daemon,
  killing the client is not cancellation.
- **dun does not call `gh`.** An expired token does not fail; it starts a
  device flow and waits. `pr` mode pushes and prints the `gh pr create` line
  for a human. Revertible when gh has a non-hanging call path. gh 2.45 does
  not gate the device flow on `CanPrompt()`: with no tty it prints a one-time
  code into a pipe and polls until the code expires, and no flag says "never".
  Restoring it needs all three: only non-auth subcommands; auth pre-flighted
  (`gh auth status`, non-zero means "no PR today", not something to fix); and a
  deadline on the call. Calling the GitHub API directly with a token meets all
  three and is the likelier route.

## Worktrees and ship

- **Ship was removed (USER, 2026-10-27: "remove ship").** Repos state their
  own shipping rules; a built-in ship tool and its prompt lines read as the
  blessed path. The `ship` tool, `/ship`, the `ship` section of dun.json and
  the `--ship`/`--pr`/`--no-ship` effects are gone. The flags are still
  accepted and do nothing (an older TUI or dund passes them), and a `ship`
  section in dun.json is ignored. The entries below that describe ship are
  history.

- **Isolation, not approval prompts.** The agent works in a git worktree on a
  fresh `dun/<ts>` branch (opt out with `--no-worktree`). dun never switches
  branches in the user's checkout.
- **`/worktree` answers about the repo, not the isolation mode (USER,
  2026-08-03).** `status` said "none (working in place)" when isolation was
  off, which states the mode and answers nothing. It reports the porcelain
  status over a pass-through `WorktreeInPlace`, and `commit` commits in place.
  `/worktree new` passes `h.Mounts()`: a worktree lives under
  `.dun/worktrees/`, so a go.mod `replace => ../agentkit` resolves only through
  a symlink beside it. Measured: build OK with the symlink, `replacement
  directory ../agentkit does not exist` without. It reuses the session's
  resolved mount list so symlinks and Docker volumes cannot describe different
  sets.
- **A commit message is written by the model and approved by the human (USER,
  2026-08-03).** `/worktree commit` once committed with the literal string
  "/worktree commit". Now one throwaway session (`Harness.CommitMessage`,
  `commit.go`; never appended to the conversation) sees the porcelain status,
  the diff vs HEAD bounded to 24k with `--stat` always intact, and the last
  three user messages as intent. Untracked contents are omitted (a new file is
  the largest, least informative part). The result is shown with
  commit/regenerate/cancel, three rounds. Unlike a recap, a commit message is
  written once and read forever. Default Conventional Commits; per repo via
  `commit.format` / `commit.instruction` in dun.json.
- **A worktree may be deleted when it holds no work, which is not the same as
  a clean tree (`prune.go`).** Measured: 37 registered, 1.1 GB; by `git status`
  34 of 37 looked dirty, almost all from one untracked index artifact. Work =
  commits the repo's HEAD cannot reach, plus edits to tracked files, ignoring
  artifact paths (`.dun/`, `.poly-lsp-mcp/`). Merged work is not work.
  Untracked files are not work. Anything holding work is reported, never
  removed: an unpushed branch may exist nowhere else.
- **Liveness must not disturb what it measures.** The prune skips trees touched
  within an hour (`liveGrace`), because several sessions run at once. A
  directory's mtime does not change when a file three levels down is edited, so
  liveness reads the newer of the directory and git's index. A plain
  `git status` refreshes the index, which is a write: the first scan stamped
  all 30 trees as touched now. Use `--no-optional-locks` in anything that
  polls. Verified by scanning twice and asserting no age got younger.
- **Automatic cleanup may only delete what is worthless; a human deletes the
  rest.** The startup prune removes empty trees and reports the ones with work.
  `/close` is the explicit destructive counterpart (worktree, branch and
  transcript, so `/resume` cannot offer it back). No pass that runs unasked may
  decide work is worthless.
- **Metadata is written when the fact is known, not when the process ends.**
  `SaveSessionMeta` was a `defer` in main, so it never ran on the TUI killing
  its engine or ctrl-C: 57 sessions, 0 metadata files, so `--resume` could not
  reuse a worktree. Path and branch are known the moment the worktree exists.
- **Never push anything that was not verified in exactly the state it will
  land in** (bors' "not rocket science rule"). Ship's order is fetch, rebase,
  checks, push (`ship.go`). Verifying before the rebase tests a tree that
  never exists anywhere. The residual hole is origin moving between checks and
  push; ship re-fetches after the checks and goes round again, bounded at
  `maxShipRounds` = 3 so a permanently busy trunk fails loudly.
- **A ship mode is a terminal state, and `allow` is the policy surface.**
  `verify`, `push` and `pr` differ only in where they stop. A repo says
  "agents open PRs, they do not push" by listing modes; a sub-agent is
  constrained the same way (`allow:["verify"]`).
  A possible `integrate` mode (advance the local base after a push) is not
  built. If it is, it must use `git fetch . HEAD:<base>`, never `checkout` in
  the main repo; `checkout` was the bug in the old `FastForwardLocal`.
- **Ship the branch you are on, against its own upstream (USER, 2026-08-01).**
  dun never switches branches, so HEAD's branch is the one being shipped and
  there is no second base to protect. `allowBasePush` was removed: it refused
  to push when `head == base`, which in a `--no-worktree` session is true by
  construction, so ship ran the whole pipeline and then declined to do its job.
  A repo that does not want agents pushing says so with `allow`. The rebase
  target is the tracking ref; with no upstream it falls back to the base; with
  no remote either, the checks are all of ship. The `head == base` check
  survives only in `pr` mode, where a PR from a branch onto itself is a
  contradiction.
- **Checks are serial waves of parallel commands; the shape carries the
  semantics.** The array is ordered, each object is not. A wave reports every
  failure (compile and lint broken are two fixes in one turn); the next wave
  does not start if one failed. Names are sorted before reporting, or Go's map
  iteration makes identical runs read as a changed result.
- **A conflicted rebase is a state, not an argument.** Ship detects
  `rebase-merge`/`rebase-apply` and resumes; the model is not asked to pass
  `continue-rebase`. The resume must run before anything reads the branch: a
  stopped rebase leaves HEAD detached.
- **Ship must not touch the user's checkout.** The old fast-forward ran
  `git checkout <base>` in the main repo, silently changing the human's
  branch, racing other sessions, and deleting the branch it had just pushed
  without pushing base. If advancing a local branch returns, use
  `git fetch . HEAD:<base>`, which moves the ref without a checkout.

## Sub-agents (`subagent.go`)

- **Purpose (USER): context offloading first, parallelism second.** A child
  does the expensive thing (fetch a page, read a 30k-line log) and returns two
  sentences; the tokens that produced them die with it. Every default is the
  cheap side.
- **A sub-agent is a second `Harness`.** Store and Shaper hang off Session, so
  context isolation forces the split. It shares the parent's MCP manager (no
  second index).
- **Custom tools are built into dun, in process.** `mcpmgr` is request/response
  only (`CallTool`, no server-to-client push), so an out-of-process agents
  server could not notify the parent. In process, `tell_parent` is a direct
  `h.Notify` onto the lift path.
- **Tool sets are chosen by role, and that is the enforcement.** Root gets
  `agent` (and `jobs` with its agent params); a child gets `tell_parent` and `ask_parent` and
  no `ask_user`. Depth 1 holds because a child is never handed `agent`; a child
  cannot reach the human because `ask_user` is absent (`applyTools`).
- **Decided (USER, 2026-07-31):** `tell_parent(status, message)`: status
  overwrites and never notifies, message is an event. `agent_monitor` also
  carries `tell`, `resume`, `quit`. Spawn is async; `wait:true` opts out. A
  child going idle fires the monitor, which is news and drives a turn.
  Children are resident until dismissed and re-askable, with no concurrency
  cap. Model per spawn with a config default. Session end kills children; on
  resume they report stopped and restart via `agent_monitor(resume:)`. The
  human gets a dedicated agents pane. Children do not get `ship`'s landing
  modes. Worktree: see the next bullet.
- **A child may be given its own worktree — `agent({worktree:true})` (USER,
  2026-10-26).** Default stays the parent's tree (cheap side: no new tree, no
  second index). Opted in, the child gets a fresh `dun/<ts>` tree off the
  parent's **current HEAD**, its own `HostShell` there, and its file tools
  rooted there — so its writes never reach the parent's tree and `ship` cannot
  find dirt that is not the parent's. It still shares the parent's MCP manager
  (the index answers about the same repo), and its branch and path are reported
  in `report()` so the parent can review, commit, or cherry-pick the result.
  Forking HEAD rather than the base branch is deliberate: the parent's work is
  usually the context the child is asked to build on, and a tree off the base
  would make the child re-derive it or hand back a patch that conflicts.
  A resume must REUSE the existing tree, never create a second one — the
  child's commits are in it.
- **Known risks:** resident children with no cap accumulate; the pane makes it
  visible, nothing prevents it. A shared-tree child writing freely means the
  parent's `ship` can find dirt that is not its own. Children do not use the
  turn clock. Per-spawn `worktree:"branch"` children were deferred until
  shared-tree children show how often one is needed.
- **A child's conversation is pulled, never pushed.** Child callbacks are nil
  by design (context isolation); feeding a UI would tag and forward N streams
  nobody watches. Pull also works for a stopped child, whose process is gone
  but whose transcript is not. A reply for a scope already left is dropped.
- **Lessons from the live runs:** without `wait` the parent improvised
  `exec("sleep 3")` and polled `agent_monitor` four times in a row; both now
  have `wait`. A nil-harness child would have panicked from its own goroutine;
  it now fails that one agent.
- **One `jobs` tool for both kinds of background work (USER, 2026-10-27).**
  `exec` and `agent` stay separate launchers; each hands back a numbered,
  typed job ("background job #N", "agent #N") that `jobs(job:N)` or
  `jobs(agent:N)` watches. `wait`, `tail` and `kill` are common (kill ends a
  command's process group, or dismisses an agent); `tell`/`resume` are an
  agent's, `grep`/`buffer_bytes`/`ignore` a command's, and the wrong kind is
  an error naming what applies. While a `wait` is on a command, its finish
  notice is not sent, so the outcome is not said twice. A child gets `jobs`
  for its own commands, with no agent params, and its agent list is its own
  (empty). `exec_monitor` and `agent_monitor` still dispatch, hidden, for
  resumed sessions and habit (`quit` is `kill`).
- **One `session` tool for what runs the model and the task's state (USER,
  2026-10-27).** `harness` and `session_state` both answered "what is this
  session"; `session` returns `{harness, state}` as JSON, reads with no
  arguments and merges any of goal/plan/todo/status. The old names still
  dispatch, hidden. `dun session <id> info` and the harness file are
  unchanged.
- **Each fact once (USER, 2026-10-27: "reduce the verbosity and
  duplication").** The system prompt no longer repeats dun's tool
  descriptions (exec, exec_monitor, ask_user, recap, the sub-agent
  paragraph); it keeps the role, containment, which tool to use for files,
  the clip rule, how to work, and lines for MCP servers whose descriptions
  dun does not write. dun writes agentkit's `{OUTPUT}` note itself, shorter
  (`OmitSlotInstructions`). First request, empty dir, no servers but the
  shell: system prompt 5,025 → 1,780 chars, tool definitions 16,182 →
  12,720 JSON bytes, 14 → 12 tools.

## Tool servers and extensions

- **A tool server failing to start is never fatal.** It costs that family and
  is reported (`ServerState.Err`, the startup hint). Losing an in-flight
  session because raglit is misconfigured would be worse.
- **Servers start in the background; the session does not wait for the
  slowest.** A blank pane lasted ~45 s while poly-lsp initialized (its
  `started` line came 54 s after raglit's). Start waits for built-ins only up to
  `serverStartGrace` (1.5 s); a late server joins at the next turn boundary.
  The TUI attaches behind the first frame, not before it. One-shot runs call
  `WaitServers` so the task keeps every tool it started with.
- **The tool set is mutable; four fields derive from it.** `Session.Tools`,
  `Dispatch`, `System` and `Preparer` are recomputed together by
  `Harness.applyTools`, never set individually, or the next `/raglit` reverts
  them. A rebuild that lands while a turn holds `turnMu` is deferred to the
  turn boundary (swapping tools under a running turn is a data race).
- **A spawned server's stderr is evidence.** agentkit's `mcpmgr` keeps a
  bounded tail and appends it to start/initialize/list-tools errors; the
  protocol layer can only say "transport closed".
- **Two config files, two kinds of fact.** `dun.json` (committed) describes
  the project; `dun.local.json` (gitignored) describes this machine: binary
  paths, DSNs, secrets. Servers merge by id; omitted fields inherit, so
  overriding one binary's path does not require restating its args, and
  adding a fourth server does not re-list three. `disabled` is only ever
  turned on by a later layer (a bool cannot tell false from absent), so a local
  file cannot resurrect a server the project turned off. A parse error names
  its file. A commandless server is rejected at load.
- **MCP switches resolve session, then project, then global**
  (`mcpscope.go`): on/off, mode (standalone, or scripted = hosted in mcpshell
  and called through `eval`), and per-tool switches. Stored mode values stay
  `direct`/`shell` because an older dun rejects any other `via` and would fail
  to load `mcp.json`. Session writes go to `<session>.mcp.json` beside the
  transcript. Project and global switch files are separate so a repo can ship
  defaults and a person can override them.
- **Ids were renamed `code`/`docs` to `poly-lsp`/`raglit`.** Old ids are still
  read from config files and the session sidecar (`canonicalServerID`); new
  writes use new ids. Model-facing tool names, raglit index names and the
  `"rag"` config key were left unchanged on purpose.
- **Extensions are manifests (`extension.go`).** An extension is an MCP server
  plus `extension.json`: its server spec, slash commands, roles (`search`,
  `index`), hooks. Special cases (proactive search, memory, skill and rules
  indexes, the AGENTS.md guard) are lookups by role, not by server id. Bundled
  manifests are embedded (one copy, no drift). Index calls go to the MCP
  `ingest`/`forget` tools when the server has them, else a CLI fallback; when
  raglit adds both, dun switches with no code change. A slash name taken by an
  earlier extension or by dun itself is dropped from the later one.
- **mcpshell is looked for on PATH, then beside dun's executable** (`missing.go`);
  absent, dun runs with one tool less and says how to install it (`dun doctor
  --fix`). Installation by `go install` was chosen over bundling because it
  builds from source and is checked by sum.golang.org. (Release tarballs have
  since been added in `release/`; this fallback remains.)
- **Hosted-server shell options replace, not merge** (`shellhost.go`): a later
  layer's `shell` block replaces a lower one's. Bad globs are dropped with a log
  line, because mcpshell exits on one and would take the shell down.
- **Project rules are read like Claude Code** (`rules.go`): root AGENTS.md and
  CLAUDE.md, `@path` imports (relative to the importing file, depth 5, cycle
  safe, an `@token` counts only if it contains `/` or `.`). Identical content
  is included once. `/rules summary` condenses imports with one side call and
  refuses a summary that is not smaller; `/rules index` puts imports in raglit.
  One offer, once per hash, when the block exceeds ~4000 tokens.
- **Shared raglit indexes** (`attach.go`): raglit's daemon is shared and its
  watcher keeps indexes fresh; dun only reads. `/raglit attach` adds a
  qualified `proj:idx` to the search selector at session, project or global
  scope (union). Attached indexes are read-only from dun. Known risk: with a
  foreign index attached, the selector replaces raglit's default, so the
  project's `shared` namespaces are not read.
- **Images stay out of the session log.** Image parts are written to
  `blobs/<sha256>.<ext>` and the log keeps a reference (`store.go`). A message
  with images typed mid-turn waits for the turn; an image cannot ride a tool
  result. Vision is probed per model (`$DUN_HOME/vision.json`, key = provider
  URL + model); a refusal records `vision=false` and a configured `vision_model`
  describes images as text instead (the description becomes the entry, so a
  resume does not describe again). Images are scaled to 1568 px; that number
  and the 1 MiB PNG-to-JPEG cut were picked, not measured. Image tokens are
  not in agentkit's estimate, so the prompt meter counts them as overhead.
- **Spend ledger is append-only and lock-free (`ledger.go`).** One file per
  month under `$DUN_HOME/spend/`, one line per priced response, `O_APPEND`
  with one write under PIPE_BUF so concurrent processes never interleave. The
  key is stored only as a fingerprint. A provider that reports no cost shows
  "not reported", never $0.
- **dun-to-dun messaging goes over the engine's stdio** (`proto/peer.go`,
  `internal/dund/peer.go`): no second transport. Only the owner's loaded
  sessions are reachable; self is refused; 6 messages per minute per
  (sender, target). The engine unsets `DUN_DUND` at start so a dun run inside
  exec does not inherit the link. Children get no peer tools.

## Sessions, supervision and diagnosis

- **One representation, not two.** The agentkit entry list is both the model's
  context and the TUI history (`store.go`). Writes are a full rewrite to
  `<path>.tmp` plus rename: a crash leaves the whole old or whole new file.
  Entries over 8 KiB go to content-addressed blobs.
- **An EOF must name its engine.** Deliberately closing an engine (session
  switch, `/reconnect`) still yields an EOF from its reader goroutine. A
  supervisor that cannot tell that from a crash restarts the engine you just
  replaced; it broke `/resume` with three restarts and a give-up. `eofMsg`
  carries its `*dunProc`; a stale one is ignored.
- **The TUI supervises its engine, including the first one.** A crash (stdout
  EOF with no `exit` event) respawns against the same session id, skipping the
  history replay and restoring the switches. A first engine that never spawned
  does not stop the UI opening (that is where the error is legible and
  `/reconnect` lives) but must not reattach, or a fresh `dun` would grab an
  unrelated old session. Capped at 3 per 2 minutes (`engineRestartMax`,
  `engineRestartWindow`): a crash loop is reported, not hidden.
- **The TUI is the default mode; long flags are `--long`.** Bare `dun` printed
  usage and exited, making the interactive UI the one mode you had to ask
  for. A positional task still runs headless, so scripts do not open a UI.
  `flag.Usage` is overridden because Go prints `-tui`, which reads like
  `-t -u -i`.
- **A recorded session is the reproduction.** `DUN_TRACE` records engine events
  with offsets; `--replay` drives the real TUI from them. Built after three
  perf findings in a row had to be inferred from after-the-fact benchmarks.
  Replay compresses idle gaps (`--max-gap`, default 2 s) and keeps bursts
  verbatim, offsets stay absolute so the scheduler stays jitter-proof, and it
  reports what it compressed; silently rewritten time is not evidence.
- **Performance metrics must attribute, not just measure.** An input stall was
  glamour's auto-style: an OSC background query with a 5 s timeout that tmux
  does not answer, rebuilt on every `WindowSizeMsg` inside `Update`. It is now
  resolved once before the loop and skipped in multiplexers
  (`DUN_MD_STYLE`, `DUN_MD_QUERY=1`). It was found only because `/perf` names
  the message type behind the worst frame. Frame stats are always on
  (`/perf`); pprof is opt-in (`DUN_PPROF`).
- **Rendering is paced and cached** (details in `tui-architecture.md`):
  `refresh()` re-wrapped the whole scrollback per streamed token (7.8 ms per
  frame at 200 blocks, ~1 s CPU per 100-token reply, on the goroutine that
  reads the keyboard). Fixed by a per-block wrap cache (invalidated by width
  and open state) and a frame cap (`renderHz`).
- **dun writes nothing into a repo.** Per-project state lives under
  `$DUN_HOME/projects/<id>/`; a repo's own `dun.json`, `.dun/` and
  `dun.local.json` are read, never created (`mcp-and-tools.md`).
