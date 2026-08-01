# dun — plan

> How this plan works: living plan = current state + active work + decisions
> ONLY. Completed trees → `done.md` (one-line pointer left here). Deferred /
> opt-in next-steps → `icebox.md`. Marks: ◻ todo · ◐ in progress · ✅ done ·
> ⏸ parked · ❓ blocked. Every active slice carries **next / risks / blocking
> decisions / optional extensions**.

## What this is

`dun` — a coding-agent harness (a Claude-Code-in-Go) that composes iode's own
pieces into one agent that works a task inside an isolated workspace:

- **agentkit** — the engine: tool loop, context shaping/compaction, proactive-RAG
  hook (`FinderPreparer`), validation, token accounting. (Owns tablestakes, NOT
  orchestration — dun IS the orchestration.)
- **poly-lsp-mcp** — semantic code: `node_query` (navigate + call-graph),
  `node_read`, `node_edit` (rename/refactor, diagnostics on edit). gopls/tsserver/
  pylsp + tree-sitter.
- **mcpshell** — sandboxed compute: one `eval` tool (arithmetic, data wrangling,
  jailed file ops, SQL). Not a raw shell.
- **raglit** — knowledge: `search`/`ingest` + proactive suggestions (its
  `agent.DocFinder` → agentkit `FinderPreparer`).

Name: sounds like "done" (gets it *dun*); a dun is a workhorse. `github.com/
iodesystems/dun`.

## Architecture

```
dun (host: Bubble Tea TUI + agentkit Session + LLM → corrallm/bonsai)
  ├─ per task: git WORKTREE of the repo (isolated mutable surface)
  ├─ Docker CONTAINER (toolchain + the 3 iode tools), worktree mounted in
  ├─ mcpmgr spawns servers via `docker exec -i`:
  │     poly-lsp-mcp mcp --root /work · mcpshell mcp --files-dir /work · raglit serve
  ├─ exec tool = `docker exec` → build/test/git (safe, contained)
  └─ end: worktree diff → review in TUI → branch/PR
```

The tool composition is ~2 functions (mcpToolDefs + mcpDispatcher over 3 servers).
The NEW work is the TUI, the Docker+worktree lifecycle, the exec tool, and
system-prompt composition.

## Completed — see `done.md`

Everything below is ✅ done + (mostly) verified live. One-line pointers only;
full detail archived in `done.md`.

- **Tooling** — `tools/ttydrive` (drive TUIs headlessly) · `dun --setup` wizard +
  `~/.dun/config.json` · version stamp + dev self-update · `dun` on PATH is a
  rebuild-then-exec launcher (`tools/dun.sh`), so it is never stale.
- **Slice 1** — headless composition (3 MCP servers → one Session; `-p` JSON
  event protocol; proven live vs bonsai).
- **Slice 2** — Bubble Tea TUI (client of `-p`): pane focus/selection, vim `/`
  search, collapsible tool blocks, inspector overlay, docs notifications,
  tall-message scroll, mouse wheel, Starlark tool renderers, slash-command
  palette, horizontal arrow navigation (convo ↔ input ↔ suggestions).
- **Slice 3** — worktree isolation + exec tool (Host/Docker backends).
- **Slice 4a** — ask_user (turn pauses) + proactive notifications + background
  exec → notification convergence + TUI polish (glamour/diff/history).
- **Slice 4b** — worktree → PR (`open_pr`, opt-in `--pr`).
- **Slice 4c** — persistence (JSONL + content-addressed blobs; `--continue`/
  `--resume`/`--sessions`); TUI replays resumed history as scrollback.
- **Slice 4d** — web UI = the real TUI over xterm.js (`dun -serve`); dropped
  option A.
- **`--disable-exit`** · **SIGUSR1 screen dump**.
- **Slice L** — launcher/daemon (`dun -d`): L1 supervisor registry + central
  build/hot-reload; L2 shared-MCP dropped (worktree-per-session ⇒ nothing to
  share).
- **`--suggest`** — next-message prediction with probabilities.
- **Declarative MCP servers** — `dun.json` (project) + `dun.local.json` (machine);
  merge-by-id, inherit, `disabled`, per-server env/timeout.
- **Opt-in tool families** — only `shell` autostarts; `/rag` and `/lsp`
  (status·on·off·auto·manual) + `--rag`/`--lsp` start the other two, autostart
  persists to `.dun/dun.local.json`, and a server that fails to start is
  reported rather than fatal. A change to the tool set is carried to the model
  as an `Aside` (rides the next tool result / turn; never schedules one).
