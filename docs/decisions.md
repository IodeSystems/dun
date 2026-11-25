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
  `execInlineMax` (20k characters, `internal/harness/exec.go`) is clipped to its start and its
  end; the whole output is spilled to a file whose path is in the gap. Both
  ends survive: a failure is at the bottom, a listing is useful from the top.
  Cuts land on line boundaries, or on rune boundaries when there are no
  newlines (invalid UTF-8 can break the transport). The clip runs after
  `Render`, so the `[exit: ...]` verdict stays in the tail. Measured on one
  task: 456,772 tokens became 32,916. Recap removes churn that already cost;
  the cap stops it being charged.
  2026-11-18 (USER): `file_read`'s `whole` parameter is dropped; a read with
  no range returns the whole file when it fits the context left, and is
  refused with the numbers when it does not (`docs/reading.md`, section 2).
  A model sent `whole` with a range and got an error for it.
- **What the human sees must be what the model saw.** Tools report the call
  from inside their own wrapper, before the outer cap runs, so the TUI showed
  a quarter-megabyte the model never read. `cappedReporter` (`internal/harness/recap.go`) forces
  the clip to apply to the UI report too, and `capOnce` memoizes by content.
  Two clips would spill twice under two refs, and the human would be told to
  read a copy the model has never heard of.
- **A read that finds the answer must not clip the answer away.** Grep on a
  98,894-character single line returned the whole line, the cap took the
  middle, and the match was in the middle. A match inside a long line comes
  back as a window around it with its offset. The cap must hold through
  `recap`'s own door (`tail:100` once returned a whole blob).
- **Line-based readers are useless on output with no lines.** Minified
  documents defeat grep, head and tail. `file_read({path, at:N})` pages by byte
  offset and each page says where the next begins (agentkit v0.25.0; this was
  `read_saved({ref, at:N})` until 2026-11-06). "Unlined" means the longest
  line, not the line count.
- **Saved output is a file, named by path, not a ref** (USER 2026-11-06). A
  clipped result or a job log is written to the session's bg dir and to its
  isolation tmp, `$TMPDIR/dun-<session>` (`internal/harness/isotmp.go`), the same path on the
  host and under `--docker` (mounted read-only there). `file_read` and
  `exec` both open it, so `read_saved` was retired: one tool fewer on every
  request. A ref existed only because the host path did not exist inside the
  container. A wiped `/tmp` is refilled from the bg dir at start, resume and
  rehoist.
- **An unset budget is unbudgeted, not a budget of zero.** agentkit's Shaper
  read `BudgetTokens: 0` as a zero-token ceiling, so the default
  (`DUN_CONTEXT_TOKENS` unset) compacted every turn: 45 folds in 29 minutes in
  one session, 5 of 50 entries surviving. A regression test pins it
  (`internal/harness/compaction_test.go`, `internal/harness/policy_test.go`). A shaping policy must set its own
  fields; setting only `BudgetTokens` skips the cheap rungs.
- **Compaction is always reported.** It is the only operation that destroys
  conversation, and it once ran unreported for three days. `OnCompaction`
  produces a TUI line, stderr and an `--engine` event, with a per-turn counter so
  thrashing names itself. A fold in progress is reported too: the summary call
  takes seconds, and the turn looked stalled. `compacting` (start) goes out when
  a fold begins, and every start ends with the `compaction` or a `compacting`
  end (failed, cancelled, nothing folded), so a client's "Compacting…" row
  always stops (`internal/harness/compaction.go` foldBegin/foldEnd).
- **The model may rewrite its own recent history (`recap`, `internal/harness/recap.go`),
  under four constraints.** It replaces a span with the account the
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
  4. It stays inside the current turn (USER, 2026-10-28). The span starts at
     or after the latest user message and never crosses one; `from` is
     optional and only starts it later. A correction typed between turns is a
     user message, so it cannot become the model's paraphrase. Words typed
     mid-turn ride a tool result (`liftQueued`); when that result is
     recapped, they are copied into the summary verbatim. The `user` field,
     which let the model reword the user's message, is gone for the same
     reason. Earlier turns are finished work: making them smaller is
     compaction's and LOD's job, by policy. The churn stays visible to the
     human (scrollback line, sidecar) without the model paying for it.
- **A `keep` term must mean what the model meant.** `keep:["exec"]` kept every
  exec call, including the 255k `cat` the recap existed to remove, because the
  model gives the tool name it knows. A bare tool name keeps only that tool's
  most recent call. A `from` prefers the user message; an assistant turn that
  echoes the phrase collapsed a span to 85 characters.
- **A removed call stays listed (stub), by default** (USER, 2026-11-08;
  `internal/harness/recapstub.go`). The summary lost which commands ran and how each ended,
  and in long Terminal-Bench trials cheap models ran them again. Each removed
  call that `keep` and `drop` do not name becomes one line in the
  replacement: tool, arguments cut to 80 characters, outcome (an exec's from
  its recorded ExecResult, its exit line after a resume), result size, and a
  file with the result in the isolation tmp. `file_read` on that path is the
  one recall route. Past 40 lines the earliest become one count line. `drop`
  (matched as `keep`) leaves no stub, for noise. The stubs are text in the
  replacement entry, so no tool pair is split. The recapN.jsonl sidecar
  stays: it is the audit trail of the whole span, with the prose and the
  dropped calls the result files do not hold.
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
- **The system prompt teaches bubbleteandmd's directive forms (Carl,
  2026-11-11).** `prompt.Format(prompt.Options{})` (Math and Diagrams off: the
  TUI shows math and mermaid as code) follows `replyFormatSystem`
  (`internal/harness/replyformat.go`): 2,000 characters, about 500 tokens by chars/4, fixed text
  so it keeps the cache. On by default; off with `DUN_FORMAT_PROMPT=0`
  (`Config.NoFormatPrompt`, env only, like `DUN_USER_RULES`). Absent in a
  sub-agent (its reply goes to the parent model) and under `--non-interactive`
  (no one reads along), so the Harbor adapter, which uses both, never has it;
  it also sets `DUN_FORMAT_PROMPT=0` itself. Present in `--no-tools` chat.
- **A reply's forms take keys from the conversation, not the composer
  (2026-11-12, `docfocus.go`).** Each reply renders through a bubbleteandmd
  `Doc` (layer 3) at height 0: dun keeps its own scroll. tab is already the
  composer's completion and the zone cycle, so focus enters where the
  composer is not the key target: the selected answer in convo focus. enter
  gives its forms the keys; → does too when no block is cut at the edge (→
  descends into a wide block first, so its sideways scroll is unchanged). The
  answer selected gets the keys, not the last one with forms: the selection
  is what the person is on. Inside: tab/shift+tab form, enter/space toggle,
  ←/→ tab or tree cursor; ↑/↓ stay dun's scroll and the forms keep the keys
  while the answer stays selected. Leaving (tab past the last form,
  shift+tab before the first, esc, ← on a form that does not take ←) is one
  level out to the answer and is consumed, as ← at a wide block's left edge:
  an extra tab does not change the zone. A fold or tab switch is the
  person's action, so the answer may shrink (its held height starts over).
  A finished reply keeps its Doc; a width change makes a new Doc with a copy
  of the State (folds, tabs, focus).
- **Tool docs prescribe only the habit that bites.** `toolDocs["node_query"]`
  (`internal/harness/mcp.go`) was written from a corpus of 39 distinct failing selectors: 25 were
  a bare file path where an id belongs, 8 a grep pattern quoted twice. Those
  two get the first lines of the cheat sheet. The corpus is a test in
  poly-lsp-mcp (`TestSelectorCorpus_*`, outside this repo, not checked here).
- **A tool must not index its own state.** Per-session worktrees live in
  `.dun/worktrees`, so indexing the workspace root indexed 16 copies of the
  repo, and workspace-wide queries returned stale duplicates. Both ends were
  fixed (poly-lsp-mcp skips `.dun`; raglit ingest skips the worktree dirs,
  `internal/harness/rag_ingest.go`). Watch for this whenever session state moves inside the tree.

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
  minute. Reminders use `notifyQuietly` (`internal/harness/bgjob.go`): `Aside` plus the
  `onNotify` ping so the human still sees the line. (The one exception, a
  child blocked on `ask_parent`, is gone: a child asks by ending its turn.)
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
  budget (`RetryPolicy()`). dun's turn-scope retry (`internal/harness/retry.go`) reuses them.
  Request scope covers everything up to response headers; turn scope covers
  what it cannot, a stream that died mid-generation. A cancelled request
  arrives wrapped in `*url.Error`, which satisfies `net.Error` and looks
  transient, so cancellation is checked explicitly.
- **Hints given once are forgotten when a fold or recap drops them
  (USER 2026-11-20).** A session flag that remembers a model-facing hint was
  said kept the model from seeing it again after a fold or recap summarized
  its text away. `forgetters` (`internal/harness/forget.go`) is the one
  registry of resets: the exec cd-nudge counter, and the recap cues (except
  large-result ones, whose result and aside go or stay together). It runs
  from two call sites, one per way the history shrinks: `noteCompactionCause`
  (every fold) and `applyRecapText` (every recap). A reset means "allowed
  again": each hint fires again only when its trigger recurs. Not reset:
  what the person sees once per session (`setupSaid`, `indexAsked`), the
  learn cost cap, and `rulesDocs` (a classifier, not a said-flag).

## exec and background jobs

- **Every command starts as a job; nothing blocks the session
  (`internal/harness/exec.go`, `internal/harness/bgjob.go`).** History: a live session sat 14 minutes because the
  model ran `gh auth login` through `exec`; gh started an OAuth device flow and
  polled for a code printed into a pipe nobody read. A 5-minute kill was added,
  then replaced: a command now waits `execGrace` (30 s, the `timeout` argument
  overrides up to `execGraceMax`, 300 s) and then is promoted to a numbered background job the model is
  told about. (A second cause confirmed it: with a persistent shell, `exit 7`
  exits the shell, so the sentinel line never prints, and the session sat
  "working" ten minutes with zero LLM requests. A job that is always tracked
  cannot hang that way.) There is no automatic kill of a wedged command, because kills
  throw away expensive work. A wedged job is caught by the heartbeat, which
  notifies instead. A caller with its own deadline (ship's `checkTimeout`)
  keeps it.
- **The end of other background work ends an exec's wait (`workEnded`,
  `notifyEnded`).** A Terminal-Bench model waited beside job #1 with
  `sleep 280` execs and killed the job at 33 min. Now a job's completion or a
  sub-agent's report, arriving while an exec waits out its grace, promotes that
  exec at once (as a user message does), and the notice rides its result
  (`liftQueued`), so it is delivered once. Only an end counts: a progress
  report or a reminder does not stop a command the model chose to wait for. A
  muted job says nothing, so it releases nothing. The exec's own command does
  not count (it is provisional while waited on). A notice queued before the
  call does not release it: it rides the result anyway, and a release at once
  would make every quick command after a finish a job. Sub-agent reports are
  included: the model is told it will hear when a child finishes, the same
  promise as for a job.
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
  and `GIT_SEQUENCE_EDITOR=true` (`internal/proc`). It is invisible in CI because,
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
  branches in the user's checkout. For paths outside the workspace this is
  reversed: see "File access asks in the turn (USER 2026-11-18)".
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
- **`/isolate none|worktree|docker|off` replaces `/docker` and `/worktree new`
  (plan, 2026-10-27).** The sub-agents' isolation words (internal/harness/subisolation.go).
  `docker` always brings a worktree: a container on the checkout runs the
  commands elsewhere but still edits the developer's files. `off` (= `none`)
  rehoists to `Harness.Home()` (the pinned ConfigDir) and keeps the worktree,
  naming its path; the startup prune removes it only when it holds nothing.
  `/docker [on|off|status]` and `/worktree new` still route, each with one line
  naming `/isolate`, and are not in the command list. Docker missing is
  `exec.LookPath`, as for sub-agents (cmd/dun/isolate.go: dockerProbe).
- **`/isolate docker image <image | Dockerfile | dir>`: a custom image is for
  exec only (USER, aw4 i_1m603tq13c2h6bb67b3c3e).** `--docker IMAGE` re-runs
  the host dun inside the container; only the bundled image holds it (the host
  binary is not mounted and is cgo, so it cannot run on musl). Startup now
  refuses any `--docker` value but the bundled one (`checkDockerFlag`) and
  names this command. `/isolate docker image` shows the image
  (`Harness.DockerImage`, `golang:1.23` until chosen); with an image name it
  sets `Config.DockerImage` (`SetDockerImage`: a session already in Docker
  switches at once; sub-agents with isolation docker read the same field). An
  existing file or a directory holding a `Dockerfile` is built with
  `docker build`, tag `dun-iso-<12 hex of sha256(Dockerfile)>`, skipped when
  the tag exists; the tag covers the Dockerfile text, not the rest of the build
  context. The build runs off the reader goroutine (`isolateBuilds`). Not
  persisted: neither the isolation mode nor the image is stored with the
  session, so a resumed session is back to the default image.
- **A commit message is written by the model and approved by the human (USER,
  2026-08-03).** `/worktree commit` once committed with the literal string
  "/worktree commit". Now one throwaway session (`Harness.CommitMessage`,
  `internal/harness/commit.go`; never appended to the conversation) sees the porcelain status,
  the diff vs HEAD bounded to 24k with `--stat` always intact, and the last
  three user messages as intent. Untracked contents are omitted (a new file is
  the largest, least informative part). The result is shown with
  commit/regenerate/cancel, three rounds. Unlike a recap, a commit message is
  written once and read forever. Default Conventional Commits; per repo via
  `commit.format` / `commit.instruction` in dun.json.
- **A worktree may be deleted when it holds no work, which is not the same as
  a clean tree (`internal/harness/prune.go`).** Measured: 37 registered, 1.1 GB; by `git status`
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

## Sub-agents (`internal/harness/subagent.go`)

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
  `agent` (and `jobs` with its agent params); a child gets `tell_parent` and
  no `ask_user`. Depth 1 holds because a child is never handed `agent`; a child
  cannot reach the human because `ask_user` is absent (`applyTools`).