- **Retry UX + mid-turn messages** — provider waits are narrated (agentkit
  `Client.OnRetry`, rich corrallm 429s), a mid-stream death retries the TURN, a
  give-up leaves the session resumable, and a message typed mid-turn is lifted
  into the next tool result.
- **Ship — one tool, three modes** (2026-07-31) — verify/push/pr differ only in
  where they stop; checks finally wired; `open_pr` folded in; never touches the
  user's checkout.
- **No unbounded foreground exec; gh out of ship** (2026-07-31) — foreground
  `exec` dies at 5m (background exempt), and `ship` prints the `gh pr create`
  line instead of running gh. Cause of the 14-minute hang is in `done.md`.
- **Exit codes + background-job visibility + container cleanup** (2026-07-31) —
  `ExecBackend` returns `ExecResult` (pass/fail is the CODE, not a substring),
  background jobs stream to a log file and are tunable via `exec_monitor`, and a
  timed-out `docker run` now stops its container.

## Active work

The pattern that drove the last three items: the harness could not say what was
actually happening — a ship that reported success while leaving the work local, a
wait that looked like slow work for 14 minutes, a background job that was one
silent event until it exited. None was found by a test; all took `ps --forest`
and `/proc` on a live session. A–C closed those, and D (sub-agents) is built
through step 5 — the same discipline applied to a new surface: a child that
answers is IDLE rather than gone, silence is distinguished from failure, and the
agents pane exists so a resident child is a choice rather than a leak.

### ❗ 0. Slice D's code was DELETED on 2026-08-01 — read this first
Everything below that describes sub-agents or background-job plumbing describes
code that is **no longer in the tree**. A cherry-pick run from outside the
session stashed the tracked files and cleaned the untracked ones; `git stash`
does not take untracked files, so these were never captured by anything:
`subagent.go` (1108) · `subagent_test.go` (702) · `bgjob.go` (415) ·
`bgjob_test.go` (283) · `plan/subagents.md` (543, the spec) ·
`cmd/dun/activity.go`. Not on any of the 47 branches, not in the dangling
objects, not in `.dun/worktrees`. **Unrecoverable.**
- What survived: these plan docs (restored from `stash@{1}`), so the DECISIONS
  live even though the code does not. `plan/subagents.md` did not — the D
  section here is now the whole spec.
- The tracked half of that work (`harness.go`'s hooks, `exec.go`'s
  `ExecResult`, `ship.go`, the tool renderers) is still in `stash@{1}` and
  still references the deleted types, so applying it does not compile.
- **next (USER decision):** rebuild slice D from the D section below, or drop
  it. Nothing else in this plan should be trusted about sub-agents until that
  is answered.
- **the lesson, which is the reusable part:** work that is not committed does
  not exist. Untracked files survive neither `git stash` nor `git clean`, and a
  repo with other agents working in it is a repo where either can happen at any
  moment. Scaffolding commits are cheap; this cost 3000 lines.

### ✅ 1. The composer — one row that grows, and readline (2026-08-01)
`multilineInput` (the 4-line box from 711ecbd) had four defects the human hit
within minutes. Rewritten around SEGMENTS that carry their own `[start,end)`
buffer offsets, which is what makes the display↔offset mapping exact:
- **Text rendered twice.** With the cursor at the end of a row, `View` wrote the
  row, the caret, then the row AGAIN — `after` was initialised to the whole line
  and never cleared in the end-of-line branch. Read as "it double types".
- **Wrapping LOST text.** Breaking on a space took `chunk[:space]` and then
  advanced past the whole window, dropping everything between. Since the cursor
  math was derived by re-measuring those strings, the cursor drifted with it.
  This is the reason for the rewrite: a wrapped STRING cannot say what wrapping
  consumed, a segment can.
- **Arrows edited the buffer.** → or ↓ on a wrap point INSERTED a newline —
  navigating your own text rewrote it. Newlines are now typed with alt+enter /
  ctrl+j, and arrows only move.
- **No readline.** ctrl+a/e/b/f/w/k/u/d, alt+b/f/d, alt+backspace, ctrl+←/→.
  Matched on `Key.String()` but **only for non-rune keys**, or a bracketed paste
  of the word "home" is read as the Home key.
- Also: the box now grows 1→4 rows instead of reserving 4 from the start, the
  caret is reverse video rather than a `█` painted over the character it sat on,
  the placeholder shows focused as well as blurred, and ↑ from the first row
  recalls history (ctrl+↑/↓ still do it unconditionally).
- **verified:** `ttydrive` against the real binary — single render, growth to
  two rows on wrap, ctrl+w/ctrl+u, alt+enter. Plus 12 tests, three of them
  regressions for the defects above.
- **risks:** wrapping measures RUNES, not display columns, so a CJK or emoji
  line wraps a few columns early. Pre-existing; not worth fixing until someone
  types one.

### ◻ G. The activity zone — one navigable tree over agents + jobs
The agents pane made a resident child visible (D); it did not make it
REACHABLE. It has no selection, background jobs have no TUI presence at all
(`bgJobList` exists, nothing emits it), and a child's conversation is
unreachable by construction — `childConfig` nils `OnToken`/`OnToolCall`/
`OnNotify` (`subagent.go:293`). So the human can see that a child is spending
tokens and cannot see what it is doing.
- **The rule that organizes it (USER, 2026-08-01): `▸` means descendable.**
  Focused + `→` opens and descends; `←` ascends. Uniform across every zone.
  This REPLACES the ad-hoc horizontal handlers (`tui.go:634–694`), where `→`
  descends into docs on one block and hops panes on another.
- **Layout — two header lines in steady state**, both gone when there is
  nothing to show (a session that never delegates loses no space):
  - task line = the last user message, clipped to one line;
  - activity line = agents + background jobs, collapsed to one `▸` line.
- **Focus is three zones, cycled by tab** (input → convo → activity → input,
  shift+tab reverse). An EMPTY activity zone is skipped, so today's two-way
  toggle is preserved exactly. `↑`/`↓` keep their per-zone meaning.
- **Descent tree:** collapsed line → row list (↑/↓ picks) → a JOB row descends
  to its command + live log tail; an AGENT row descends to **agent scope**.
- **Agent scope** is a scope switch, not an overlay: task line = the child's
  spawn prompt, conversation = the child's, activity = that child's jobs plus a
  `↰ parent` row. Depth-1 in practice (children cannot spawn children) but the
  model is recursive and costs nothing extra.
- **Decided (USER, 2026-08-01):** in agent scope the input is LIVE and routes to
  `agent_monitor(tell:)` for that child — the human steers it directly. This is
  a new human→child channel that children were deliberately denied (no
  `ask_user`), and the parent only learns of it when the child next reports.
  That asymmetry is accepted: the human is not the model, and the pane exists
  precisely so a child is steerable rather than merely observable.
- **Transport is PULL, not push.** The TUI asks for a child's transcript
  (`<parent>.sub<N>.jsonl`) and the engine replies with a `history`-shaped event
  — the same one that already rebuilds scrollback (`main.go:387`) — re-pulled
  while the child runs. Pushing would mean un-nil'ing every child callback and
  tagging N streams nobody is looking at; pull also works unchanged for stopped
  and dismissed children, which push cannot do at all.
- **built once, then deleted with slice D (see ❗0):** step 1 was done —
  `JobInfo`/`jobState`/`Jobs()`/`jobsChanged()` in `bgjob.go`, `Config.OnJobs`,
  a `jobs` event mirroring `agents`, pushed on start and on finish and NOT in
  between (a push per output chunk repaints on every line of a build log), with
  times crossing as unix seconds so the UI ticks a running job's clock itself.
  Two tests, green under `-race`. `cmd/dun/activity.go` had the zone rendering.
  All of it went with the untracked files.
- **next:** rebuild step 1 (the `jobs` event) — it is small, it is fully
  specified by the bullet above, and nothing in the UI can name a job until it
  exists. But it depends on `bgjob.go`, which is part of the ❗0 decision.
- **assumed, not decided:** the expanded row list is capped at 6 rows and
  scrolls within itself, so an uncapped child count cannot swallow the
  conversation viewport.
- **risks:** the horizontal rework touches every existing `left`/`right` case,
  including the docs descent and the suggestion selector — the one part of this
  that can regress behaviour people already use. Agent scope re-pulls a
  transcript on a timer; a large child conversation re-parsed per tick is the
  obvious cost. Routing input to `tell` while the child is RUNNING has no
  defined behaviour yet — `tell` on a busy child is not the same as on an idle
  one.
- **blocking decisions:** none outstanding.
- **optional extensions:** a job row that offers cancel; agent scope showing the
  child's token spend against the parent's; making the task line clickable
  (mouse is already wired for the viewport).