- **Sub-agents start only on plan nodes (USER, 2026-11-10: "Subagents should
  only be launchable via plan nodes!").** `agent({node, …})` is the model's
  one way to start a child; the free `prompt` is gone, and a call with one, or
  with no node, an unknown node, a finished one, or one a live child owns, is
  refused, starts nothing and says the fix (add a node with `plan_add`, then
  `agent({node:"<id>"})`). The brief is the node's title, details and
  done_when alone, not a prompt on top: one place for the task, which the
  person and a resume can read, and one parameter fewer. One node, one child.
  Children still get no `agent` (depth 1). See docs/taskplan.md.
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
- **`agent({dir})` names where a new child works (USER, 2026-11-17).** A
  parent whose workspace is a non-git overseer directory could not give a
  child a worktree of a repo below it ("isolation worktree needs a git
  repository"), and an unknown `dir` was dropped, so the child worked in the
  wrong place. `dir` is absolute, `~/` or relative to the parent's
  workspace, and any directory ("Any directory"), inside the parent's tree or
  not. With isolation none the child works in it in place (a host shell on
  it; refused when the parent's commands run in a container). With worktree
  or docker the worktree is branched from the HEAD of dir's repo (`git
  rev-parse --show-toplevel`), in that repo's project store, and the child
  works in the same subdirectory of it (`isolation.Sub`); git calls stay on
  the worktree root. A dir in a linked worktree (another child's) branches
  off that worktree's HEAD, but its repo is the main checkout
  (`--git-common-dir`): the store, the merge hint and the release use it.
  A dir not in HEAD (untracked, ignored, empty) is refused, and the tree made
  for it removed. A dir in no repo is refused, naming the repos up to two
  levels below it. A repo not the parent's gets its own mounts, and the merge
  hint says `git -C <repo> merge <branch>`. `dir` is a spawn's only: with
  `id` it is refused unless it is where that agent works. It is kept in
  `<sub>.iso.json`, so a resume goes back to the same directory.
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
- **Sub-agents ask by ending their turn (USER, 2026-10-27).** Sub-agent
  messages go out of band, like dun↔dun ones, which arrive as notifications.
  `ask_parent` blocked the child inside a tool call on an answer channel, and
  that needed its own machinery: a BLOCKED state in the report, the pane and
  the heartbeat, a heartbeat that had to buy a turn, and a 2s poll in
  `jobs(agent:N, wait:true)` to break a parent-waits-on-child-waits-on-parent
  deadlock. None of it is needed when a question is a reply that ends a turn:
  every turn end already notifies the parent with the child's `final`, or
  else its reply, and `jobs(agent:N, tell:…)` on an idle child already starts
  its next turn with its context intact. `tell_parent` stays (status,
  message, final). `ask_parent` is unlisted; it still dispatches, as
  `tell_parent(final: question)` with a result that says to end the turn, so
  an old transcript or a model's habit gets the same out-of-band exchange
  and never blocks. Old transcripts still render `ask_parent` and a BLOCKED
  report.
- **Telling a working sub-agent (USER 2026-11-20: "why not treat it as
  standard session interaction?").** A child takes a message on the
  standard session input path: the main session's engine and a child's
  `tell` go through one gate (`harness.TurnGate`, turngate.go). While a turn
  runs, text is queued (`SayAs`) and lifted after the next tool result or
  read in a turn of its own after the last round; a message with images is
  held and starts the turn after the running one (`AskImages`, as the
  session's); with no turn running it starts one. Images are refused only
  when the child's model cannot take them (`ImagesRefused`, the session's
  check), and the message goes back to the composer (`image_refused`). The
  gate's lock covers both the queueing and a loop's decision to go idle
  (`Settle`), so no message is left between a run's last look and its
  settle; this replaced the child-only `tellMu`/`lastLook`, and closes the
  same window in the engine's router. A queued message after a failed turn
  is delivered and starts a turn, as on the session. A run still bounds the
  turns it takes for news nobody typed (`autoContinues`, 8: a child has no
  automatic-turn budget, autoturn.go), but a queued message is never counted
  and always gets its turn. The ack is `Queued for agent #N…`; the TUI shows
  it until the child's transcript takes the message. The parent's aside
  ("The user told agent #N directly") covers both cases.
- **Persist carries the sub-agents (2026-10-27).** A child has no persist
  setting of its own: it follows its root's. dund passes `--persist` at
  every engine start of a persisted session (`spawnArgs`), as it passes
  `--resume`, and a client cannot. The engine reads it only when it
  restores children (`restoreAgents`): with it, a child that was running or
  idle is restarted from its transcript and isolation, idle; without it, it
  comes back stopped, as before. So toggling persist on the root toggles it
  on its children with nothing to propagate, and no runtime command to the
  engine is needed. A child's state is kept in `<sub>.agent.json` (state,
  model, prompt), written whenever it settles, and not by the shutdown that
  cuts its run: a child running at the stop resumes as one that was running,
  and the root is told its run was cut off. A dismissed child stays
  dismissed. The root is told once, in an aside at start, which children
  came back; with persist, dund's own restart message leaves them out
  rather than call them stopped. A transcript with no sidecar comes back
  stopped. Children come back idle, not re-run: re-running a cut task
  without the parent's word could repeat its side effects. (Superseded for
  a child a dund stop cut mid-run, USER 2026-11-17: dund resumes it with
  its task; see "automatic launches wait for the new build".)
- **A watched child's entries are pushed (2026-10-27).** The TUI's view of
  a child was a pull of its history; it is now the history and then each
  entry its transcript gets (`agent_entry`), while someone watches
  (`agent view` / `agent leave`, a count, so two viewers do not end each
  other's watch). Entry, not token, granularity: the store's append hook
  already sees every entry, and a child's callbacks stay nil, so a child
  nobody watches costs nothing. A viewer that disconnects without `leave`
  leaves the count up; the cost is forwarded events, not state.
- **No `session` tool (USER, 2026-11-06; it replaced `harness` and
  `session_state` on 2026-10-27).** Harness info is a file: `$DUN_HARNESS`
  in exec's environment names `harness.json` in the isolation tmp, rewritten
  at each turn start; the system prompt says so in one line, with no session
  id (cache). What the session is doing is its plan: a fold is told the
  plan's brief, `dun_list` shows its first line, and a resumed session's
  first turn carries the `[plan]` aside. The goal/plan/todo/status store and
  its `.state.json` are gone. `dun session <id> info` and the harness file
  beside the session are unchanged.
- **Attribution is read, not typed (2026-11-16).** `dun session [<id>]
  attribution [--format git-trailer|kv|json]` prints harness, version, model
  and effort from `<session>.harness.json`; an unknown field is an error, a
  dev build's version is `dev`. A sub-agent has its own record
  (`<parent>.sub<N>.harness.json`) and its commands' `$DUN_SESSION_ID` names
  it (`withExecEnv`), so it never reads the parent's model. Commands also get
  live `$DUN_HARNESS_NAME`, `$DUN_HARNESS_VERSION`, `$DUN_MODEL_ID`,
  `$DUN_PROVIDER`, `$DUN_EFFORT` (`$DUN_HARNESS` stays the harness.json
  path), exported before each command in the persistent shell, and shell
  `eval` has a reserved `dun` var with the same facts. Long-lived children
  (MCP servers, jobs outliving a turn) have start-time values: they run
  `dun session attribution`. `/model` and `/login` change the saved default
  for the next session only; a running session's model does not change.
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
  `Harness.applyTools`, never set individually, or the next `/rag` reverts
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
  (`internal/mcpserver/scope.go`, `internal/harness/mcpscope.go`): on/off, mode (standalone, or scripted = hosted in mcpshell
  and called through `eval`), and per-tool switches. Stored mode values stay
  `direct`/`shell` because an older dun rejects any other `via` and would fail
  to load `mcp.json`. A global standalone is stored as `direct`, never as
  absence: with `default_via` (top level of `mcp.json`, set by `/mcp default`)
  scripted, absence means scripted. Session writes go to `<session>.mcp.json` beside the
  transcript. Project and global switch files are separate so a repo can ship
  defaults and a person can override them.
- **Ids were renamed `code`/`docs` to `poly-lsp`/`raglit`.** Old ids are still
  read from config files and the session sidecar (`canonicalServerID`); new
  writes use new ids. Model-facing tool names, raglit index names and the
  `"rag"` config key were left unchanged on purpose.
- **Extensions are manifests (`internal/mcpserver/extension.go`).** An extension is an MCP server
  plus `extension.json`: its server spec, slash commands, roles (`search`,
  `index`), hooks. Special cases (proactive search, memory, skill and rules
  indexes) are lookups by role, not by server id. Bundled
  manifests are embedded (one copy, no drift). Index calls go to the MCP
  `ingest`/`forget` tools when the server has them, else a CLI fallback; when
  raglit adds both, dun switches with no code change. A slash name taken by an
  earlier extension or by dun itself is dropped from the later one.
- **mcpshell is dun's pinned copy under DUN_HOME** (`internal/mcpserver/tools.go`;
  it was looked for on PATH, then beside dun's executable, until "dun manages
  its tools under DUN_HOME" below); absent, dun runs with one tool less and
  says how to install it (`dun doctor --fix`). Installation by `go install`
  was chosen over bundling because it builds from source and is checked by
  sum.golang.org; release builds download the pinned tarballs.
- **Hosted-server shell options replace, not merge** (`internal/harness/shellhost.go`): a later
  layer's `shell` block replaces a lower one's. Bad globs are dropped with a log
  line, because mcpshell exits on one and would take the shell down.
- **Project rules are read like Claude Code** (`internal/harness/rules.go`): root AGENTS.md and
  CLAUDE.md, `@path` imports (relative to the importing file, depth 5, cycle
  safe, an `@token` counts only if it contains `/` or `.`). Identical content
  is included once. `/rules summary` condenses imports with one side call and
  refuses a summary that is not smaller; `/rules index` puts imports in raglit.
  One offer, once per hash, when the block exceeds ~4000 tokens.
- **User rules files are read in every session** (USER 2026-11-10: "does not
  read ~/AGENTS.md or ~/CLAUDE.md!!!!"; `internal/harness/rules.go`). `~/AGENTS.md` (or
  `~/agents.md`), `~/CLAUDE.md` and `~/.claude/CLAUDE.md`, in that order: the
  last is Claude Code's user file, the second is where some keep it instead
  (the owner does), so both are read; the same bytes go in once. They are a
  "## User rules" section before "## Project rules", each file under a
  `### ~/path` header, so the project's rules, read later, refine them. Their
  `@path` imports work as the project's (relative to the importing file) and
  are rendered in the user section in full mode. With the workspace at the
  home directory, its AGENTS.md and CLAUDE.md are the project's and are read
  once. Own budget, not a share: `rulesUserMax` (64 KiB) for the user files
  and their imports together, the per-file 64 KiB cap as before, and the
  project keeps its whole 256 KiB, so a large `~/CLAUDE.md` cannot crowd the
  project's rules out. Past the budget a file is cut with a note and an import
  is "not read: the user rules reached their 64 KiB total". `$XDG_CONFIG_HOME`
  and `$XDG_CONFIG_HOME` is not read (no doc named it a rules file);
  `~/.dun/AGENTS.md` is read as **User defaults** after the project rules,
  in the project's budget.
  Off: `DUN_USER_RULES=0` (`Config.NoUserRules`; a child inherits it with its
  Config, as it does the rules); the Harbor adapter sets it by default so a
  trial does not depend on the image's home. Each user file is also a `/rules`
  switch, so global scope off takes one out of every project.
- **`/rules add <path> [--session|--project|--global]` makes any file a root
  rules file** (USER 2026-11-17; `internal/harness/contextfiles.go`). Kept in the scope's
  `context-files.json` as `"add": [...]` (session when no scope is given, as
  `/rules on`): a session add relative to the workspace when inside it, a
  project or global add always absolute (the project's file is read from its
  worktrees and subdirectories, the global one from every project); `~/` is
  home. Loaded after its group's own roots and their imports: a global add
  counts in the user files' budget, a project or session add in the
  project's; `DUN_USER_RULES=0` leaves the global adds out with the user
  files. A file already in the block (a root, an import; for a global add,
  only the user files and their imports), or with a root's bytes, is not
  added again; a file that is not there is a root row with the note "not
  found".
  `/rules remove <path> [--scope]` takes one back; on/off switch it by name.
  A file whose `@import` was skipped because the target was already loaded
  says so on its row ("includes ~/AGENTS.md (already loaded)"), only while
  that file is read and on.
- **Shared raglit indexes** (`internal/harness/attach.go`): raglit's daemon is shared and its
  watcher keeps indexes fresh; dun only reads. `/rag attach` adds a
  qualified `proj:idx` to the search selector at session, project or global
  scope (union). Attached indexes are read-only from dun. Known risk: with a
  foreign index attached, the selector replaces raglit's default, so the
  project's `shared` namespaces are not read.
- **Images stay out of the session log.** Image parts are written to
  `blobs/<sha256>.<ext>` and the log keeps a reference (`internal/harness/store.go`). A message
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

## Retrieval: dund default, raglit add-on (USER 2026-11-17)

- **dund's index is the project index; raglit is an add-on.** dund
  (`internal/ragd`, raglit's engine as a Go module, reached through
  `RagLink`, `internal/harness/raglink.go`) holds a project's code, docs, memories,
  strategies and sessions indexes in every session, with no `/rag on`.
  raglit (`/rag on`) is for its own document indexes: attached ones,
  include, its UI. Reason: a feature that needs a second daemon, a login and
  `/rag on` is off in most sessions; dund already runs.
- **One `search`.** `search` is dund's (code and docs, `internal/harness/codesearch.go`).
  raglit's search tool is `docs_search` for the model (`modelToolName`,
  `internal/harness/mcp.go`); its server-side name is still `search`. Before, raglit's
  `search` took the name and dund's hits were appended to its answer
  (`withCodeInDocsSearch`, removed): two indexes behind one name, and a
  session's search changed with `/rag on`. The "model-facing names left
  unchanged" rule above holds for every other tool.
- **Folded history goes to dund's `sessions` index**
  (`internal/harness/sessionindex_dund.go`). Each span a fold or a recap removes is written to
  `<session>.folds/<key>.md` beside the log, then synced into the project's
  `sessions` index, tagged `session:<id>` and `kind:<fold|recap>`; the model
  reads it with agentkit's `session_search`, filtered to its session's tag.
  The file is the record and the only copy (a fold rewrites the log), so it
  is written before the index is asked, and each Put syncs the whole
  directory: a span written while dund was down is indexed at the next.
  A failure (dund down, a dund from before `sessions`) is said once as a
  notice, never dropped. Before, folds went to the session's raglit branch
  (`<ns>:sessions@<id>`) only with a keyed raglit and a login, so most
  sessions had none. Branches written then are left to raglit's reaper; no
  dual write.
- **Retention follows the log.** `/close` (`ForgetSession`) removes the span
  files, after `ForgetSessionFolds` removes their documents. A prune
  (`pruneTranscript`) removes only the documents: it keeps a session's
  archives, and the span files are one (the 2026-11-14 rule). `/clear` keeps
  the session id, so the spans of the cleared log stay with the session.

## /plan summary and /journal (USER 2026-11-17)

- **`/plan summary` (`s` on the /plan screen) is one side call, beside the
  turn** (`internal/harness/plansummary.go`). The session's model reads the plan
  (`Graph.List`) and the recent session (the newest entries, each cut at 800
  runes, 12,000 in all: `tailLines`) and answers in three parts, `## Done`,
  `## Doing`, `## Next`. It goes through `askOnce` (`sidecall.go`), which takes no
  turn lock: a running turn is neither interrupted nor waited for (`fork`
  takes `turnMu`, so it was not used). Counted as side call kind
  `plan_summary`; the spend meter sees it as every call. The last summary is
  in memory and rides the `plan` event (`proto.PlanSummary`: text, when made,
  tokens, cost, running), so every client shows the same and the TUI shows
  "summarizing…" while it runs; a second request while one runs is refused.
  Not kept across a restart: it is a reading of the moment.
- **`/journal` is a time-ordered list of what the agent did**
  (`internal/harness/journal.go`), kept in `<session>.journal.jsonl`,
  append-only, so it survives a resume. Three kinds: `fold` (every compaction
  fold, from the summary the fold wrote: `lateSessionIndex.Put` adds the entry
  as a reference to `<session>.folds/<key>.md`; with no dund link there is no
  file and the entry keeps the summary; the rescue fold, which writes no span,
  does the same; recaps are not entries, they are many and small), `now`
  (`/journal now`: one side call, kind `journal`, over the plan and the
  session's entries since the last journal entry, bounded like the old
  `/worklog write`; its text goes to `<session>.journal/<n>.md` and into dund's
  sessions index as `kind:journal`, so `session_search` finds it), and
  `worklog` (an account `/worklog write` wrote, by reference). No extra model
  call for folds. `/worklog` and the `worklog` kind were removed (2026-11-20).
- **Reason.** A fold deletes from the context what it summarizes; the
  summary was on disk but no view listed them in order. The plan says what
  is planned, not where the work stands; a summary on request answers that
  without a turn.

## Idle plans: the suggestion call records done work (USER 2026-11-17)

- **The idle suggestion call also returns what the last turn finished that
  the plan does not record, and those become done plan nodes**
  (`internal/harness/suggestdone.go`). One request, not two: the
  instruction gets a `"done"` part and the plan as text, after the history,
  so the turn's cached prefix holds as before. Still side call kind
  `suggest`.
- **Marked as the suggester's.** taskgraph's node has no author field and
  agentkit is not changed for this: dun keeps `by` beside the graph in the
  sidecar (`planFileData.By`, id → `suggest`), as it keeps `Archived`. It
  reaches every client as `PlanNode.by` on the `plan` event; the TUI shows
  `auto` beside the ✓, `/plan` text `[done] (auto)`. The model's own
  `plan_show` does not mark it: the agent sees these nodes as any other and
  may edit them.
- **Under the node the model names** (`under`): the new node `needs` it, the
  graph's own "under" (as a child adds under its node). An id the visible
  plan lacks puts it at the top level.
- **Only adds, and only when safe.** It never closes, edits or reorders a
  node. Nothing is added when the plan changed since the turn began
  (`planMemStore.rev`, noted in `prepareTurn`; a user edit while idle counts
  too), when the plan is empty and the turn made no tool call
  (`sessionStore.toolCalls`), when no turn ran in this process (a resumed
  session), when a turn began during the call, or when the call is off
  (`--no-suggest`: `emitSuggestions` never calls `Suggestions`). A second
  call in the same idle adds nothing: the first one's nodes moved the plan.
- **Bounded.** At most 3 nodes per call, titles cut to 60 runes, none whose
  title (case and spaces aside) the plan already has, archived nodes
  included. A malformed done part adds nothing; `parseSuggestions` reads the
  object again without it, so the suggestions still arrive.
- **Reason.** A turn that did the work without touching the plan leaves the
  plan behind what happened; the idle call already reads the whole turn, so
  recording it there costs only the extra output tokens.
- **Risk, known.** `prepareTurn` runs before each attempt of a retried
  turn, so a plan change in a failed attempt is not seen as the turn's.

## Asks and the turn ledger (USER 2026-11-17)

- **Each message the person types is an ask: a top-level plan node.**
  Reversed 2026-11-18: asks are dun's own ledger, not plan nodes or plan
  roots, and a message grows the intake instead of opening an ask ("Asks
  are acknowledged and folded" below). Not on the intake: a message that
  starts with a bracketed lowercase tag (`[automatic] …`, `[control/plan]
  …`, `[settle] …`: what dun or dund sends in the person's place; a person
  who starts a message with `[wip]` is misread, accepted), a sub-agent's, a
  `--non-interactive` run's (nobody reads the asks; the benchmarks' turns
  stay as they were), a session without the asks tool, and `Config.NoAsks`
  (embedders, and tests of other things).
- **The turn-end check raises a forgotten ask.** Changed 2026-11-18: the
  check names the intake, the touched asks not marked, and once an ask long
  untouched ("Asks are acknowledged and folded" below). The plan nudges
  count every open plan node again: no plan node is an ask.
- **The turn ledger: facts from the turn's own tool events, no model call**
  (`internal/harness/ledger.go`). `withLedger` is the outermost dispatcher
  wrapper, in the root and in each sub-agent (a child's facts carry its
  number). Files: `file_edit`'s own `+N -M`; `file_write` and `file_delete`
  diff against the file read before the call (Myers line count, exact up to
  4,000 edits); `file_move` names both paths. A file a command changed is not
  seen: a workspace diff per turn would cost a git run over the tree each
  turn, and the tool events are exact for what they cover. Commands: exec's
  first line, exit (from the `ExecResult`, `markExecLedger`), duration; one
  that outran its grace is `job #N still running`, and the job's end is
  recorded when it comes (`bgJob.finish`). Sub-agents: started
  (`spawnAgentFor`) and finished with done/failed/dismissed (`settleThen`).
  Tool errors: a failed call other than an exec that ran. Commits: each
  watched tree's HEAD (the root's and each child's own worktree) at the
  turn's start and end, `git log --first-parent` between (a merge is one
  commit; a child's tree new since the start counts from the root's start
  HEAD, which it branched from). Totals: the round stats' tokens and cost
  for the turn (sub-agents' included, as `roundstats.go` counts them), the
  rounds, the tool calls, the wall time, interrupted, and the turn's error.
  Every list stops at 50 with `… +N more`; the totals count everything.
  Events between turns (a job ending while idle) go to the next turn.
- **Facts go to the asks open during the turn** (open as it began, or
  made in it). When the turn names asks (the asks tool acted on them, or a
  plan change touched a node that serves one or descends from one: `needs`
  followed up), the facts go to those asks only; otherwise to every open
  ask, and the turn is marked shared on each. Chosen as the simplest rule
  that uses what the agent already says; a turn that touched nothing cannot
  be split honestly, so it is counted in full in each. A turn with no ask
  open and a tool call is its own `/journal` entry, kind `turn`, indexed as
  `kind:ledger`; one with no tool call gets nothing.
- **An ask's `/journal` entry** (kind `ask`, indexed as `kind:ask` so
  `session_search` finds "which ask changed X") is written at the end of the
  turn in which it is closed (the asks tool marks it done or filed; the
  turn end reads the ledger), with its text, outcome and note, the
  turns that served it (numbers, times, shared, interrupted) and the facts
  summed. Row: `ask 3 · done · "fix the flaky test" · turns 12–15 · 6 files
  (+120/−34) · 1 commit · 11 cmds (1 failed) · 9m` (the time is the turns'
  wall time summed). An open ask is listed after the kept entries as open
  with its facts so far; when the session ends (`Close`) with it open, it
  gets one entry, `open at session end`, once.