### ❓ F. A child wedged with no in-flight request — cause unidentified (code gone, ❗0)
Live session `20260801-095147`, agent #1. Its last transcript record is
`11:50:13` (`tell_parent` → "answer recorded — you can stop now"), its last
prompt `11:50:14` (327 messages, ~106k tokens). Then nothing for 15+ minutes:
no session writes, no log lines, no retry notes, 0% CPU, **no socket**. The
MAIN agent in the SAME process kept working throughout — a prompt every 1–3s,
`bytes_sent` 84KB→3.4MB, zero 429s — so the single slot was free and the child
simply was not asking for it. It is blocked in Go, inside `s.h.Ask`
(`subagent.go:347`), not on the provider and not on the network.
- **next:** on the next occurrence, `kill -QUIT <the -p pid>` BEFORE anything
  else — the goroutine dump lands in the process's stderr log
  (`/tmp/dun-tui-*.log`) and names the frame. Everything short of that has been
  tried on a live instance.
- **why it was not taken this time (USER, 2026-08-01):** SIGQUIT kills the whole
  session, and the main agent was mid-loop. The child had already banked its
  answer, so there was nothing to recover — only a cause to learn.
- **suspects, none confirmed:** the parent's MCP manager is SHARED with the child
  (`childConfig`: `cfg.Servers = parent.specs`) and the same `*llm.Client` is
  shared too — `wireRetry` re-points `c.OnRetry` on every `Start`, so the last
  harness started owns the callback. Compaction in a child is invisible
  (`childConfig` nils `OnCompaction`), so a fold that never returns would look
  exactly like this.
- **risks:** unbounded. A child that wedges is resident forever, holds its
  Harness, and (before E below) reported itself as healthy.
- **first: make it observable** — a child that has stopped writing to its
  transcript is the cheap signal, and nothing watches for it.

### ✅ E1. A running child looked identical to a wedged one (2026-08-01)
Found while investigating F, and the reason F went unnoticed for 15 minutes
while the parent polled three times. `agent_monitor`'s report has exactly two
moving parts, and both were frozen:
- `tell()` restarted a child without clearing `ended`, and `snapshot` measures to
  `ended` whenever it is set — so every later report gave the duration of the
  child's FIRST task. Live: `18:22`, `18:29`, `18:44` all returned
  "RUNNING after 2m1s". Fixed by `startRunLocked`, which is now the one place a
  run's clock starts.
- The tally only moved when a run FINISHED, and `+=` on a CUMULATIVE
  `Usage.Total` double-counted every earlier turn when it did. Now `Session.OnUsage`
  → `Harness.noteUsage` → `subAgent.setTokens` assigns it per chat round, and the
  pane is pushed each time.
- `resumeAgent` left a restored child measuring from `started` to now, so an
  idle child that had never run showed a growing age in the field that means
  "how long its last run took".
- **risks:** `noteUsage` fires per chat round and pushes the agents pane with it;
  if that ever gets expensive, coalesce there rather than going back to
  end-of-run. `h.self` is written after `Start` returns and read from the
  callback — safe today because the run goroutine starts after the assignment,
  and nothing runs a turn during `Start`.

### ◻ E. Data race in the TUI input stream (pre-existing)
`go test -race ./cmd/dun` fails `TestInputStreamResetCallback` — a real race
around `newInputStreamFrom` (`cmd/dun/main.go:700`), reproduced on a CLEAN tree,
so it predates this work and is not fallout from it.
- **next:** read the reset path; the reader goroutine and the callback swap are
  the obvious suspects.
- **why it matters:** `-race` failing anywhere means the suite stops being a
  guard against the next one. `go test -race .` (the dun package) IS clean.
- **risks:** none to the fix; the cost is that nobody has been running `-race` in
  CI, so there may be more than one.

### ⏸ D. Slice 5 — sub-agents — **was built through step 5; the CODE IS GONE (❗0)**
`plan/subagents.md` went with it, so this section is now the entire spec — the
decisions below are what a rebuild would work from, and the "verified live"
notes are what it must reproduce, not what it currently does.
A sub-agent is a second `Harness` (Store and Shaper hang off Session, so context
isolation forces the split there).
- **Purpose (USER):** context offloading first, parallelism second. A child does
  the expensive thing — fetch a page, read a 30k-line log — and returns two
  sentences; the tokens that produced them die with it. That is why every default
  here is the cheap side.
- **Decided (USER, 2026-07-31):** custom MCP tools **built INTO dun**
  (in-process) · **tool sets chosen by ROLE** — root gets `agent` +
  `agent_monitor`, child gets `tell_parent` + `ask_parent` and no `ask_user`,
  which is also what enforces depth-1 · `tell_parent(status, message)` — status
  OVERWRITES, message is an event · `agent_monitor` also carries `tell`,
  `resume`, `quit` · async spawn, `wait:true` opts out · a child going idle FIRES
  the monitor, and that is news, so it drives a turn · children are **resident
  until dismissed** and re-askable · **no concurrency cap** · worktree per-spawn,
  default the parent's, and a child there **may write, unenforced** · model
  per-spawn with a config default · `ship` per-spawn bounded by `ship.allow` ·
  session end kills children; on resume they report **stopped** and restart via
  `agent_monitor(resume:)` · the human gets a **dedicated agents pane**.
- **The fact that decided the transport:** `mcpmgr` is request/response only
  (`CallTool` and nothing else — no server→client push). An out-of-process agents
  server therefore could not notify the parent at all. In-process makes
  `tell_parent` a direct `h.Notify` onto the lift path that already exists.
- Role-based tool sets are the ENFORCEMENT, not a convenience: depth-1 holds
  because a child is never handed `agent`, and a child cannot reach the human
  because `ask_user` is simply absent from its set.
- Sub-agents still do not get `ship`'s landing modes — `ship.allow` was already
  the policy surface, so a `work` child is handed `allow:["verify"]`.
- **✅ steps 1–5 (2026-07-31) — the slice is built.** `subagent.go` +
  `plan/subagents.md`.
  - `agent({prompt, model?, wait?})` spawns a child Harness in the parent's
    worktree **sharing its MCP manager** (no second index), on the configured
    child model via a per-model client cache in `cmd/dun`.
  - `tell_parent({status, message, final})` — status overwrites and never
    notifies, message is an event, final is the answer. `ask_parent` blocks;
    `agent_monitor({agent, tail, tell, wait, resume, quit})` answers it.
  - Going idle fires the report onto the lift path, and it is news, so it drives
    a turn. Children stay RESIDENT and re-askable; session end kills them; a
    resumed session reports them **stopped** and `resume:true` restarts them
    from their transcripts.
  - Role picks the tool set in `applyTools` — which is what makes depth-1
    structural and keeps `ask_user` away from children.
  - TUI: an agents pane, live, one row per child with state, elapsed, spend, and
    a loud marker for a blocked one, fed by a new `agents` event.
  - **Verified live** twice against bonsai. Run 1: parent spawned, child ran
    `wc -l`, called `tell_parent{final:"137"}`, parent answered from the report —
    6.5k tokens spent in the child, none in the parent's window. Run 2:
    `wait:true` returned the count, `agent_monitor(tell:)` re-asked the SAME
    child for the last line (correct), tokens accumulated 10.9k → 30.8k across
    both turns, and the pane's events tracked running → idle → running → idle →
    dismissed.
  - **Two things the live runs taught, both fixed:** without `wait` the parent
    improvised `exec("sleep 3")`, and without `wait` on `tell` it called
    `agent_monitor` four times in a row to poll. Both now have it.
  - A nil-harness child would have panicked the process from its own goroutine;
    it now fails that one agent instead.
- **✅ tool renderers (2026-08-01):** `cmd/dun/renderers.go` grew renderers for
  every tool dun owns — `exec`, `exec_monitor`, `ship`, `agent`,
  `agent_monitor`, `tell_parent`, `ask_parent`, `ask_user`. They all collapsed
  to a clipped sentence that hid the verdict. Now the preview carries the
  verdict and the body stays verbatim: exec shows ✓/✗ with the exit status and
  the last INFORMATIVE line (a bare "FAIL" is a verdict, not a reason); ship
  names the checks that ran or failed, because "verified" without saying what
  ran is a claim it has not earned; agent shows state and SPEND, which is the
  argument for delegating at all; tell_parent/ask_* read the ARGS, since
  "status set." says nothing.
  - The exec renderer matches `[exit:` by POSITION (last line), not
    `strings.Contains` — the same trap the exec code was rewritten to avoid.
    Cosmetic here rather than control flow, but a preview that says ✗ about a
    passing command is still a lie.
- **next:** steps 6–7 — `worktree:"branch"` children (own tree, own servers,
  branch hand-off) and per-spawn `ship` bounded by `ship.allow`. Not started;
  the spec says to wait until shared-tree children have run on real tasks, so
  there is evidence about how often one should have been branched.
- **risks:** children are resident until dismissed and there is no cap, so
  forgotten Harnesses accumulate — the pane makes it visible, nothing prevents
  it. A shared-tree child writing freely means the parent's `ship` can find dirt
  that is not its own; the clean-tree refusal does NOT yet name running children.
  The 5m foreground exec timeout still applies inside a child with nobody
  watching (icebox: configurable timeout, condition now reached).