- **Kept in `<session>.asks.json`** with the session's turn counter, so a
  resumed session keeps its asks and numbers its turns on; `/close`
  (`ForgetSession`) removes it, and the journal and index entries go as the
  others do. The idle suggestion call's done nodes name the turn
  (`recorded after turn N by the idle suggestion call`): the ask or turn
  entry of turn N holds its facts.


## Asks are acknowledged and folded (USER 2026-11-18)

"There should be a single unacknowledged ask that grows as the user adds to
it." A real session listed 14 asks, all `working`: "continue", "resume",
"retry", "rety", "test" each opened one, none closed, and every open ask
spun. Then: asks are a ledger, pinned in the prompt, not plan roots.
`internal/harness/asktool.go` and `asks.go` unless named.

- **Asks are a ledger, not plan nodes.** This reverses "each message is an
  ask: a top-level plan node" (2026-11-17). `<session>.asks.json` holds the
  intake and the asks: a number, a title, the text, the person's words
  (`source`, `continued`), a state (open, blocked, done, filed) and the plan
  nodes that serve the ask (`nodes`). The plan graph has no ask nodes; a
  plan node carries its ask only as that link, shown on /plan as a tag
  `ask N` (chosen over grouping nodes under asks: one word per row, and the
  tree stays the plan's own).
- **One intake.** A message the person types goes on the intake, the one
  ask not acknowledged. No word list: "continue", a typo, a pasted list of
  four requests all grow it. Rejected: matching control words and typos by
  edit distance (USER: "kinda dumb"), and one `new` ask per message merged
  away later (14 rows again until the model acts).
- **The `asks` tool**, one call of several items, at any point in the turn
  (its schema flattened 2026-11-19: id 0 is the intake, split is a list of
  titles; see "The person's message ends the prompt"):
  on `intake`, `split` (asks with short titles, each with its part of the
  text), `merge_into` an ask (the intake's messages go to its `continued`,
  word for word; `text` is a note on them, never in their place; a closed
  ask reopens: the person carried on with it), or `state` alone (one ask in
  that state); on an ask (its number), `title` (the text stays), `nodes`
  (links), and `state` `done` (note), `filed` (nodes), `blocked` (the
  question: what ask_user's refusal tells an automatic round to do) or
  `open`. Messages that arrive during the call stay on the intake. 911
  bytes of tool def (was 1,816). Not offered to sub-agents: the tool is
  listed where `asksWanted` holds (root, interactive, not `NoAsks`), and
  `asksOn` is that plus the tool in the set, so the two cannot differ.
- **Pinned in the prompt.** The model reads the intake (its newest 5
  messages, 200 runes each, and `+N earlier`) and the open asks (up to 12,
  number and title; done and filed not listed) in one block, `Open asks:`:
  an aside keyed `asks` as each turn begins (beside the plan's line,
  `planTurnLine`), and in what a fold keeps: the fold's ask is the ask the
  turn works, with its newest 3 merged messages and the intake's newest
  (`+N earlier` past that), remembered for the note after the fold and the
  summary check (`askNow`), and the note after a fold prints the block. One
  system line (300 bytes): acknowledge the intake, mark each ask done as it
  finishes, and file one not finished this turn (deferred, waiting, a list
  of later items) as `plan_add` nodes marked filed with them, so nothing
  asked is lost (USER: "keep track of delayed user asks with the plan
  tool"). Cost per request: the 300-byte line and 911-byte tool def, against
  1,816 bytes of tool def before; per turn the block (175 bytes for one ask
  and an intake of 8 messages), against the plan board's line per ask node
  and the check's 300-rune quote of each open ask.
- **The turn-end check** (`idleAsks`, in `idlecheck.go`'s note), in a turn
  the person started only: the intake while it has messages, each ask the
  turn touched (by the tool, or a plan change to a node that serves it)
  and did not mark in the turn, and, once, an ask left open and untouched
  for 5 of the person's turns (`still open: N …` — file it, mark it, or keep
  it). Nothing when there is nothing. 123 bytes for an intake of one
  message, about 25 plus the title per ask.
- **Automatic rounds** never grow the intake (not the person's). They may
  use the tool, unprompted: acknowledging and closing only lower the open
  asks, which tightens the automatic-turn budget ("Unrequested turns").
  Only the intake and an open ask the person's last 5 turns touched hold
  the budget off; an ask left open stops counting, so an open ask never
  disables it for good. The network wait resumes the intake (one id per
  intake) and the open asks not blocked, each counted on its own.
- **Shown.** dund's plan event carries the ledger beside the rows (`asks`,
  `intake`; a row's `ask`). Only the ask the turn works (the intake until
  acknowledged, then the first open ask the call made or merged into) is
  `working`; the TUI spins it while a turn runs, other open asks show
  `open · <age>`. /plan lists the asks on top: the intake as one row,
  `▸ new · 3 messages · "the tools and stubs dir…" +2`, the open asks, and
  done and filed asks in one row, `▸ N done · M filed`, that opens. /journal
  folds done and filed ask entries the same way (the entries' `outcome`).
  Mobile renders the same; web is the TUI's pty; `dun ps` and `dun_list`
  show the newest open ask (`AskBrief`) before the plan's first line.
- **Old sessions** are moved into the ledger on first use (`asksMigrate`):
  each ask node leaves the graph, its state goes to the record, a node that
  needed it serves it, and closed ones get no second entry. Chosen over
  leaving them as plain nodes: those would stay 14 `ask:` roots nobody
  closes.
- **"Current ask: continue"** was the fold's node (`fold.go` `planAsk`): one
  plan node holding the latest user message, which a fold copies into its
  summary. Where asks are kept the fold takes the ask from the ledger and
  writes no node; an open one from before is dropped.

## dund's HTTP listener: runtime, one listener, chooser (USER 2026-11-17)

- **dund starts and stops its HTTP listener while it runs.** `dun dund
  enable web [--addr host:port]` / `disable web` reach the running dund
  over its socket (`proto.WebSet`, `internal/dundcli/web.go`); with no dund
  running the change is saved and "applies when dund next starts". Default
  address `127.0.0.1:20003`. Reason: the listener was fixed by a flag in
  the unit, so turning it on meant editing the unit and restarting every
  session.
- **The saved state wins.** `<home>/dund/web.json` (0600, atomic write:
  `{enabled, addr, tui}`) decides at every start once it exists.
  `--web-experimental` / `--http` only seed it while it does not;
  `disable web` stops a listener the flag started, and the flag does not
  bring it back. `service install` writes no such flag into a new unit
  (it seeds `web.json` instead); an old unit's flag still seeds. `serve
  --check` binds the address `web.json` names. A busy port refuses the
  change and leaves the old listener serving.
- **One listener, three ways in.** `/` is a chooser: Terminal (`/tui`) or
  Web app (`/web`, marked experimental). The choice is kept in a cookie
  whose value is only `tui` or `web`, so `/` redirects to one of two fixed
  paths (no open redirect); `/?choose=1` offers it again. `/web/*` is the
  spa with the prefix stripped; the API stays at root (`/api/*`,
  `/auth/*`, `/releases`, `/dun.apk`, `/healthz`) because the native app
  uses `baseUrl + /api/...`.
- **The TUI in the browser is a separate opt-in** (`enable tui` /
  `disable tui`) on the same listener; off, `/tui` is 404 and disabling
  closes the open tabs. Each tab runs the dund's own `dun --tui` in a pty
  (`internal/webterm`), in `$HOME`, with `DUN_HOME` set to the dund's home.
  A TUI on a non-loopback address is allowed, with a warning: it is a shell
  as this user for whoever signs in.
- **Every page needs a sign-in; the browser holds a session cookie.**
  `/auth/signin` takes a token (local, named, or a sign-in token: AUTH-2) and
  sets an HttpOnly, SameSite=Strict, Secure-over-TLS cookie whose value is a
  new sign-in token tied to that bearer by hash: a new `dund token`, a
  `token revoke` or a sign-out ends the sessions made from it. The OIDC
  callback sets it too. A redirect at the end of a cross-site chain is
  cross-site, so the browser withholds a Strict cookie: the callback lands
  with a page of its own (`hop`). Open: `/auth/*`, hello, `/healthz` and the
  releases (the app opens `/dun.apk` with `Linking.openURL`, which carries
  no credentials). A cookie request that changes state must be same-origin.
- **The cookie belongs to one host name, never a configured domain**
  (USER 2026-11-17). A cookie is sent to every port on its host, so: no
  Domain attribute; each session is bound to the host it was made on, and
  is refused under another (no replay through 127.0.0.1); `__Host-` and
  Secure over https. `X-Forwarded-Proto` is believed from loopback (hz,
  cloudflared) or `web.json`'s `trusted_proxies`; `X-Forwarded-Host` only
  from `trusted_proxies` (cloudflared passes a client's through). Origin
  checks compare scheme and host. Plain http on a shared loopback name goes
  to `dun.localhost` before sign-in (not when a proxy on the box forwarded
  it); this stops ordinary local servers, not a hostile local process
  (https + `__Host-` does). A bare LAN IP is allowed with a warning. See
  sessions-and-dund.md, "Reaching the web listener".
- **The pty WebSocket checks the cookie and the Origin.** `CheckOrigin`
  returning true (the old `--web-tui`) let any page the user visited open a
  shell. It is a second realtime endpoint beside RT-1's `/api/graphql`: a
  byte stream to a pty is not a GraphQL subscription (caveat proposed with
  this change).
- **Every exit kills the ptys first.** `http.Server.Shutdown` does not close
  hijacked connections, and `/tui`'s are hijacked: stop, re-exec and the
  service handover call `webterm.Close` before the listener stops.

## Pair a phone by QR (USER 2026-11-20)

`dun dund enable web --url https://<public host> --token` prints a QR code;
the phone scans it and is signed in. `internal/dund/pair.go`,
`internal/dundapi/pair.go`, `internal/dundcli/qr.go`.

- **`--url` is saved** in `web.json` as `public_url` (scheme://host, no
  path); later `--token` calls omit it.
- **The code**: 160 random bits, kept only as its SHA-256 in the running
  dund's memory (a restart ends every code), 10 minutes, one use,
  compared in constant time. Minted over dund's socket (`WebSet.Pair`),
  refused while the listener is off; `disable web` ends every code. 20
  wrong codes in a row end every live code. Used and expired codes get
  their own error for an hour.
- **The link** is `<public_url>/auth/pair#code=<code>`: a fragment, which
  the browser never sends, so neither the tunnel nor dund logs the code.
  The page's script moves it into a form, and the person taps Sign in:
  a link previewer that runs scripts must not spend the code. `/auth/pair`
  is a dund page, not the web app's: the web app signs in by dund's
  cookie only (AuthProvider), so the exchange lands where `/auth/signin`'s
  does.
- **What it makes**: the session cookie `/auth/signin` sets (7 days,
  bound to the host), minted from a new named API token
  `paired-<date>-via-qr-<hex>` whose secret nobody keeps. That token is
  the device's handle: `dun dund token list` shows it and `token revoke`
  ends the session. Named tokens allow no spaces, so not "paired <date>
  via QR". After 7 days the phone pairs again; the handle stays listed
  until revoked.
- **Threat model**: whoever opens the link first, within 10 minutes, is
  signed in as the owner, with the TUI too when `/tui` is on. The QR is a
  credential on screen: anyone who sees or photographs it in that time
  gets in. A quick tunnel URL (`*.trycloudflare.com`) is public and
  unauthenticated; dund's sign-in is the only gate on it. Plain-http
  public URLs are allowed with a warning: the code crosses in clear.

## Automatic token spend: guards and measurement (USER 2026-11-18)

From the 2026-11-18 inventory of model calls nobody typed for.

- **A shaper fold is side call kind `fold`** (`internal/harness/fold.go`,
  `foldRunner.call`). Each summarizer call counts: a fold stage is up to two
  (the first and its retry). The rescue fold keeps kind `rescue`. Before
  this, folds were the one automatic call /context and the usage event did
  not show. `collectText` now reads to the stream's close after the finish,
  as `askOnce` does, because the usage can arrive in its own chunk after it.
  Measurement only: what the Shaper receives is unchanged.
- **A fold shares the session's prompt cache** (agentkit v0.33.1
  `Shaper.SessionPrefix`; `internal/harness/fold.go` `sessionPrefix`, set in
  `measuredBuild`, `internal/harness/shaping.go`). A fold that fits one request
  sends the round's own system (the string agent.Session passes to Build:
  heredoc tool text and slot note included), the round's tools
  (`forkTools`: none on heredoc) and the round's PromptCacheKey, then the
  history and the summary instruction. Set at every build, so a server
  starting or `/rag on` reaches the next fold. Before, a fold read the whole
  history uncached under the compaction worker's prompt (the 85% of a
  fold's overhead in `compaction.md`). Since agentkit v0.34.0 a fold too big
  for one request is split: its first request carries the prefix over the
  longest head of the history that fits beside the session's system and
  tools, and the later stages keep the worker prompt and send no tools and
  no key. `foldRunner` knows every fold request by `agent.IsFold(ctx)`, not
  by its system (the first request's system is the session's). Limits:
  only the first request of a split fold can match the cache; pinned
  entries are left out of the fold
  request, so with `PinLatestUser` the cache matches up to the latest user
  entry; the fold sends thinking off, which some templates render
  differently. Proof: `TestFold_SharesTheRoundsPrefix` (a whole fold and a
  split one); no live measurement yet (side-call kind "fold" records the
  cached tokens).
- **A fold keeps the newest entries up to 55% of target (USER 2026-11-18:
  fold only to ~55% of target)** (agentkit v0.37.0
  `ShaperPolicy.FoldKeepPct`; `foldKeepPct` in
  `internal/harness/shaping.go`). Before, a fold kept only the pristine tail
  and left the window nearly empty, so the next fold came soon and each one
  paid a summarizer call. Now the newest entries stay verbatim as long as the
  rebuilt prompt (prefix, a summary at its cap, pinned and kept entries,
  without LOD) fits 55% of target; only the oldest are folded. The pristine
  tail is the minimum. The fold notice says what was kept: `compacted:
  oldest 237 entries → summary (6.2k), newest 198 kept (141.6k) · history
  295.0k → 147.8k tokens …` (sizes since agentkit v0.39.0); the arrow
  ends at `TokensAfter`, what the next build sends (since agentkit v0.38.0
  the count without LOD; v0.37.0's `TokensAfterFull` is gone). Proof: `internal/harness/foldkeep_test.go`
  (the simulated 434-entry session sends 51% of target; little history and
  a huge newest result fold as before).
- **A refused reasoning off is agentkit's to resend** (agentkit v0.34.0;
  `internal/harness/reasoningoff.go`). A thinking-only model refuses off
  (OpenRouter: "Reasoning is mandatory for this endpoint and cannot be
  disabled"). agentkit learns `reasoning.thinking_only` from the 400, resends
  once with every dialect's off form rewritten to the lowest effort, and
  sends that effort from then on; dun keeps its default resend on
  (`NoResendOnRefusal` unset), and stores the fact through `OnLearn`
  (`capstore.go`), so a new client starts with it. dun's own resend
  (`offGuard`) is gone: with both, a resend that failed again could cost a
  third request. dun keeps the one `/effort none` (off) note. Since
  agentkit v0.35.0 the role resolver sends the lowest level from the start
  once the record says thinking-only ("Roles and model profiles"). Proof:
  `TestReasoningOff_*` (fold, rescue fold, `/effort off`; llama.cpp named and
  detected, OpenRouter) passed with the guard disabled before it was
  removed.
- **Heartbeat escalation is bounded** (`internal/harness/asidettl.go`,
  `heartbeatOpts`). A job's or child's "still running, quiet" heartbeat is
  an aside that escalates to a wake when no turn carried it in 5 s. Before,
  that wake repeated for every heartbeat for as long as the job ran, so a
  job nobody watched bought one model call per beat (hourly at the end of
  the backoff, forever). Now: `--aside-escalate-ms 0` (or
  `DUN_ASIDE_ESCALATE_MS=0`) turns escalation off (it meant 5000 before);
  one job or one child escalates at most 3 times (counted when an aside
  actually becomes a notification, through `AsideOpts.OnEscalate`), then
  its heartbeats stay asides; an IDLE child's heartbeat never escalates,
  because it waits for the parent and is not wedged. Only a running, silent
  child's does.
- **Job progress reports have a floor and an hourly cap**
  (`internal/harness/jobs.go` `tuneJob`, `internal/harness/bgjob.go`
  `sendReport`). Each report is a notify, so a wake: `every_secs:5` on an
  hour-long build was 720 model calls. `every_secs` under 60 is raised to 60
  and the tool result says so (0 stays off). One job's reports (by bytes or
  by time) wake the agent at most 12 times in an hour; past that they are
  asides, carried by the next turn that runs anyway, until the person's next
  message (`Harness.personMsgs`, counted in `SayAs` and `AskAs`). The count
  is of reports sent as wakes, not of turns: several may share one turn, so
  the cap is the stricter reading. A job's finish is never capped. The
  settle prompt (`settle.go`) no longer tells the model to set a report
  rate when it waits on a job: the finish wakes it, and reports are for
  progress it must act on before the end.

## Unrequested turns: budget, no questions, bounded checks (USER 2026-11-18)

"Don't stop work on asks; stop senseless unrequested work; auto done
limited." `internal/harness/autoturn.go` unless named; docs/attention.md,
"Unrequested turns".

- **The automatic-turn budget.** A turn's origin is the person's, user-set
  (a `[loop …]` / `[scheduled …]` firing) or automatic (job finish or
  report, heartbeat escalation, child report, server wake, peer wake, the
  settle, the upgrade's `[automatic]` continue). While an ask node is open
  (todo or doing) automatic turns run as before. With none open, at most 3
  start per rolling hour; past that the wake's news is written into the
  conversation and claimed (`Continue` returns `ErrTurnHeld`, the engine
  sends done `held`), counted as notes, and shown `idle: N notes waiting`
  (turn_state `notes_waiting`; TUI status line, web header, `dun ps` row).
  The person's next message (`AskAs`, `SayAs`, a `dun msg` from a shell)
  resets the count; the notes are in its context. Only where asks are kept
  (root, interactive, plan tools, not `NoAsks`).
- **Loops and schedules: chosen exempt.** The person set them, so a firing is
  never held and not counted; but it is not their message now: not an ask
  (asks.go: `[loop`/`[scheduled` prefixes, which the bracket-tag rule missed
  because of the `,` and `:` in them), it arms no settle, it does not reset
  the budget, and it does not count as the person for the job-report cap
  (`personMsgs`) or the idle suggestion.
- **No ask_user in a round dun added**: after the turn-end check, the round
  right after a plan nudge or stall, and an automatic turn with no open ask.
  Refused with a tool error (block the ask with the question as the reason),
  not withheld: a tool set that changes per round re-reads the prompt. A
  blocked ask is not open for the budget nor the check; the plan views show
  its `why`.
- **Turn-end check** (idlecheck.go): skipped in an automatic turn with no
  tool call; blocked nodes not listed; the open nodes not listed while the
  plan's revision equals the last check's (any turn); the count is the
  turn's (`turnBegin`), not agentkit's per `Session.Turn` call, which a retry
  (retry.go) started again. Known effect: an interactive turn whose plan did
  not move gets no second check after a file edit.
- **Done nodes from the idle call** (suggestdone.go): at most 3 per idle, and
  one batch per set of open asks (`doneAsks`): a second idle with the same
  asks open adds none. In memory, not persisted.
- **Settle** (settle.go): armed by the person's turn only.
- **Idle suggestion** (suggest.go `IdleSuggestions`): one call per idle in the
  engine, whichever client asks (a mutex over the call; a later ask gets the
  same answer); none after a turn the person did not start until a presence
  ping.
- **Peers** (dund peer.go): 20 wakes an hour per (sender, target), as a
  server's (serverwake.go); and the reply that closes A → B → A, with no
  person's message in A or B since A's (dund counts user echoes that pass
  `harness.PersonText`), is delivered with wake false. Past either, delivered
  quiet with `PeerReply.Unwoken` saying why. Chosen literally: the reply
  itself is quiet, so A reads B's answer at its next turn.

## Continue when the network returns (USER 2026-11-18)

A laptop loses and regains network while dun works. Before, a provider
outage past the retry budget (request scope 5m, then the turn scope,
retry.go) ended the turn, and nothing ran again when the network came
back. `internal/harness/netwait.go`.

- **Only a network fault.** `NetworkDown`: refused, reset, DNS failure,
  timeout, no route, network down, the client's "of transport failures"
  give-up. Not a 4xx, a 402 budget, a 429, a 5xx or "no backend" (the
  provider answered), not the turn's own deadline, not a model error.
  Classified on the turn's final error, not on a `giveup` note: with
  `DUN_TURN_RETRY_BUDGET=0` the turn ends with no note.
- **Only with an ask open** (todo or doing), only where asks are kept
  (`asksOn`) and with `Endpoint.URL` to probe.
- **The probe** is an HTTP GET of `<base>/v1/models` (5 s timeout): any
  response, a 401 included, is the network back. No tokens. Through
  `http.DefaultClient`, so a proxy from the environment is used. Backoff
  5 s doubling to 60 s. No OS network-change hint: not portable cheaply.
- **One continue per give-up**: a notification `[automatic] the network
  was down from 14:02 to 14:19; continue: <ask title>` (asks joined by
  `; `) and a wake. It is ask work: the automatic-turn budget allows it.
  "Down from" is the first transport or interrupted retry note since the
  last success (the request's own elapsed time subtracted), else the
  give-up. A resumed turn that fails the same way waits again.
- **Cap**: 12 resumes per ask in a rolling 24 hours. With every open ask at
  the cap the wait does not start; a turn-scope `giveup` retry note says
  so ("send a message to continue").
- **Cancelled** by the person's message (`AskAs`, or a `Continue` the
  person's queued message makes theirs) and by any turn that succeeds.
- **Persisted** in `<session>.netwait.json` (waiting, since, resume
  times): a restarted engine with the flag and an ask still open waits
  again (end of `Start`). dund's restart does not touch the wait: the
  engine runs on (engine adoption).
- **Shown** as turn_state `net_wait {since, next}` (Unix ms), kept across
  the turn's edges like `notes_waiting`; TUI idle status `waiting for
  network · since 14:02 · next try 30s`, web/app header the same, `dun ps`
  row `(waiting for network since 14:02)` (ProcSession `net_wait_since`).
- `/retry` settings still apply: an unbounded budget never gives up, so
  this path is not reached.

## File access asks in the turn (USER 2026-11-18)

This reverses "Isolation, not approval prompts" (Worktrees and ship) for
paths outside the workspace. The file tools refused such a path and told the
model to ask for `/file add`; the model went to exec instead. Evidence: 17
exec edits followed an "outside the root" refusal. `internal/harness/fileaccess.go`.

- **When.** A file call (`file_read`, `file_edit`, `file_write`, and the
  opt-in tools that take a `path`) names a path outside the workspace that
  no scope opens, or one open for reading only when the call writes.
  Absolute paths, `..` escapes and symlinks whose target is outside are
  all resolved to the real path first, so a `..` reaches a grant too.
- **The question** goes through the session's Ask (the `ask` event: TUI,
  web and mobile; the clock is paused while it waits). Options: yes for
  this session, this project, everywhere, or no. An answer in the person's
  own words is a no, and the words go to the model as the reason.
- **A yes** opens the path the call named, the file or the directory, at
  that scope (`AddPath`), for writing when the call writes. It is not
  indexed: `/file add` indexes a directory by default, but raglit watching
  a tree is not what a file call asked for (`/file index` turns it on). A
  read-only grant gets mode write at that scope (`SetPathMode`). The
  chosen scope decides: the nearer scopes forget the path's on (and, for a
  write, its mode), else a session's read would hide a project's write
  (`clearNearer`). The call then runs once and its result is the answer;
  a note after the option (the TUI's `<option> — <note>`) rides with it.
  A move asks about its `to` as well.
- **dun's own session files are readable without a grant.** The isolation
  tmp (`dun-<id>`, job logs, clipped output), the shell's overflow dir, and
  a sub-agent's ancestors' are open to read, grep and list with no question
  (`sessionReadDirs`, files.go); writes ask as above. Another session's
  `dun-*` is not among them, and neither is the rest of the temp dir: it
  was read-open to read, grep and list since USER 2026-10-23 (scratch
  output, a test's log, a command's dump that exec just printed; commit
  4b54c0c), which let a session read other sessions' `dun-*` dirs. (USER
  2026-11-18: narrowed to the session's own dirs; other temp paths ask.)
  A path such as `/tmp/x.log` now asks like any path outside the
  workspace, and with nobody to ask is refused with `/file add`. Paths are
  resolved before the tmp check: macOS's `/var/folders` link made the job
  log dun cited "outside this workspace".
- **A file's mode is its own.** A directory's grants used to share one
  mode, so one file open for writing opened its read-only neighbours;
  each file now keeps its own (`addedSets.writable`).
- **Nobody to answer.** A sub-agent and `--non-interactive` have no Ask:
  the refusal is as before, with the exact `/file add <path> [--write]`.
  In a round dun added (`autoRound`: automatic turns, which include a
  peer's wake, the turn-end check, the round after a nudge) the refusal
  also says to block the ask with the command as the question: the same
  text as ask_user's refusal there (`askBlockHow`).
- **A read-only workspace keeps its edit tools.** agentkit's `ReadOnly`
  dropped `file_edit` and `file_write` from the list, and the model edited
  through exec with nothing to say why. They stay listed and refuse with
  `/file mode . write`; the list no longer changes with the mode.
- **Several edits in one call, builds apart.** 18 exec edits were the model
  making several replacements and running `go build` in one python or sed
  call. `file_edit` already takes `edits: [{old, new}, …]`; the prompt line
  says to use it and to run builds and tests in an exec call of their own.
- **mcpshell's Files roots follow file-access (USER).** mcpshell v0.15.0
  refuses an absolute path outside its roots, so a grant the person gave
  the file tools did not reach `Files.*` in eval. dun now declares the MCP
  `roots` capability (`listChanged`) to an mcpshell whose build info says
  v0.15.0 or newer, passes `--files-client-roots`, and answers `roots/list`
  with each grant that is on and `sessionReadDirs` read only
  (`internal/harness/shellroots.go`). A write grant carries `_meta
  {"mcpshell/access":"write"}`; mcpshell reads any other root as read only.
  Every edit of a paths file sends `notifications/roots/list_changed` when
  the list differs from the one last answered. An older or unversioned
  build (`(devel)`, `v0.0.0-…`) gets no flag and no capability. Limits:
  the shell is a built-in, per session and never shared through dund, so
  roots need no per-client split; a sub-agent's shell is its root
  session's, so its roots are the root session's grants, not the child's.
  A grant changed by another session at project or global scope reaches
  this shell on its next `list_changed`, not at once. The workspace is not
  listed: `--files-dir` already makes it a read-write root, and a second
  entry showed twice in mcpshell's refusal. dun's read-only workspace mode
  does not reach mcpshell (as before).

## Tool calls sent as text are retried (USER 2026-11-18)

A DeepSeek Flash session wrote its tool calls into the reply as DSML
(`<｜DSML｜…<invoke>…<parameter>`, U+FF5C bars) with no tool_calls: nothing
ran, the turn ended, and the TUI dropped the tags as HTML. Once the markup was
in a plan node's text. `internal/harness/textcall.go`,
`internal/textfmt/toolmarkup.go`.

- **Detect structure, not a tag.** A reply with no tool calls whose text,
  outside code (fenced, also in quotes and list items; indented; inline),
  has a DSML marker (`<｜DSML｜`, `<|DSML|`) or `<|tool▁calls▁begin|>`;
  `<invoke name=` with `<parameter name=`; `<function_calls>` with
  `<invoke`; or `<tool_call>` then `{` and `"name"`. A tag named in prose
  ("Qwen wraps calls in <tool_call> tags") is not a call.
- **Respond.** onIdle answers first with one `[automatic] your last reply had
  a tool call written as text (…)` note and the turn goes on; at most 2 a
  turn, then the turn ends as before (with its usual turn-end check). Only a
  reply written in this turn is read: the newest entry as the turn began is
  skipped, so a wake with an empty reply does not correct the last turn's.
  It runs in automatic turns too, but as a note inside the running turn,
  not a new turn: the unrequested-turn budget does not count it.
- **Plan arguments.** A plan tool call with the markup in any string argument
  is refused with the same guidance: plan text is shown to the person and in
  the turn-end check. File contents are not checked.
- **Display.** The TUI fences each markup span (its lines only, a
  parameter's value kept whole) before rendering, so it shows raw as a code
  block. In a blockquote or an indent the fence lines keep the prefix; a span
  that opens on a list marker line is left as it is. The app (mobile/web) draws HTML nodes as their text
  and was not changed.
- Not a gateway fix (Carl, 2026-11-18): the markup is malformed model
  output (mismatched tags), which no parser can repair; corrallm passes it
  through. The guard is the fix.

## Mid-turn messages are answered first (USER 2026-11-18)

A person typed "What is your goal here? what are you doing?" during a turn,
and later "IT's weird that you don't stop and respond to me?". The model
(Gemini via OpenRouter) never answered: it read the words only as
`[user] …` inside a tool result, under the plan board, and the plan force
and the turn-end check pushed it back to its tools.
`internal/harness/midreply.go`.

- **A user message, not text in a tool result.** A framed header in the
  result was tried first; Gemini ignores even framed text inside a
  functionResponse, while Qwen and others answer. Now a message typed while
  a tool runs is saved, once that result is stored, as a pending user entry
  (`deliverMidTurn`, tag `mid-turn`). agentkit (v0.36.0) claims it at the
  next round and renders it after the round's last tool result
  (assistant(tool_calls) → tool → user); the turn goes on. The words are
  verbatim, with no tag: a user message needs no explanation, and no test
  showed one is needed. No system-prompt line either. Other lifted news
  (board, background, peers) still rides the result.
- **Once.** The entry is the message's only copy: no `delivered` entry is
  written for it (that kind is now only in older logs), the queue no longer
  holds it, the intake (asks.go) records it once at `SayAs`, and
  OnDelivered names the call whose result it followed. History shows it as
  queued (typed mid-turn).
- **Fallback.** `providers[].fold_user_into_tool_result: true` sets
  agentkit's `ShaperPolicy.FoldUserIntoToolResult`: the words go at the
  end of the last tool result as `[the user wrote, while this ran] <text>`.
  Default off; for a chat template that rejects user-after-tool (some
  llama.cpp ones).
- **Missed reply.** If the round after the delivery writes no text and
  only calls tools, `[dun] The person asked you something mid-turn; reply
  to them now.` rides that round's results: once per delivered message (a
  new message arms it again). Nothing when nobody typed.
- **The plan force waits.** While a reply is due (the round after the
  delivery, and after the missed-reply aside the round after that) the
  forced plan call is not sent; it stays armed for the next request.
- **The turn-end check accepts the reply.** A text-only round right after
  a delivery ends the turn with no check (no "Open plan nodes… Finish
  each"). The simpler of the two options: asking whether to continue would
  cost a round and put the model back in charge of a turn the person is now
  in. No new turn and no wake: the unrequested-turn and spend rules are
  unchanged.
- **It starts no turn.** A mid-turn user entry is a `KindUser` like a
  turn's input, so what finds the turn's start by the newest user entry
  reads it through `turnInput`, which skips the `mid-turn` tag: recap's
  `turnStart` and its repeat cue, the fold's ask (asks off; the message is
  a later word, as a `delivered` one was) and the turn's reply
  (`lastReplyID`). A recap over it keeps the words verbatim (`Said`). It
  still counts as the person's message where `SayAs` counts it: the intake,
  settle, the automatic-turn budget, the missed reply.

## agentkit owns the empty reply (2026-11-19)

agentkit v0.38.0 detects a round with no text and no tool call, runs one
repair round (a notification tagged `empty-reply`: "Your last reply was
empty … Reply again."), and when that is empty too ends the turn with
`*agent.EmptyReplyError{FinishReason, NativeFinishReason}`. Before, the
turn ended with `Reply ""` and no error, and dun detected it
(`noteEmptyReply`, the entry below).

- **Detection and repair are agentkit's.** dun's `Reply ""` check is gone.
  The error is not transient (`llm.TransientUpstream`), so the turn-scope
  retry returns it at once: one repair, never two.
- **The notice is dun's.** `describeTurnError` (`internal/harness/turnerrors.go`)
  gives the error dun's text, `<model> returned an empty reply: the turn
  ended with no text from the model (finish_reason stop, native
  MALFORMED_FUNCTION_CALL)`; it unwraps to the agentkit error. The UI shows
  it as the turn's error, for every turn (the `OnNotify` notice and its
  person-only rule are gone: an error event already reaches every client).
  `<session>.errors.jsonl` keeps it with `finish` and `native_finish`.
- **The repair note is the model's**, like the turn-end check: no
  `OnNotify` ping, not in the history shown, still in the transcript.
- The same pass names a stall (`*llm.StallError`, agentkit's 10m first-byte
  and 2m between-chunks watchdog): `stalled: no bytes for 2m
  (between_chunks)` as the error and the turn-retry reason, `stall` in the
  record. An SSE error event's code, type and metadata (`*llm.StreamError`)
  are in the record too.

## The person's message ends the prompt (2026-11-19)

A repro with Gemini 4: every round came back empty, agentkit wrote no
assistant entry, the turn-end check ran once ("Asks (the asks tool): …
Acknowledge the intake"), the model was empty again, and the turn ended
with `Reply ""` and no error. The person saw nothing. The prompt ended on
an asks block, not on the person's message: `AskAs` ran `prepareTurn`, then
published the message, and `runTurn` ran `prepareTurn` again, whose
`planTurnLine` queued a second block that `flushSilent` wrote after the
message. Each retried attempt added one more.

- **One asks/plan block per turn, before the message.** `prepareTurn` (the
  caller, once, before the message) writes it; `runTurn` calls
  `prepareAttempt` before each attempt, which does the rest of the
  preparation but not `planTurnLine` (`internal/harness/turn.go`). The
  asides are flushed before the inbox arrivals, so a queued message of the
  person's is also the last thing before the round.
- **The block's entry is tagged `asks`** (`AsideOpts.Tag`): agentkit renders
  a notification `[<Tag or kind>] <content>`, so the model reads `[asks] Open
  asks: …`, not `[notification] [asks] …`. The history shows it the same.
- **An empty reply is said.** A person's turn that ends with no text from
  the model in any round (agentkit sets `Reply` only from a round with
  text) gets a notice, `<model> returned an empty reply: …`, through
  `OnNotify`, and a record in `<session>.errors.jsonl`, so `dun errors`
  shows it (`noteEmptyReply`, `turnerrors.go`). An automatic turn may end
  quietly: nobody waits on it. Replaced by "agentkit owns the empty reply"
  (agentkit v0.38.0).
- **The turn-end check is the model's.** agentkit tags it `idle_check`; the
  store does not ping `OnNotify` for it (no 🔔 in the TUI or the plain CLI)
  and the history shown leaves it out. The model still reads it, and the
  transcript keeps it.
- **The asks tool's schema is flat.** `id` and `merge_into` are integers,
  `id` 0 the intake; `split` is an array of the new asks' titles (each
  takes the item's state, nodes and note, and the whole intake as text),
  not an array of objects. No union-typed field and no nested object,
  which Gemini's function schemas take least well. The call still reads
  `"intake"`, `"3"`, `"#3"` and split objects, the shape before. 713 bytes
  of tool def (was 911).

## Thinking titles are bold (USER 2026-11-18)

USER: "strip out the thinking meta language and use **what** — because we A)
want to render markdown in thoughts, and B) the bold/title seems like a great
way to track what the thinking is." `internal/harness/systemprompt.go`
(`systemThinking`), `internal/tui/thinking.go`, `mobile/src/lib/thinking.ts`.

- **The prompt asks for a bold title per step**, `**<what, in a few
  words>**`, in place of `# Thinking: <what>` / `# Summary: <what>`: 245
  bytes became 106. Claude writes bold titles unasked; the line stays for
  models that do not (DeepSeek, Qwen).
- **A title is a line that is only a bold span** (`**…**` or `__…__`, a colon
  after or inside it allowed). Bold inside a sentence is not one; a span
  counts once it is closed. The folded line and the open header show the
  latest title (with time and words) where "thinking…" / "thought for" was;
  no title falls back to the first line, as before. No side call names one.
- **Old logs keep their titles.** `# Thinking:` / `# Summary:` still count,
  and a Summary wins once the thought ends. `demoteStepHeads` still turns a
  leaked heading in a reply into italics; a leaked bold title is plain bold.
- **The body is Markdown** (TUI): bubbleteandmd, like a reply, Frame while
  live, in the thought's colour (stSecondary) with no margin, a title in a
  paragraph of its own. A side effect: lines of one paragraph join, as in a
  reply. Cost, a 1,100-word live thought: render about 1ms a frame (a reply
  0.7ms); the box's wrap went from 34ms (it stripped the whole body with a
  regexp) to about 1.5ms by wrapping only rows too wide. The pager shows the
  text plain.
- **Mobile** has the same title rules; its body stays plain text, because
  `Markdown.tsx` draws only in the reply's colour and size.

## dun manages its tools under DUN_HOME (USER 2026-11-18)

dun took mcpshell, poly-lsp-mcp and raglit from the user's PATH first, then
from its own directory. What ran was whatever the shell had first: a Mac
ran a modified mcpshell checkout from `~/.local/bin`, far below Min, for
weeks; the version was checked only in `dun doctor` and `dun tools`.
`internal/mcpserver/tools.go`.

- **Managed first.** For each tool: an override, else the pinned copy at
  `$DUN_HOME/tools/<tool>/<version>/<goos>-<goarch>/<tool>`, else not installed. The user's
  PATH and dun's directory are not searched for these three. Other commands
  (git, language servers, ant) are found on PATH as before.
- **Overrides.** `$DUN_MCPSHELL`, `$DUN_POLY_LSP_MCP`, `$DUN_RAGLIT`, else
  `tools.<name>.path` in `$DUN_HOME/mcp.json`; env wins, as env does over
  every config file (standards CFG-3). A server entry whose `command` is a
  path is used as it is, before either. `tools` is read from
  `$DUN_HOME/mcp.json` only, as `default_via` is: a repo's dun.json does not
  pick a machine's binaries, and the lookup has no workspace. An env var
  reaches dund's engines only when dund was started with it; the mcp.json
  entry reaches every process. An override that is not an executable is
  "not installed" with the reason; the pinned copy does not stand in for it.
- **Version.** A pinned copy is the pinned version by construction. An
  override runs whatever its version; doctor, `dun tools` and `dun ps` call
  it "your build" with its version, and say when it is below Min. dun never
  updates an override.
- **Auto-fetch on first need, release builds only.** When a session starts
  a tool whose pinned copy is missing, `EnsureCommand` downloads the
  tarball from deps.json (or the registry's newer pin), the code doctor
  --fix runs (`FetchPinned`, `update.Fetcher.Install`: sha256 and size
  checked, temp file, rename). A lock file per tool (`tools/<tool>/.lock`,
  flock) makes concurrent sessions and dund download once. One quiet line:
  "fetching mcpshell v0.14.0 …", then "fetched …" or "… failed: <reason> —
  dun doctor says how to get it" (the server log and an OnServerSetup
  event). A failure is "not installed"; the session goes on. A build that
  is not a release does not fetch; its `doctor --fix` runs `go install`
  with GOBIN set to a temp directory under `tools/<tool>/`, then renames the
  binary into `tools/<tool>/<version>/<goos>-<goarch>/`.
- **Old versions are kept; there is no prune.** When the pin's directory is
  missing (the registry raised the pin while offline, or a build that does
  not fetch), the newest kept copy at or past Min runs, and doctor and the
  session-start hint say it is behind. A prune is not needed yet: each
  version is 15-40 MB, and a pin moves every few weeks.
- **Migration: old copies are left.** Tools doctor --fix put beside dun stay
  there, unused. doctor names each copy on PATH or in dun's directory as
  "unused (dun uses <managed path>)", with the `rm`, and `tools.<name>.path`
  to keep using it.
- **Images set the overrides.** The Dockerfile (`--docker`) sets
  `DUN_MCPSHELL`, `DUN_POLY_LSP_MCP` and `DUN_RAGLIT` to its
  `/usr/local/bin` copies; the harbor agent sets `DUN_MCPSHELL` to the copy
  it uploads; the release workflow exports all three for its tests.
- **A waiting fetch ends with its context**: the lock is a non-blocking
  flock tried again every 100 ms (`fsutil.LockFileContext`), so an engine
  closed or a server switched off while another dun downloads stops waiting.
  The per-platform directory lets one DUN_HOME on a shared disk serve Linux
  and macOS.
- **A copy on PATH is named** in the session's "not installed" line, with
  `DUN_<TOOL>` / `tools.<name>.path` to use it.
- **dund and engines** resolve through the same code in the same DUN_HOME,
  so the service's PATH no longer matters for these tools; doctor's
  service-PATH warning is kept for git only.

## Model tiers (USER 2026-11-18)

The `agent` tool took a raw model id per child, with no guidance on which
model fits which work, and every side call ran on the session's model.
Code: `internal/dunconfig/tiers.go` (resolution), `internal/harness/tiers.go`
(spawns and side calls), `cmd/dun/tiers.go` (wiring, doctor).

- **Three fixed tiers: fast, work, think.** `"tiers": {"fast": {"provider",
  "model", "effort", "use"}, …}` and `"default_tier"` in
  `$DUN_HOME/config.json`, beside `facets` (the file that holds the
  providers a tier names; `default_via` is in mcp.json, which holds no
  models). Shipped `use` texts: fast "mechanical work (renames, formatting,
  file operations, summarizing given text; no judgment)", work "scoped
  research, code reading, single-area fixes", think "planning, tradeoffs,
  architecture, review".
- **A repo narrows only the guidance.** A repo's dun.json layers (the
  same files as `files` and `rag`) may set `tiers.<name>.use` and
  `default_tier`. A provider, model or effort there is ignored and dun
  doctor names it: a committed file does not pick what a machine pays for.
  It does pick, with `default_tier`, which of the machine's tiers a spawn
  that names none uses.
- **Resolved at engine start.** A change to the tiers in config.json
  applies from the next engine start. The engine sends its resolved tiers
  in the settings event; `/model` shows those, not a fresh read of the
  config.
- **An unavailable tier does not stop spawns.** A tier on a provider that is
  not logged in is said at engine start and in doctor; a spawn on it (or on
  a tier whose client cannot be built) runs on the child default, and the
  tool result leads with "tier fast unavailable: <reason>; ran on <model>".
  The older silent fallback (a model on the session's provider with no
  client) gets the same note.
- **Precedence (standards CFG-3).** Field by field as facets: flag
  (`--tier-fast`/`--tier-work`/`--tier-think`, `[provider/]model`) > env
  (`DUN_TIER_FAST`…) > config. default_tier: `--default-tier` >
  `DUN_DEFAULT_TIER` > the repo's > config's > work. Like `--child-model`,
  the tier flags are not among dund's allowed engine flags.
- **Agent tool.** A `tier` enum param, and one line per tier in the
  description (its `use` and the model it resolves to). A `model` given
  overrides the tier, effort included; the tier's effort applies unless the
  call or `--child-effort` gives one (the role order, "Roles and model
  profiles"). A tier with no model runs on the child default
  (facets.child, `--child-model`, else the session's model). With no tier
  naming a model the param and the lines are left out: the tool is byte for
  byte as before, checked against main's output in
  `harness/testdata/agent_tool_notier.golden.json` (3473 bytes, 3548 since the roles enum; three tiers
  with the shipped texts add 509).
- **Side calls.** `"side_calls": {"<kind>": "<tier>|session"}` in
  config.json. Defaults: suggest, fold, remember and rescue `session`;
  plan_summary, journal, commit, rephrase and rules `fast`; learn
  and repaint `work`. A tier with no model, an unavailable one, or a client
  that cannot be built is the runner the call used before. A tier call
  refused before any reply (context too long, auth, model not found) is
  logged and asked once more on the session's runner. Side calls take the
  tier's model, not its effort: each has its role's (fold and rescue
  `lowest`, the others nothing), clamped to the model it runs on.
- **Window-sized calls default to the session's model.** fold, suggest and
  remember send the session's prompt-cache prefix and its history (fold
  since SessionPrefix; suggest and remember are forks of the turn's
  prefix); rescue sends up to half the window to recover a session that is
  over it. The cache hits only on the model that wrote it, and a smaller
  model's window may not hold the input: a rescue refused there would leave
  the session unable to recover. A `side_calls` entry that puts one of them
  on a tier is warned about at engine start and in doctor (the lost cache).
  Since roles (2026-11-18) such a call goes to the tier only when the tier
  model's profile knows a window that holds the input and the reply's cap;
  otherwise, and when no window is known, it runs on the session's model,
  logged. The retry on the session's runner still covers a refusal. The
  proposal had suggest on fast; that is one `side_calls` line for a user
  whose fast model holds the history. The other side calls are not capped
  to the tier model's window; the retry covers an input too large for it.
- **Shown.** A child a tier picked the model of carries the tier: the
  sub-agent strip (`#2 fast running · low`, the level as sent), its report and the agents pane
  (`fast: model`), the sidecar a resume reads, and `dun ps` (the session's
  label and its tree's rows). `/model` lists the engine's tiers and what each
  resolves to; `dun doctor` lists the tiers, the default, each side call's
  tier and what it runs on, and the warnings. Both also show the role table
  (below).

## Roles and model profiles (USER 2026-11-18)

Which model and thinking level a request got was decided in six places:
`/effort` for rounds, `--child-effort`/facets.child and `tiers.<t>.effort`
for children, the agent call's `effort`, a literal off in `foldOpts` (and
rescue), and agentkit's capability record on the wire. Code:
`internal/harness/roles.go` (resolver), `profile.go`, `effortclamp.go`.

- **Roles.** `main` (the session's rounds), `child:fast|work|think`, and one
  per side-call kind (`SideCallKinds`, the same list as dunconfig's). Each
  resolves to {provider, model, wanted effort} with the source of each
  field (`RoleChoice`; `RoleResolver.Resolve`). Every request path asks it:
  rounds (`sessionOpts` in the prompt builder), spawns (`spawnAgentFor`),
  side calls (`sideRunner`, `roleOpts` for fold and rescue). spawnTier, the
  literal off and the per-path effort defaulting are gone.
- **One order for every role**, field by field: the call's argument > a CLI
  flag or env var > the repo's dun.json (only `use` and `default_tier`, the
  tiers rule) > `$DUN_HOME` (config.json's tiers, facets, side_calls;
  `/effort`'s effort.json, which beats facets.main as before) > the role's
  built-in default. For a child `--child-effort` is the flag layer, so it
  now beats `tiers.<t>.effort` (it did not before); facets.child's effort
  stays under the tier's.
- **Defaults state intent.** main unset (nothing sent; the server decides),
  fast `lowest`, work unset (the child loads the saved `/effort` level, as
  before), think `high`, fold and rescue `lowest`, the other side calls
  unset (their options as built: nothing sent). A tier's default applies
  only when the tier names a model; a tier with no model is the child
  default, effort included. So with no config every request sends what it
  did: `TestRoles_NoConfigSendsAsBefore`.
- **lowest** is the lowest level the model accepts: none on a model that
  can turn thinking off (fold still sends off there), else agentkit's clamp
  of none, minimal and up, with its reason ("minimal (none not supported:
  reasoning.thinking_only=true …)").
- **Model profile** per provider+model (`ModelProfile`): the effort clamp
  (agentkit's `Client.Effective`, `EffortSupport`), `thinking_only` (the
  record; a model list's `thinking_modes` with no off is recorded so, as
  declared), the window (the harness's for the session's model; the record's
  `limits.context`, else one model-list GET, cached, for another), and tool
  support (`tools.supported` in the record: dun's key, which nothing learns
  yet). The profile decides: a window-sized side call's tier (above); a
  child tier whose model is known to take no tools is the child default,
  with "tier X unavailable: <model> takes no tool calls …; ran on <model>";
  a tier's effort is clamped to the tier's model, not the session's.
- **agentkit v0.35.0's scale** (bumped with this work). `ChatOpts.Effort`
  replaces `ReasoningEffort`/`EnableThinking` and dun's `applyEffort`;
  `effortclamp.go` is the one adapter (default is `EffortUnset`, off is
  none). `/effort` offers default, none, minimal, low, medium, high, xhigh,
  max; off is read as none, so an old effort.json still works. The picker
  marks a level the session's model does not take, with the reason;
  `/status` keeps the saved level and says what is sent. The agent tool's
  effort enum is the full scale plus lowest (+75 bytes on every round:
  3473 to 3548 for the tool with no tiers; the golden changed on purpose).
  `lowest` is taken wherever a level is (flags, facets, tiers, the agent
  call, `/effort`). A value that is no level is said at engine start and in
  doctor; one from `--child-effort`/facets.child is kept, so a spawn that
  would take it is refused, as before roles, not dropped. A resumed child's
  saved off is read as none. The window-derived thinking budget is still
  not sent (as before: the level decides).
- **Tier defaults change configured tiers.** A fast or think tier with a
  model and no effort inherited `/effort` before; it now runs at lowest or
  high. The table's source says `tier default (fast→lowest)`, and the engine
  says it once at start (and doctor): "tier fast (m) has no effort set …".
- **Wire changes from dun's side with no config:** none on a model whose
  record is empty. On a record that says thinking-only, fold, rescue and
  `/effort none` rounds send minimal (agentkit's lowest) from the start
  instead of none then agentkit's rewrite.
- **Wire changes from v0.35** (intended; agentkit, USER): OpenRouter low and
  medium send `reasoning.effort` instead of max_tokens 1024/4096; an
  Anthropic-compatible high sends a 16k thinking budget (dun's high sent no
  thinking there before); llama.cpp none adds `reasoning_budget_tokens: 0`;
  a refused level clamps lower first.
- **One view.** `/model` (the roles window) and `dun doctor` show the
  table: role, model, wanted, sent, reason, sources. The settings event
  carries it in `Settings.Tiers` after the three tier rows (rows with
  `role` set; no parallel field), and the engine sends it again when a
  client learns a fact or translates an effort (`OnCapsChanged`, from
  agentkit's OnLearn and OnTranslate), only when the table changed. The
  sub-agent strip (`#2 fast running · minimal`) and `dun ps` (`#2 fast,
  effort minimal`) show each child's level as sent. Clients render what
  the engine sends.

## Release updates

- **para shares dun's signing key (Carl, 2026-11-10); manifests are bound to
  their product.** `VerifyManifest` (`internal/update/manifest.go`, used by
  `Run` and `Switch`) requires the signature's trusted comment to be exactly
  `dun <version> manifest` (what `make release` writes) and refuses a
  `product` field other than `dun` (absent = dun). It runs before `key_next`
  is returned and before any `State` write, so a para manifest cannot add a
  trusted key or move the replay floor. The registry index keeps its own
  check (`VerifyAny`, comment `dun registry`, unchanged).

- **A source build moves to releases only when asked (Carl, 2026-11-10;
  reverses item 47's "dev/source builds have no download paths at all",
  `git show cb0e5bd:plan/plan.md`).** The manifest URL is in every build
  (`release/key.go`, beside the key that was already there), but a source
  build reads it only on an explicit `/upgrade --channel=release`; it never
  self-updates from releases otherwise. The channel stays a property of the
  binary, not a config key: the installed release identifies itself, so
  plain `/upgrade` follows releases afterwards. The explicit switch may
  install an older version than the running one (a source build is usually
  ahead of the last tag) and shows both; the manifest replay check stays,
  and plain `/upgrade` still refuses a downgrade. The launcher's binary is
  not overwritten (the launcher would rebuild it from source on the next
  start); the release goes to install.sh's `~/.local/bin/dun`, and any `dun`
  that comes first on PATH is named, never deleted. Release → source is
  `make install` in the checkout: dun downloads and builds nothing for it.
  Details: `sessions-and-dund.md`, Release and source channels.
- **Carl: questions don't hold an upgrade; idle actions wait for it
  (2026-11-10).** Questions: a turn waiting on an `ask_user` question waits
  on the person, so it no longer holds a restart (it held for up to
  `upgradeGrace`, then the agent was told to ask again). The stop saves the
  question in the registry; the next dund shows it again and the answer
  arrives as a new `[automatic]` user message, not as the tool result —
  Carl accepted that; the engine that asked is gone, and a message needs no
  engine to keep a call open across the restart. Interrupting it leaves a
  quiet note: an interrupted question starts no turn today either. Idle
  actions: while an upgrade is pending dund starts no
  turn, because each turn is one more thing the restart waits for. The
  scheduler fires nothing (`scheduler.held`); a held entry keeps its `At`,
  so `schedule.json` already carries the hold across the restart, and the
  next dund fires each held entry once (missed loop runs are not replayed,
  as after any downtime). A failed upgrade pokes the scheduler. One
  exception, Carl's choice: an idle session with background jobs or
  sub-agents the upgrade waits for is woken by the wrap-up notice, once per
  pending upgrade, so it can wrap them up (the 2026-10-28 "wakes for it if
  idle" stands for them); a turn would otherwise wait for its jobs or the
  grace period. The wrap-up notice to a running turn and the "did not
  happen" notice are quiet; a session with nothing running is not told.
  Not covered: the engine's own wakes (a
  job finishing, an aside escalating, a tool server's wake) and a peer's
  `dun_msg` with wake; they are not idle actions of dund's. (Covered since
  USER 2026-11-17, next bullet.)
- **Decided (USER, 2026-11-17): automatic launches wait for the new build;
  the resume is written before anything is cut, and continues the cut
  work.** Drain gate: while an upgrade is pending, a message no person
  typed is kept in the registry (`record.Queued`) and sent after the
  restart; the sender gets `ack {queued}` ("dund is upgrading; …"), also
  without a cid, and `dun_start`/`dun_send` return `queued: …`. Automatic is
  said by the sender (no client or source field existed): `User.Auto`, set
  by dun mcp's `dun_start`/`dun_send` only, and a peer's `dun_msg` from
  another session's agent. The TUI, the web and the app are people, also
  typing into a `--worker` session; `dun msg` from a shell is a person
  (`PeerMsg.Human`: the engine's hold lets its wake through). An older dun
  mcp sends no `Auto` and is taken for a person. A message a person types
  starts at once and holds the restart as before. `dun_start` while pending makes the session and
  starts its idle engine (no turn: nothing the upgrade waits for); only
  the first message waits, and a session whose only message is queued is
  kept on load. Engine self-wakes: dund sends `upgrade_hold {on}` to every
  engine (and to each one started while pending; `on:false` when the
  upgrade fails), numbered (`gen`, up at each change; an engine ignores an
  older one, so a start racing a failure cannot stay held). Held, the engine's loop runs no turn of its own — every
  self-wake (job end, peer `dun_msg`, server wake, escalated aside, settle)
  goes through `h.wake` and `Pending`, one place to gate —; it writes the
  waking news into the conversation (`HoldPending`, persisted in the
  transcript) and emits `wake_held`, which dund keeps out of the log and
  turns into a registry wake for the next start. Chosen over "the engine
  queues it in its state": the engine is killed at the restart (adoption
  is icebox i_1m7pe91wn18bq6j9m61cv1), so in-memory state would need a
  second durable store; the transcript already is one, and the next turn
  reads the news where it would have anyway. A person's message and dund's
  waking wrap-up notice (one pass) still start a turn; quiet news (asides,
  `wake:false`) stays in memory and rides the next turn, lost at a restart
  as before. A peer's `dun_msg` to an idle session says "queued". Once
  the stop has begun (`PrepareStop`, state `restarting`), `dun_msg` and
  `dun msg` are refused ("dund is restarting: send … again") — the record
  is written; a `wake_held` that arrives after the record records itself
  and rewrites the registry (`heldAfterStop`). Scheduled entries stay held as before. Durable resume: the
  stop records every working session's resume (and held wakes, cut
  sub-agents) and writes the registry first; a failed write takes the
  flags back, interrupts nothing, and puts the upgrade back to `pending`
  with the error, retried every `upgradeRetry` (5 s); failing for
  `upgradeRecordGiveUp` (2 min), the upgrade fails (the hold ends, the
  queued messages go). Queued messages and sub-agent continues are
  delivered at least once: one whose send fails is kept for the next try,
  and a crash between a delivery and the registry write after it sends it
  again. A SIGTERM cannot be
  refused: the error is logged and dund stops. After the restart each cut
  turn auto-continues: the `[automatic]` message names the tool calls in
  flight (dund now tracks `tool_call`/`tool_result`) and the killed jobs
  with their commands. One continue per cut turn: dund marks the session
  `continued` until a turn ends that the person caused or saw (done, an
  error, their interrupt or stop; not dund's own stop interrupt); a stop or crash that cuts
  it again records `resume_quiet`, and the next start leaves a quiet
  `[dun]` note and wakes nothing (no loop). Sub-agents running at the
  stop are recorded (`record.Agents`) and each gets `agent {action:
  continue}` after the parent's message — restarted from its transcript
  if it came back stopped (`Harness.AgentContinue`), then told to go on —
  and the parent's message and an aside say so. This replaces the
  2026-10-27 "children come back idle, not re-run" for children a restart
  cut; a child idle at the stop still comes back idle. Open `ask_user`
  is unchanged.

## Sessions, supervision and diagnosis

- **Turn errors are kept beside the session, structured (2026-11-18; aw4
  i_1m7rqbptp6z6f22q6jgff8).** `<session>.errors.jsonl`
  (`internal/harness/turnerrors.go`, written from `turnThenTriggers`, so
  engine, `-p` and children alike): time, session, provider, endpoint host,
  model, error text, HTTP status, the server's body; payload and text at most
  8 KiB, the newest 200 per file; the endpoint's key, Authorization and key
  headers and fields, bearer and `sk-` tokens become `[redacted]`. A
  canceled or held turn is not kept. A sidecar, not a transcript entry: the
  transcript's `error` entry (`turnerror.go`) keeps only the text, and
  reading it back means every transcript. `dun errors [--session <id>]
  [--json] [-n N]` prints them from `$DUN_HOME` with no dund. Limit:
  agentkit v0.37.0 keeps only an SSE error event's `error.message`
  (`llm/client.go` ~2028-2069, then `fmt.Errorf("%s")` in
  `agent/session.go` ~1113); type, code, metadata and the raw event are
  dropped, so a record of one holds the text alone.
- **Engines outlive a dund restart (USER 2026-11-17).** dund starts each
  engine with `--listen <home>/dund/engines` (its own unix socket; stdin
  `/dev/null`, output to `engines/<pid>.log`) and, under systemd, in a
  transient scope of its own; an upgrade's restart (in-place exec and the
  exit-75 handover) detaches from the engines and the next dund dials them
  back (`engines/<pid>.json`, hello, `link_resume{after}`). Before, every
  restart killed every engine four ways: `svc.Close()`, the unit's
  `KillMode=control-group`, SIGPIPE on fd 1, EOF on stdin; so an upgrade had
  to drain or cut work. Turns and self-wakes run on in the gap; the engine
  rings 16 MB of numbered events and replays them, a ring overflow becomes
  `history{reason:"reattach"}`. An adopted engine of an older build moves to
  the new build at its next idle. The drain and resume record stay as the
  fallback when adoption fails (version mismatch, dead socket, wrong
  session). SIGTERM still stops and kills. Wire format, acks, skew:
  `engine-adoption.md`.
- **One representation, not two.** The agentkit entry list is both the model's
  context and the TUI history (`internal/harness/store.go`). Writes are a full rewrite to
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
  for. A headless task (`dun -p "task"`) does not open a UI, so scripts do
  not. `flag.Usage` is overridden because Go prints `-tui`, which reads like
  `-t -u -i`.
- **`-p` is the headless task; `--engine` is the JSON protocol (USER
  2026-11-17).** `dun -p "task"` runs one task and exits: the answer on
  stdout, the work and the diff on stderr (answer.go); with no task and stdin
  not a terminal, stdin is the task, the way other CLI agents read a pipe. The
  line-delimited JSON events that dund, the TUI and the Harbor adapter drive
  moved to `--engine`. Bare words are only subcommands: `dun "fix it"` is
  refused with the `dun -p` spelling, so a typo in a subcommand cannot start a
  paid run. A dund from before the rename still spawns `dun -p` with no task;
  exactly that spawn (argv `-p --workspace …`, `DUN_CHILD=1`, `DUN_DUND=1`,
  stdin a pipe) stays the engine (cli.go `oldDundSpawn`), so an upgrade does
  not turn its sessions into headless runs that read their JSON as a task.
  Remove it one release after the rename.
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
  (`DUN_MD_STYLE`, `DUN_MD_QUERY=1`). It was found only because the slow-frame dump names
  the message type behind the worst frame. Frame stats are always on (a dump
  is written when frames stutter; `/perf` was removed); pprof is opt-in (`DUN_PPROF`).
- **Rendering is paced and cached** (details in `tui-architecture.md`):
  `refresh()` re-wrapped the whole scrollback per streamed token (7.8 ms per
  frame at 200 blocks, ~1 s CPU per 100-token reply, on the goroutine that
  reads the keyboard). Fixed by a per-block wrap cache (invalidated by width
  and open state) and a frame cap (`renderHz`).
- **dun writes nothing into a repo.** Per-project state lives under
  `$DUN_HOME/projects/<id>/`; a repo's own `dun.json`, `.dun/` and
  `dun.local.json` are read, never created (`mcp-and-tools.md`).
- **The divider is a centered line with pane labels (2026-11-17).** One dim
  `─` across the full row, the same glyph whatever has focus, with the panes
  named left-aligned: `─ input ─[messages]─ jobs ───`. The focused name is
  accent, bold and bracketed, so it reads without colour; `jobs` shows only
  while the activity strip has rows. Narrow rows drop names in order: full,
  short (in/msg/jobs), the focused one alone, a plain line below 20 columns;
  the "↓ newest" cue stays at the right end before any name. Names are
  clickable (one function draws them and returns their columns). Pushed
  screens name their panes the same way. This replaces the earlier half-line
  that leaned toward the focused pane (and its ▁/▄/▔/▀ glyphs).
- **`/rules add ./path` at project or global scope names each project's file
  (2026-11-20).** Kept as typed and resolved against the open workspace when
  the rules load, so `/rules add ./AGENTS.local.md --global` reads each
  project's own file; a workspace without it gets a "not found" row. Rows say
  "(each project's)". Without `./`, `~/` and absolute paths stay absolute at
  those scopes, as before; session scope is unchanged. At project scope the
  `./` resolves per worktree or subdirectory that shares the project's
  config, so each reads its own copy.
- **The AGENTS.md guard is removed (2026-11-23).** poly-lsp-mcp's file hook
  (`POLY_LSP_HOOK`, `agents_hook.go`) cancelled a read or edit until the model
  had read the nearest AGENTS.md. It predates rules in the system prompt: it
  blocked the root file the model already had (rules.go loads the root AGENTS.md
  and CLAUDE.md), a read through the shell did not count, and a hook can only
  cancel, not proceed with a note. Agents began to avoid the code server to
  dodge it. Gone with it: the host-hook arming (the guard was the only
  script), `DUN_AGENTS_STATE`, the `agents_read` state and its import from
  `.dun/`. Existing `agents_read` files under `$DUN_HOME/projects/*/` stay on
  disk; only a pruned worktree's overlay dir is removed, as before.
  `host_hooks` is still accepted in a manifest as a deprecated field and
  ignored, because installed and published poly-lsp manifests still carry it.

## File edits on tool results (USER 2026-11-24)

- **A tool result carries the files it changed, whole texts before and after**
  (`tool_result.edits[]`, docs/event-protocol.md "File edits"), for an ACP
  bridge's `diff` content and any client. The engine reads the files around
  the call (`withEdits`, fileedits.go) rather than the tools reporting them:
  agentkit's file tools and poly-lsp's node_edit answer with text or JSON
  that has no file text, and reading the disk is one rule for both. The model
  is not sent the texts; its result is unchanged.
- **Only tools whose arguments name every file they change.** exec and
  mcpshell's eval (a command or script changes anything), node_refactor and
  node_rename_file (importers across the workspace) are not read: a list of
  edits that misses files would tell an ACP client a wrong story. A
  workspace diff per call would cover them but cost a tree walk per call.
- **Caps: 1 MiB a file, 2 MiB a call, 8 MiB a history replay.** dund keeps
  20,000 events in memory per session; with no cap, a session editing a big
  file would hold every version. A file past a cap, binary, a directory or a
  symlink is named with `skipped` and no texts.
- **Persisted in a sidecar, not the transcript** (`<id>.edits.jsonl`, texts
  over 8 KiB as blobs). The transcript is agentkit's Entry, which has no field
  for it, and what the model reads. Size: one line per editing call, plus one
  blob per file version over 8 KiB (content-addressed, so an edit's old text
  is the previous edit's new text, kept once). `/clear` removes it.

## ACP bridge (USER 2026-11-24)

`dun acp` (`internal/acp`, `docs/acp.md`) lets editors (Zed, JetBrains
Air, Neovim) drive dun over the Agent Client Protocol.

- **A dund client, not an engine mode** (USER). The bridge talks to dund
  over its unix socket as the TUI does and translates; dund keeps owning
  sessions, so an ACP session is an ordinary one that the TUI and web
  follow too, and the rule that clients stay dumb holds: no turn state is
  inferred beyond what a prompt's answer needs.
- **SDK: github.com/coder/acp-go-sdk v0.20.0** (USER: a community Go SDK,
  coder's first). Apache-2.0 (on the notices allow-list, no exception),
  standard library only, ACP protocol version 1 at schema 1.32.0, generated
  from the official schemas, every method behind an interface the build
  checks (`sdk.Agent`, `sdk.AgentLoader`). Maintained by Coder, many
  releases (v0.18 to v0.20 on 2026-11-18): v0.20 broke API on purpose
  (MIGRATION_v0.20.md), so an upgrade is a reviewed change, not a
  renovate merge. Not evaluated further: it fit.
- **In place** (USER). session/new's `cwd` is the workspace and no
  `--worktree` is passed (dun's default is in place): the editor shows the
  files on disk. No dund change was needed.
- **A mid-turn prompt is queued** (USER), and its JSON-RPC answer waits
  for the end of the turn that gave its message to the model (`delivered`,
  or a turn's input), with that turn's stop reason. ACP does not say what a
  second prompt means; answering the earlier prompt at its own turn's end
  and the queued one at the end of the turn that read it is the one rule
  that never answers a prompt before the model has seen it. The SDK
  cancels the earlier prompt's context when the next arrives; the bridge
  ignores prompt contexts and stops a turn only on session/cancel.
- **Questions**: one answer from options is session/request_permission
  (the file-access question is the case it was asked for); a multiple
  choice is the agent's text, answered by the next prompt. ACP's
  permission is one choice from options, and the editor has no other
  question UI.
- **--no-suggest** on ACP sessions: no ACP client shows suggestions, and
  each costs a side call.

## Stop conditions: /until (USER 2026-11-24)

"You're dun when…": `/until` (`internal/harness/until.go`,
`cmd/dun/until.go`) keeps a session starting turns itself until a
condition fires. Carl approved the feature and layered conditions; the
rest below are assumptions he can overturn.

- **A set of conditions, not one mode** (USER). `turns N`, `goal <text>
  [--check "<cmd>"]`, `plan`, any of them at once
  (`/until plan turns 15`, `/until goal "tests pass" --check "make test"
  turns 10`). The run goes on while every target is unmet and stops at the
  first that fires, and says which. goal or plan without turns stops at 20
  turns. `/until off` clears them; bare `/until` shows each condition's
  progress. The name: no command, flag or tool was called until.
- **Safety stops, always** (USER): the person interrupted (a turn, or an
  interrupt between turns while the goal is judged); the turn failed; two
  turns in a row with no progress. Progress: a plan node changed state
  (with plan), or the turn made a tool call (with goal or turns).
- **A question pauses the run, the answer resumes it** (USER 2026-11-24;
  it stopped the run before). While a question to the person is open
  (ask_user, or a file-access question: the permission prompt; both are
  `Config.Ask`, which waits inside the turn), the `until` event says
  `paused` and its line `until: paused (question) · turn 3/15`. Whatever
  answers the ask (the TUI, the web client, ACP's request_permission)
  ends the pause: the turn goes on, and the run with it, turn and stall
  counts kept. An interrupt while paused cancels the ask and the turn: the
  run stops (`interrupted`). `/until off` while paused ends the run; the
  question stays open and the turn ends as usual. Paused is in the
  sidecar, and survives the engine (2026-11-24, "Questions survive a
  restart"): the new engine owes the run no turn, asks the question again
  (the run paused all the while), and the answer goes on with the turn and
  the run. The continuation counts as the run's next turn.
- **Engine-owned, persisted** (USER: dund and the engine own semantics,
  clients dumb). The conditions and the run are in `<session>.until.json`
  and in a `until` state event (event-protocol.md) with a one-line summary
  for status bars. A run under way when the engine stops (dund's restart,
  a model switch, a crash; an interrupt with a reason is not the person's)
  is owed its next turn, which the new engine starts at once. A
  `[automatic]` resume message dund sends meanwhile is queued into that
  turn, or is a turn of the run itself. Not tested live: only the sidecar
  round trip is (until_test.go). `/clear` stops a run and keeps the
  conditions.
- **A turn is any turn the session runs** while conditions are set: the
  person's message is turn 1 (or the turn running when /until is given).
  The conditions stay after a run stops (USER 2026-11-24), so the next
  message starts a new run (a mode, as the ACP option reads). They are on
  show at all times: the event's line is set idle too (`until: plan ·
  turns 15`), on the TUI's status line (ready, working, and a parked
  question's line) and in the web client's session header. Messages typed during
  a run are queued and delivered as always; they do not stop it.
- **The next turn is a user-set turn**, started with a `[until]`
  notification that says the turn number, the cap, the goal's last
  verdict and the plan's open nodes. User-set (autoturn.go): not under the
  automatic-turn budget, and ask_user is not refused in it, because the
  person asked for the run.
- **Goal judging** (USER). With `--check`, the command alone decides:
  run in the session's exec backend in a subshell, 10 minutes at most,
  exit 0 is met; a failure's last 2000 bytes of output are the verdict
  the next turn reads. Without it, a side call of kind `until` (its own
  role, tier work like learn, since it judges) gets the goal, the last
  turn's reply (6000 bytes) and its tool-call count, and must answer
  `{"met": bool, "reason": "…"}`; 2048 tokens out, reasoningBudget's share
  for thinking. An answer that does not parse, or a failed call, is not
  met: the run goes on to its cap or stalls.
- **plan stops when no node is ready or under way**: drained (none
  open), empty, or open nodes all blocked or waiting (said as such).
  Archived nodes are not counted.
- **ACP: two options** (USER): `mode` (off/goal/plan/goal+plan) and
  `turn_cap` (off/5/10/20/50, custom category `_turn_cap`); a turn cap with
  mode off is "run N turns". A goal's text is the next prompt's (or the
  running one's). A prompt is answered when its run stops. Details:
  acp.md "Stop conditions".
- **Not done**: no `--max` flag (turns N is the cap; one way to say it);
  the web client shows the event's line in its header, and nothing else of
  the run (no way to set conditions there but the `/until` command); the judge does not see tool results, only the reply and
  the call count.

## Questions survive a restart; in-flight calls are optionally replayable (USER 2026-11-24)

Carl: "we shouldn't lose questions? are they not tool calls that expect a
response?" A question is a tool call waiting for its result. Before, an
engine that stopped while ask_user or the file-access question waited
lost it: the next turn's heal answered the call "INTERRUPTED — the
connection to the model dropped" (wrong, too), dund's planned stops showed
a saved copy and sent the answer as a new `[automatic]` message, and a
model switch or a crash dropped it with no trace.

- **The rule** (Carl): in-flight tool calls are optionally replayable. A
  tool that declares itself replayable survives a restart: its pending
  call is restored and replayed on engine start. A call of any other tool
  that was in flight gets a failed result that says it was cut off by a
  restart and tells the model to try again (`restartedCall`, turn.go;
  before: "the connection to the model dropped"). An unanswered call can
  only be a stop's: agentkit answers every call while the engine runs.
- **Replayable is the tool's** (`Harness.replayable`, replay.go): ask_user,
  and the file tools while they wait on the access question, which runs
  before the call does anything. Replay means the call is dispatched again
  from its arguments, through the whole dispatcher (tool events, guards,
  the lift of queued messages), so it asks again and the answer becomes
  its result as on the first run. exec, MCP tools, the agent tool and the
  rest are not. Candidates not turned on: the pure reads (file_read,
  search, plan_show, session_search) could replay with no question, but
  that starts a turn on engine start with nobody asking for it, and dund
  already wakes a crashed session with a resume message. MCP tools: the
  spec's annotations (`readOnlyHint`, `idempotentHint`) would say which
  are safe, but agentkit's MCP client does not read them (none in v0.40.0),
  so none is replayable.
- **Kept**: a replayable call's question goes in
  `<session>.pending.json` when it opens (ask id, call id, tool, the
  arguments, the question, options, multi, no_custom, recommended, the
  turn's origin) and is removed when it is answered or cancelled. A
  record whose call has a result or is gone (a /clear) is dropped.
- **Stop**: a `stop` while the question is open (dund's restart, a model
  switch: `Restart`, /exit) SUSPENDS it (`ErrSuspended`): the call gets no
  result (the store drops agentkit's), the turn ends (`done` `suspended`,
  no fold, no further round) and the engine exits at once instead of after
  dund's 10 s kill. A kill or a crash leaves the same state with no help.
- **Start**: before anything else in the engine loop (before /until), a
  pending question is replayed (`Harness.Replay`, `replayStep`): a turn
  of the origin the asking turn had, the call dispatched again, the same
  `ask` event (same `ask_id`, `call_id`) to every client. The answer is
  the call's result and the turn goes on: the next round reads it last,
  as after any call, with no new user message and no asks or plan block
  after it ("The person's message ends the prompt"). Not a true mid-turn
  resume (the round's other calls, after the question, were never run and
  get `restartedCall`), but what the model reads is the same.
- **Interrupt** of the asked-again question: its result says the user
  cancelled it (`questionCancelled`; the live ask_user's interrupt says the
  same now), the turn ends interrupted, no model round.
- **A message instead of an answer**: as for any open question (the replay
  is a turn, behind the same gate), a `user` message is queued mid-turn
  and rides the call's result once the question is answered; only an
  `answer` answers it.
- **dund** (internal/dund): an engine that keeps its question
  (`questionPending`: the sidecar, and its call unanswered) is not
  interrupted by a planned stop (an interrupt is the person cancelling),
  no question is saved in the registry, and the next dund starts that
  engine (`reask`) with no crash resume message. A crashed engine dund
  restarts asks again by itself. An older engine keeps the restored path
  (the saved question, the answer as a message).
- **/until**: a run paused on the question stays paused across the
  restart and goes on with the answer (until.go, Stop conditions above).
- **Lost still**: a running exec or background job (killed with the
  engine; the call gets `restartedCall`, the job dund's resume note), a
  sub-agent's question (children have no sidecar; the child is resumed
  with its task), a mount question, and the terminal (non-engine) mode's
  question, which nothing replays.
- Tests: replay_test.go (ask again and continue, cancel, file access,
  other calls fail, /until, stale record), dund reask_test.go,
  cmd/dun askstop_test.go. Live 2026-11-24 (a private dund and `ux llm`):
  engine killed by PID mid-question, and a `switch_model` mid-question:
  each time the same ask id and call id came again, the answer was the
  call's one result, and the turn replied.