## Decisions
- **A command's outcome is its EXIT CODE; the text is for the model.** Pass/fail
  was `strings.Contains(out, "[exit:")`, which a check that merely PRINTS that
  marker turns into a false failure and which reads as a silent PASS wherever the
  marker is lost. `ExecBackend.Run` returns `ExecResult`; `Failed()` is the only
  question callers ask. `Render()` still emits the marker — the model reads it,
  nothing parses it. Anyone adding a consumer must use the code, not the string.
- **Nothing the model runs in the foreground may block forever, and nothing it
  runs in the background may be cut off.** A foreground command waiting on input
  it can never receive is indistinguishable from slow work (measured: 14
  minutes), so it dies at `defaultExecTimeout`. A background job exists precisely
  to be long, so it is exempt — the two rules are the same rule, applied to
  opposite intents. A caller with its own deadline (ship's `checkTimeout`) keeps
  it.
- **A background job is silent until asked.** Streaming every job by default
  would spend the context window narrating a build one line at a time; saying
  nothing until exit is what made a job that died in second 2 look identical to
  one still working. The resolution is opt-in per job (`exec_monitor` with
  `buffer_bytes`/`grep`/`ignore`), plus a log FILE whose path the model gets — a
  50k-line build log should be grepped with exec, never pasted into context.
  Reports carry only complete lines: the model cannot act on half a line and a
  regexp cannot judge one.
- **A spawned process must be stoppable by name, not just killable by pid.**
  Killing `docker run` does not stop the container — it runs to completion and
  only then does `--rm` remove it. Every run is `--name dun-<pid>-<n>` and the
  cancel path stops that name first. The general form: if a spawn delegates the
  real work to a daemon, killing the client is not cancellation.
- **The system prompt must not describe a tool that is not there.** It described
  `exec` even with no backend configured, and quoted no deadline. Same rule the
  MCP families already followed; the deadline is interpolated from
  `defaultExecTimeout` rather than restated, so the promise cannot drift from
  what the code enforces.
- **Never push anything that was not verified in exactly the state it will
  land in.** bors' "not rocket science rule", and the whole reason ship's order
  is fetch → rebase → checks → push. Verifying BEFORE the rebase — the obvious
  order — tests a tree that will never exist anywhere. The residual hole is
  origin moving between the checks and the push, which is what merge queues
  exist for; ship re-fetches after the checks and goes round again if the base
  moved, bounded at 3 rounds because a permanently busy trunk must fail loudly
  rather than spin.
- **A ship mode is a terminal state, and `allow` is the policy surface.**
  verify/push/pr differ in nothing but where they stop. A repo says "agents open
  PRs, they do not push" by listing modes, which is also how a sub-agent is
  constrained. Pushing the base branch is its own opt-in (`allowBasePush`,
  default off): commits made straight to main are DETECTED and refused, with the
  pipeline still run, because landing on a shared trunk unreviewed should be
  something a repo asks for, not a fallback.
- **Checks are serial waves of parallel commands, and the shape carries the
  semantics** — the array is ordered, each object is not. A wave reports EVERY
  failure in it (compile and lint broken is two things to fix in one turn, not a
  second failure discovered after fixing the first); the next wave does not start
  if one failed. Names are sorted before reporting, or Go's randomized map
  iteration makes two identical runs produce different text, which reads to the
  model as a changed result.
- **A conflicted rebase is a state, not an argument.** Ship used to need
  `action:"continue-rebase"` from the model — one more thing to get wrong when
  git already knows. Detected from `rebase-merge`/`rebase-apply` and resumed.
  The resume must run BEFORE anything reads the branch: a stopped rebase leaves
  HEAD detached, so "what branch am I on" has no answer until it finishes. A
  test caught that ordering; the first implementation had it backwards.
- **Ship must not touch the user's checkout.** The old `FastForwardLocal` ran
  `git checkout <base>` in the *main* repo — silently changing what branch the
  human's working directory was on, and racing any other session doing the same.
  The whole point of worktree isolation, undone by its own tooling. It also
  deleted the branch it had just pushed while never pushing base, so a
  "successful" ship left the work only in the local checkout. Both gone. If
  advancing a local branch comes back, `git fetch . HEAD:<base>` moves the ref
  without checking anything out.
- **No child of dun may keep dun's controlling terminal.** `git push` to an
  https remote opens `/dev/tty` DIRECTLY for its credential prompt — stdin at
  /dev/null stops nothing — and the TUI's Bubble Tea reader is blocked on that
  same tty, so the kernel splits arriving keystrokes between the two readers.
  Measured: a push parked on a prompt for 21 minutes while the session merely
  looked broken, ~40% of taps reaching the UI, git's prompt invisible under the
  alt-screen. Every spawn goes through `detach()` (Setsid + `GIT_TERMINAL_PROMPT=0`).
  `killGroup()` is the other half, not a nicety: a detached child no longer dies
  with the terminal, so cancellation has to take the process group or the
  descendants doing the real work outlive it. Not git-specific — ssh, sudo and
  docker login all do this.
- **The tool set is mutable; four fields derive from it.** Servers start and stop
  mid-session, so `Session.Tools`, `Dispatch`, `System` and `Preparer` are all
  recomputed together by `Harness.applyTools` — never set individually, or the
  next `/rag` silently reverts them. A rebuild that lands while a turn holds
  `turnMu` is deferred to the turn boundary (swapping tools under a running turn
  is a data race).
- **A tool server failing to start is never fatal.** It costs that family and is
  reported (`ServerState.Err`, the startup hint); losing an in-flight session
  because raglit is misconfigured would be the worse outcome.
- **`.dun/` is the per-workspace state directory.** `/rag auto` writes
  `.dun/dun.local.json` (0600 + a `.gitignore`), which is also where per-session
  state goes when one workspace runs several sessions.
- **A spawned server's stderr is evidence, not noise.** agentkit's `mcpmgr` keeps
  a bounded tail and appends it to start/initialize/list-tools errors — the
  protocol layer can only ever say "transport closed", and the actionable line
  is always on stderr.
- **One retry policy, two scopes.** agentkit's `llm.Client` owns the schedule and
  budget (and exports them via `RetryPolicy()`); dun's turn-scope retry reuses
  them rather than inventing its own numbers. Request scope covers everything up
  to the response headers; turn scope covers what it structurally cannot — a
  stream that died mid-generation.
- **Tool docs prescribe only the HABIT that bites.** `toolDocs["node_query"]`
  was written from a corpus, not from the grammar: of 39 distinct failing
  selectors in recorded sessions, 25 were a bare file path where an id belongs
  and 8 a grep pattern quoted twice out of shell reflex. Those two get the
  cheat sheet's first two lines. The corpus lives on as a test in poly-lsp-mcp
  (`TestSelectorCorpus_*`), which is how "is it easier" stays measurable.
- **An unset budget is UNBUDGETED, not a budget of zero.** agentkit's Shaper
  read `BudgetTokens: 0` as a zero-token ceiling, so dun's default
  (`DUN_CONTEXT_TOKENS` unset → 0) compacted on every turn: 45 folds in 29 min
  and 38 in 7 min across two real sessions, 154k chars of summary written, 5 and
  7 entries surviving out of 50 and 45. Fixed in the Shaper; regression test
  pins it. Anyone adding a "sensible default" budget must not reintroduce it.
- **An EOF must name its engine.** Deliberately closing an engine (a session
  switch, /reconnect) still produces an EOF from its reader goroutine, and a
  supervisor that cannot tell that from a crash restarts the engine you just
  replaced — it broke /resume on the first try: three restarts and a give-up in
  the middle of a working switch. eofMsg carries its *dunProc; a stale one is
  ignored.
- **A tool must not index its own state.** dun's per-session worktrees live in
  `.dun/worktrees`, so pointing the code index and raglit at the workspace root
  indexed 16 copies of the repo: every workspace-wide query came back 100% stale
  duplicates. An agent's own isolation mechanism poisoning its own search. Both
  ends fixed (poly-lsp-mcp skips `.dun`; dun ingests the workspace's ENTRIES,
  not the workspace). Watch for this whenever session state moves inside the
  tree.
- **Only the short gaps carry load.** Replay compresses idle gaps (`--max-gap`,
  default 2s) and keeps bursts verbatim: a 4m33s session replays in 9s and still
  stutters where it stuttered. Offsets stay ABSOLUTE after the rewrite, or the
  player's sleep-to-offset scheduling stops being jitter-proof. The replay
  reports what it compressed — silently rewritten time is not evidence.
- **A recorded session is the reproduction.** `DUN_TRACE` records the engine's
  events with offsets; `--replay` drives the real TUI from them. Built after
  three perf findings in a row had to be inferred from after-the-fact
  benchmarks. Replay is also the only UI fixture that is not someone's guess at
  what a conversation looks like.
- **A blocking terminal query is not a "slow render".** The input starvation was
  glamour's auto-style: an OSC background query with a 5s timeout that tmux does
  not answer, rebuilt on every WindowSizeMsg, inside Update. Resolved once
  before the loop starts, skipped in multiplexers. Found only because /perf
  names the message type behind the worst frame — the lesson is that perf
  metrics have to attribute, not just measure.
- **Rendering is paced, and measured.** refresh() re-wrapped the whole
  scrollback once per streamed token — 7.8ms/frame at 200 blocks, ~1s of CPU per
  100-token reply, on the goroutine that reads the keyboard. Fixed by a
  per-block wrap cache (invalidated by width + open) and a 30Hz frame cap; the
  input path is now 3µs/token. Frame stats are always on (`/perf`) because the
  bug was invisible from inside the session; pprof is opt-in (`DUN_PPROF`).
- **Compaction is reported, always.** It is the only operation that DESTROYS
  conversation, and it ran unreported for three days. `OnCompaction` →
  `CompactionNote` → TUI line, stderr, and a `-p` event, with a per-turn counter
  so thrashing (>1 fold in one turn) names itself.
- **Three tiers of buffered news, split by what a turn is worth.** `Say` (user)
  and `Notify` (background job) are news: if nothing picks them up, the driver
  runs a turn. `Aside` (the tool set changed) is context: it rides a tool result
  or joins the next turn, is appended with `store.appendSilent` so it never
  counts as an inbox arrival, and waits indefinitely otherwise. A turn whose
  only output is "ok, noted" is not worth buying.
- **News reaches the model inside a tool result, never as a new turn.** Background
  completions and mid-turn user messages share one buffer (`Harness.Say` /
  `Notify` → `withLiftedQueue`), because a tool result the model is already
  reading is the only free delivery slot — and an assistant message appended after
  another is what providers reject.
- **The TUI supervises its engine, including the first one.** A crash (stdout
  EOF with no `exit` event) respawns it against the same session id, skipping
  the history replay (already on screen) and re-issuing the `/rag`/`/lsp` the
  user had on. A first engine that never spawned does NOT stop the UI opening —
  that is where the error is legible and where `/reconnect` lives — but it must
  not reattach either, or a fresh `dun` would grab an unrelated old session.
  Capped at 3 per 2 minutes — a crash LOOP must be reported, not hidden behind a
  flickering UI. An announced exit is left alone: that one was asked for.
- **The TUI is the default mode; long flags are `--long`.** `dun` bare printed a
  usage line and exited, which made the interactive UI the one mode you had to
  ask for. A positional task still runs headless — scripts must not suddenly
  open a UI. `flag.Usage` is overridden because Go prints `-tui`, which reads
  like `-t -u -i`.
- **The turn budget is a pausable clock, not a context deadline.** Time a human
  spends in `ask_user` is not dun working, so it is not charged (`turnclock.go`,
  `withoutClock`). A deadline context cannot express that — hence the timer.
- **`--timeout` bounds a TURN interactively, the RUN one-shot.** A wall clock on
  a session a human is sitting in front of measured the wrong thing: at the
  deadline the session context died, every later turn failed instantly on it,
  and the reader loop exited — while the UI advised "send a message to retry".
  A hung turn is the thing worth cutting off. The engine now exits only on
  ctrl-C, an explicit stop, or stdin EOF, and announces which (`exit` event).
  Auto-continue after a failed turn is disarmed until something new arrives, so
  a down provider cannot spin the loop.
- **A failed turn is recoverable, not terminal.** The conversation is on disk, so
  the fix is to make the history structurally valid again (pair off orphan tool
  calls, re-kind orphan results) and let the next message resume.
- MVP is a Claude-Code-like **Bubble Tea TUI** (not a one-shot CLI) — but built
  engine-first (Slice 1 headless) then wrapped.
- Safety model = **Docker container + git worktree** isolation, not per-action
  approval prompts.
- The 3 tools are sibling MCP servers bridged into ONE Session (NOT nested inside
  mcpshell's `--mcp` composition — the model should call node_edit/search
  directly).
- LLM: any OpenAI-compatible endpoint; default corrallm/bonsai
  (`ternary-bonsai-27b`, confirmed tool-calling).
- Tool servers are declarative (`dun.json`/`dun.local.json`) over the built-in
  defaults; Slice 3 moves them into the container image.

## The gap it fills
None of the four runs arbitrary commands (build/test/git) — mcpshell is
sandboxed, poly-lsp only gives diagnostics. dun's exec tool (Slice 3) is the
command-runner, made safe by the container.
