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
  ├─ mcpmgr spawns servers on the HOST (stdio JSON-RPC):
  │     poly-lsp-mcp mcp --root /work · mcpshell mcp --files-dir /work · raglit serve
  ├─ Docker CONTAINER (when /docker on): exec tool only
  │     `docker run -v worktree:/work` → build/test/git (safe, contained)
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

### ✅ 8. The pane grew and the offset stayed behind (2026-08-26)
Follow-up to the bottom-anchor fix (026acbb), reported from a phone: open the
soft keyboard, close it, and the conversation is no longer at the bottom.

- **Not the anchoring math.** A sweep of every park position against five
  keyboard heights round-trips exactly. The bug is that `maxYOffset` is
  `len(lines) - Height`, so GROWING the pane SHRINKS it — and every height
  change assigned `vp.Height` directly. `SetYOffset` clamps; a bare field
  assignment does not, so the offset was left past the end of the content.
- **Why nothing caught it.** `AtBottom` is `YOffset >= maxYOffset`, which
  answers TRUE from outside the content — so the pane reported itself pinned
  while drawing blank rows under the last line, and no policy pulled it back.
- **Why only sometimes.** A non-streaming grow is rescued by `applyScroll`'s
  `GotoBottom`. Mid-stream it takes the "keep the last user message in view"
  branch, which leaves `YOffset` alone — so the failure needed a reply to be
  arriving, which is exactly when a phone user closes the keyboard.
- Fix: `convoPane.setHeight` assigns and re-clamps, used at all four sites that
  set the height (the Update funnel's two branches, the width path, and View).
  Measured before the fix: `yoff=44 max=32`, 12 rows past the end.
- **next**: confirm on the actual Android session — the reproduction here is a
  simulated resize, not a real terminal.

### ✅ 7. Every exec is a job; a slow one hands itself over (2026-08-26)
A live session sat "working" for ten minutes with ZERO requests to the LLM,
twice in one hour. It was not slow and it was not thinking — it was blocked in a
foreground exec, and nothing in the loop could tell the two apart.

- **Root cause, three layers.** `TestHostShell_ExitCode` sends `exit 7` into the
  persistent shell, which exits the SHELL, so the sentinel line the reader waits
  for is never printed. The escape hatch (`dead()`) could not fire: it read
  `cmd.ProcessState`, which Go only fills in after `Wait`, and nothing ever
  waited. A first fix added a waiter goroutine that took `h.mu` — held by `Run`
  for the whole command — so it never reached its `close(deadCh)` either. Same
  hang, three mechanisms. And nothing bounded the tool call: `-p` mode has no
  turn clock (`turnTimeout` is interactive-only), so `go test -timeout 10m` was
  the only thing that ever ended it.
- **The shape of the fix is the point.** There is no foreground path any more.
  Every command starts as a job (`Harness.startJob`); the only question the tool
  call asks is who reports it. Finishes inside the grace → output inline, one
  round trip, the job never surfaces. Doesn't → it is PROMOTED: a number, a log,
  a pane row, a notification on completion, `exec_monitor(job:N)` meanwhile.
  Nothing is killed and nothing re-runs. The worst case is now a job the model
  was told about, not a turn that never ends.
- **Grace = 30s**, `timeout:` overrides it, `background:true` promotes before the
  command even starts (which removes the race where a fast command reports
  inline to a caller that asked for a job).
- **Donation** is what makes promotion possible. The promoted command keeps the
  shell it is already inside; `HostShell` drops its reference, releases the lock
  and starts a fresh shell for whoever is next. That is what let the stateless
  `bgBackend` swap in `servers_runtime.go` go away — one backend for everything,
  and a long build can no longer hold the shell every other command serializes
  on. Cost, tested and documented: exports made before a handover are gone from
  the fresh shell.
- Also fixed on the way through: `sh`'s stdout+stderr now share ONE `os.Pipe`
  we own instead of two `StdoutPipe`s (Wait closes the pipes it created, which
  was eating the tail of any command that killed the shell); `Rehoist` closes
  the shell it replaces instead of leaking it; the two `TestRehoist_*` tests
  still asserted `HostExec` after the host backend became `*HostShell`.
- **Relationship to item 2 below.** That removed a 5-minute deadline because
  killing a legitimately-long build is worse than letting it run. Still true —
  which is why this is a HANDOVER, not a deadline. The bound is on how long the
  MODEL waits, never on how long the command may take.
- **Per-agent, which it was not.** The goal said exports persist *per agent*.
  `childConfig` copies the parent's Config, and the shell is a POINTER — so
  every sub-agent shared one shell with its parent: a child's `export` landed in
  the parent's environment and its siblings', and all of them queued behind each
  other. Children now get their own `*HostShell` on the same directory, and
  `Harness.Close` reaps it.
- **risks**: promotion is only wired for `*HostShell`; a stateless backend has
  nothing to donate, which is correct but untested end-to-end under `--docker`.
  The pre-handover output a provisional job buffers is capped at `bgEarlyMax`
  (1 MiB); past that a promoted job's log starts at the promotion point.
- **next**: run a real dun-on-dun session against this and confirm the ten-minute
  silences are gone.

### ✅ 6. One missed ask disarmed the whole session (2026-08-24)
Restarting the dun-on-dun session on the fixed binary proved the measurement
half and found the protection half unreachable — the same shape as the bug it
fixes, one layer up.
- **The calibration is right and the old error was worse than recorded.**
  `prompt 367532 chars → 187003 tokens (1.97 chars/token); the 4 chars/token
  default had estimated 91883 (-51%)`. That session had been sitting at
  **187,003 tokens in a 188,160 window** — 1,157 tokens of headroom — while
  every number it reported said half full.
- **And none of it was armed.** The startup ask for the window happened at a
  moment the model was asleep, timed out once, and the session then ran with
  `window: 0`: no shaping, no generation cap, for its whole life, behind one log
  line. The turn survived on luck (a 172-token reply). Asked again three minutes
  later the same endpoint answered in **38ms**, and repeat startups — bare, with
  all three MCP servers, and while a turn was in flight — all resolved 188160.
  So the miss was transient and the punishment was permanent.
- **Fixed by asking again.** `ensureWindow` runs in `measuredBuild` — the one
  place that runs before every request, on the turn's goroutine — and adopts a
  window mid-session. Shaping and the cap arm together because both are computed
  from `h.window` a few lines below. `h.window` became `atomic.Int64`: it is no
  longer written once before any turn exists.
- **The backoff is in BUILDS, not seconds** (1, 2, 4, 8, 16 → asks at builds 1,
  3, 6, 11, 20, 37). A build is what a missed window actually costs; a fixed
  minute is either idle time on a busy session or a needless timeout on an idle
  one. Bounded at `windowRetries` = 6, so an endpoint that will never state a
  window costs six timeouts over a session rather than one per round.
- **✅ VERIFIED LIVE** with a proxy that refuses the first four `/props`
  requests and then serves them (`scratchpad/flaky_proxy.py`): startup refused →
  "no context window known … asked again on the next build"; build 1 refused;
  build 2 silent (backoff); build 3 → "context window resolved mid-session (ask
  2 since startup)" → 188160 → the very next build logged **`response capped at
  32768 (185044 left in the 188160 window)`**.
- **risks:** the ask is silent about WHY it failed — agentkit's `ContextWindow`
  returns `(int, bool)`, so a 503 and "this endpoint states none" are the same
  answer, and both are retried. That is why the bound exists. A window adopted
  mid-session does not retroactively shape the prompts already sent.
- **next:** nothing outstanding.

### ✅ 5. The freshness check could not see the engine (2026-08-24)
Every fix in slices 3 and 4 was live in the tree and absent from the running
process. The live dun-on-dun session took the *exact* failure slice 3 fixes — a
tool call cut at 2,329 chars, retried, cut at 836 — with no overflow
notification and no fold escalation, because the binary predates them. Two
independent causes, and only the second is code:
- **`dun` on PATH was a plain `go install` build** (2026-08-10 16:05, version
  `dev`, `srcDir` unstamped). Unstamped disables BOTH self-update
  (`selfupdate.go`) and the daemon's central builder (`launcher.go`
  `buildIfStale` returns on `srcDir == ""`). `~/.dun/launcher.log` proves it:
  1,215 `rebuilt →` lines, the last on 08-10 16:04, every `launcher up` since
  reading `(dev)`. Eleven dun commits and five agentkit commits never reached a
  running process. Fix is `make install` (the launcher SCRIPT), not a code
  change.
- **`sourceNewerThan` walked only the dun tree.** dun's engine is a `replace`d
  module living outside it, so an agentkit-only edit left the check reporting
  "fresh" forever — the one case that matters most, since agentkit is where the
  context/retry work lives. `tools/dun.sh` already parsed the replace lines;
  the Go side did not, and the two disagreeing is what made "always the latest"
  false. Now `localReplaceDirs` reads the same lines (single and block form,
  comments stripped, non-path targets skipped) and each target is walked too.
- **Nested modules are pruned by their own `go.mod`, not by name.** The old
  walker hardcoded `tools` — correct for dun (`tools/ttydrive` is a separate
  module) and wrong for any replaced module that happens to have a `tools/`.
- **risks:** mtime-based, as before — a `git checkout` that rewrites timestamps
  triggers a rebuild that changes nothing (cheap), and a clock skew between
  trees could suppress one. The line scan is not a go.mod parser: a replace
  target written across lines would be missed. Both are strictly better than
  the previous behaviour, which was to never look.
- **next:** `make install` on this box, then restart the Aug-10 `dun -d` so the
  daemon itself is not the stale one.

### ✅ 4. A turn retry that keeps its place in the queue (2026-08-24)
The endpoint is single-slot (`maxConcurrent: 1`), and dun retries at two levels:
`llm.Client` retries a REQUEST (up to response headers — 429, 5xx, gateway
down), `runTurn` retries a TURN (a stream that died mid-generation, which is not
resumable). corrallm could not tell the two apart from the outside: every turn
retry arrived as a brand-new caller, so the waiting the earlier attempt had
already done was forfeited, and a fresh arrival from anyone else outranked it.
Measured on the live box: **dun took 175 queue-timeouts in 7 days.**

- corrallm mints a signed ticket when it turns away a request without one and
  returns it on the 429; agentkit echoes it inside its own retry loop and
  exposes it as `RetryEvent.BP.Ticket` + `ChatOpts.RequestID`.
- `wireRetry` now REMEMBERS that id (it was only being rendered), and `runTurn`
  puts it on `Session.ChatOpts` before every attempt — before, not after the
  failure, because the id is issued during an attempt's own request-scope
  retries and the NEXT attempt is the one that must present it.
- **The ticket is cleared when the turn ends, however it ends.** corrallm derives
  queue age from the ticket's own mint time, so one held past its turn would
  keep claiming credit for waiting that finished minutes ago — a later,
  unrelated turn would jump the queue on a debt somebody else paid.
- Mutating the shared `*llm.ChatOpts` is safe here for the same reason it is in
  `applyGeneration`, and for no other: `agent.Session` copies it by value at the
  top of every round, on the turn's own goroutine.
- **risks:** the id is only remembered while the client is an `*llm.Client` (the
  hook is a property of that transport). A different runner gets no ticket and
  the previous behaviour, which is correct rather than degraded.
- **next:** watch a live queue-timeout followed by a turn retry and confirm
  corrallm's Journeys panel shows ONE journey with two attempts rather than two
  journeys with one each. That is the whole observable.

### ✅ 3. The context window has two halves, and dun budgeted one (2026-08-24)
Found in a live yscr session: a tool call cut off after 1278 characters of
arguments with `finish_reason=length`, retried, cut again identically. Three
defects, stacked — each invisible on its own.
- **Every token number was `chars/4`.** Measured against the session's own bytes
  (llama.cpp `/tokenize`, local-Qwen3.8-27B): 60,000 chars → 21,636 tokens. The
  estimate was 31% low, so the shape target (159,344 est) was ~230,000 REAL
  tokens against a 188,160 window. **LOD, compaction and `rescue.go` were
  unreachable by construction** — the first thing to notice the wall was the
  model, mid-generation.
- **The window was spent as an input budget.** 90% to the prompt, an
  uncommunicated 10% to the response, and no `max_tokens` sent at all — with
  llama.cpp's `n_predict` defaulting to -1, nothing ended a generation but the
  slot filling. ~109k prompt + ~79k of thinking = 188,160.
- **The provider reported the truth every round and nobody looked.** `Usage`
  carries `prompt_tokens`; `Usage.Active` was `chars/4` too.

Fixed: `tokencal.go` (measure the ratio from `Usage`), `outbudget.go` (reserve
the response's room and SEND it), `overflow.go` + agentkit's `StopReasonLength`
(the cut is an event, not a log line).
- **The ratio is AFFINE, not one number** — the correction the first live run
  forced. `prompt_tokens` includes the tool schemas (own request field, in no
  message) and the chat template; dividing that constant into the character
  count spread a constant across a variable. Six live rounds: a single ratio
  swung 1.26 → 1.66 chars/token purely from amortisation, and the last one
  predicts **193,079 tokens for a 301,203-char prompt — over the window, every
  turn, forever** (the 45-folds-in-29-minutes failure). Fitted as two terms:
  2.30 chars/token marginal + 1,617 per request → 132,699. The marginal figure
  agrees with the independent `/tokenize` measurement (2.77); the gap IS the
  overhead. `Estimate` reports the marginal cost only — the constant is charged
  once, against the budget, because the Shaper calls `Estimate` per message.
- **compact vs. hint is the decision that matters.** Both failures wear
  `finish_reason=length` and the remedies are opposites: a prompt that ate the
  window is only fixed by folding; a response that ran away is only made worse
  by it (history destroyed to make room the model spends the same way). The
  threshold is one response's reservation of room left; a second cut in the same
  turn escalates to folding, because a hint that did not work will not work
  twice.
- **The hint carries the arithmetic** (window, prompt, room left, what the reply
  reached). A model told "be brief" without them cannot judge how brief.
  Delivery needs no new mechanism: it is a tail-kept notification tagged
  `agent.OverflowTag`, so it lands beside the error result when the cut was mid
  tool-call and rides the next turn when it was mid-reply. It is DISCARDABLE —
  written in the present tense about the last response, so `prepareTurn` drops
  it; left in place it teaches a permanent, unexplained timidity.
- **✅ `/context` now shows it (2026-08-24).** The view could itemise the
  pre-conversation cost in five rows and could not say how big the window was —
  so a reader saw WHAT was expensive and never whether it was near the wall,
  which is how this went unnoticed for a session. New `window` block: size,
  prompt vs budget with a percentage, the measured per-request overhead on its
  own row, the response reservation, room left (flagged when it drops under the
  reservation), the ratio WITH whether it was measured and over how many rounds,
  and a cut count. `dun.WindowBudget` + `Harness.Window()` carry it; the two
  duplicated `usage` event literals became one `usageEvent`.
- **✅ VERIFIED LIVE (2026-08-24, local-Qwen3.8-27B via corrallm).** Forced with
  `DUN_MAX_OUTPUT_TOKENS=60`, a cap no response can fit, so the cut is
  deterministic whatever the model does. Every rung fired:
  - the cap is SENT — `response capped at 60 (178816 left in the 188160 window)`;
  - calibration on round 1, then the fit converging **2.37 → 2.32 → 2.28 → 2.27
    → 2.25 → 2.24 chars/token marginal + 1470 → 1266 per request**, with the
    per-round estimate error at **-1% … +1%** once fitted (it was 69% low on the
    uncalibrated first round);
  - `finish_reason=length` → `StopReasonLength` → narrated, not silently retried;
  - **both branches**: a cut TOOL CALL (`[recap]`, `[ask_user]`) and a cut REPLY
    — the latter being the one that used to end a turn on half a sentence;
  - attempt 1 → hint only, correctly diagnosing a response problem (176,968 of
    room left); attempt 2 → escalated to folding 20 entries; attempt 3 → the cap
    fails the turn with the evidence, non-fatal and resumable;
  - the model-facing hint lands as a notification carrying the real arithmetic.
- **Two defects the live run found, both fixed** — and both the same failure
  this slice is about, a number or a diagnosis that LOOKS measured:
  - **the thrash warning blamed the budget for overflow folds.** Two folds in one
    turn on a 9,824-token prompt against a 178,354-token budget printed "the
    context budget cannot fit one turn's floor. Raise DUN_CONTEXT_TOKENS" —
    wrong in every particular. `foldCause` now splits the counter, and an
    overflow-driven thrash names the endpoint instead.
  - **every rescue logged `0 → 0 tokens (saved 0)`.** The fold path never set
    TokensBefore/After, so the one place a reader checks whether a fold was worth
    its LLM call reported a measurement-shaped zero. Now measured with the meter.
- **next:** nothing outstanding. `/context`'s window block has not been seen
  against a long real session (only synthetic and short live ones); it renders
  from the same numbers the log prints, which were verified.
- **risks:** `defaultMaxOutputTokens` (32k) is reasoned, not measured — it is
  also the reserve subtracted from the prompt, so it trades context for headroom.
  `DUN_MAX_OUTPUT_TOKENS` overrides. The fit is cumulative over a session, so a
  hard change in text mix lags. `applyGeneration` mutates the shared
  `*llm.ChatOpts` from the context builder: safe only because `agent.Session`
  copies it by value at the top of each round, on the same goroutine.
- **blocking decisions:** none.
- **optional extensions:** none outstanding. Turn-level loop detection was the
  one named here; it is slice 4, and what it can and cannot see is recorded
  there.

### ✅ 4. Turn-level loop detection — and the two detectors that failed (2026-08-24)
`llm/repetition.go` catches a loop inside ONE generation. Nothing caught the
loop across rounds: every response well-formed, every tool call valid, the
sequence going nowhere. `loopguard.go` does, and the useful part of this slice
is what the measurement REJECTED. Three candidates, tried against the stuck
yscr session:
- **near-identical arguments — rejected.** The visible churn was six attempts at
  one failing test; consecutive `exec` arguments scored **0.01–0.29** similarity.
  Each attempt was a genuinely different edit. Syntactic similarity cannot see
  semantic churn, and the original plan for this slice assumed it could.
- **recurring result lines — rejected.** Lines recurring across a sliding window
  of 8 results: **3/8 healthy, 4/8 stuck**. No threshold fits between them
  without refusing normal debugging, where the same failure legitimately recurs
  while you work on it.
- **identical arguments, CONSECUTIVE — accepted**, and it found a loop nobody had
  noticed: **12 back-to-back `recap` calls with byte-identical arguments** (calls
  39–50). The first folded 379 entries; the eleven after it each folded ONE,
  writing `recap17`–`recap26`, 2,523 bytes apiece. All of them nothing.

Consecutiveness is the whole precision argument: the same session called `ship`
identically 8× and `git log …` 3×, all legitimate, all separated by gaps of 3–48
calls. Zero false positives at any threshold above 2, on the evidence available.
- **Refuses, does not warn.** A warning is a round trip that ends with the model
  deciding, and the observed loop ran 12 deep with every call SUCCEEDING — no
  failure for a warning to attach to. The refusal is a tool result, paired to
  the model's own call, so it needs no new delivery path; it quotes the previous
  result back, because a model told only "refused" reads a transport failure and
  retries. At +2 more it forces `ask_user` (once) via `ForceToolCall`.
- **Polling tools are exempt** (`exec_monitor`, `agent_monitor`, `ask_user`):
  repetition is their success mode. A poll also RESETS the run, since waiting on
  something is exactly "the world changed in between".
- **It lifts the queue itself.** It sits outside `withLiftedQueue` so it can see
  every tool, so a refusal would otherwise strand a message the user typed while
  the agent looped — the worst moment to drop one.
- **known limitation, stated in the file:** it does NOT catch semantic churn.
  Six different edits chasing one failing test still go unnoticed, and nothing
  here should be read as claiming otherwise.
- **risks:** `defaultLoopRepeats` = 3 is judged, not fitted — the only observed
  loop ran to 12, so anything from 3 to 10 would have caught it and 3 is the
  earliest that is clearly abnormal. `DUN_LOOP_REPEATS` overrides; 0 disables.
  A tool outside `pollingTools` that is legitimately called identically in a row
  would be refused; none exists today.

### ✅ 2. Exec timeout removed — monitor heartbeat catches wedged commands (2026-08-04)
The `defaultExecTimeout` (5m) was a blunt instrument: it killed foreground
commands that were legitimately long, and the only escape was `background:true`.
The resolution is to remove the timeout entirely and rely on the monitor
heartbeat (the same mechanism that catches wedged background jobs and sub-agents)
to notify rather than kill. This avoids expensive throwaway work — a build that
ran 4m 50s before being killed is worse than one that ran 5m 10s and was
reported as wedged.
- Removed `defaultExecTimeout`, `noTimeoutKey`, `WithoutExecTimeout`, `bound()`.
- Removed `TimedOut` and `Limit` fields from `ExecResult`.
- Simplified `finish()`, `Render()`, `Failed()`, `jobState()`.
- `HostExec.Run()` and `DockerExec.Run()` no longer call `bound()`.
- `startBackground()` no longer exempts from a timeout that no longer exists.
- Updated tests: removed `TestHostExec_ForegroundDeadlineKillsAndReports` and
  `TestBound_DefaultsOnlyWhenNothingElseSaysOtherwise`.
- System prompt already said "no time limit" — no change needed there.

### ✅ 0. Slice D and the activity zone — REBUILT (2026-08-01)
The deletion below happened; the recovery is done. The tracked half came back
from `stash@{1}` and applied cleanly except `harness.go` and
`servers_runtime_test.go` (both 3-way merged). `subagent.go`, `bgjob.go` and
`cmd/dun/activity.go` were rewritten from the D and G sections of this file,
which is the argument for keeping decisions here rather than only in code.
- **What is NOT back:** `plan/subagents.md`. The D section below is now the
  whole spec, and the live-run notes in it describe what a rebuild must
  reproduce, not what has been re-verified since.
- **Re-verified live against llm.iodesystems (2026-08-04).** Everything is
  unit-tested, `-race` clean, and the rebuild was validated end-to-end.

### ❗ 0b. What was lost on 2026-08-01, and why
Kept because the LESSON is the point, not the loss. A cherry-pick run from outside the
session stashed the tracked files and cleaned the untracked ones; `git stash`
does not take untracked files, so these were never captured by anything:
`subagent.go` (1108) · `subagent_test.go` (702) · `bgjob.go` (415) ·
`bgjob_test.go` (283) · `plan/subagents.md` (543, the spec) ·
`cmd/dun/activity.go`. Not on any of the 47 branches, not in the dangling
objects, not in `.dun/worktrees`. **Unrecoverable** — rewritten, not restored.
- What survived: these plan docs (restored from `stash@{1}`), so the DECISIONS
  live even though the code does not. `plan/subagents.md` did not — the D
  section here is now the whole spec.
- The tracked half (`harness.go`'s hooks, `exec.go`'s `ExecResult`, `ship.go`,
  the tool renderers) was in `stash@{1}` and could not compile on its own,
  since it referenced the deleted types — which is why the recovery had to be
  "apply the tracked half, then rewrite the rest", in that order.
- **resolved (USER, 2026-08-01):** rebuilt. See 0 above.
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
- **shift+enter (USER, 2026-08-01): the terminal DOES send it; bubbletea throws
  it away.** Measured: `ESC O M` → `KeyRunes "M"` with alt=false — `ESC O`
  parses as SS3, `M` is not in the SS3 table, and the final byte falls through
  as a bare rune, byte-identical to pressing the letter M. `ESC [13;2u` and
  `ESC [27;2;13~` produce nothing at all. So it cannot be a binding, and is
  rewritten one layer down (`keyfilter.go`) to LF = ctrl+j, which is already the
  newline key — a spelling, not a meaning. Two traps, both tested: a lone ESC
  must pass through immediately (holding it to see what follows makes the
  Escape key wait for the next keystroke), and raw mode becomes OURS, because
  bubbletea only raws a tty `*os.File` and a wrapped reader is not one — the
  first attempt left the tty canonical and nothing reached the UI at all.
- **verified:** `ttydrive` against the real binary — single render, growth to
  two rows on wrap, ctrl+w/ctrl+u, alt+enter. Plus 12 tests, three of them
  regressions for the defects above.
- **risks:** wrapping measures RUNES, not display columns, so a CJK or emoji
  line wraps a few columns early. Pre-existing; not worth fixing until someone
  types one.

### ✅ G. The activity zone — one navigable tree over agents + jobs (2026-08-01)
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
  ✅ **Extended to the CONVERSATION blocks (2026-08-03).** The activity tree and
  docs lists obeyed it; a folded tool block wore the same `▸` and could only be
  opened with enter. Now `→` walks minimized → expanded → raw one level per
  press and `←` walks it back, via `convoEntry.deeper()/shallower()` —
  deliberately NOT `viewState.Next()`, which wraps: wrapping would make `→`
  close what it had just opened, and would land a block with no raw view on
  `viewRaw`, where `view()` falls back to the collapsed line. `→` still hands
  over to the input, but only from a block with nothing left to open. Enter is
  unchanged (cycle, and inspector for a tool block).
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
- **BUILT.** Steps 1–5 all landed: the `jobs` event, the task + activity header
  lines, the three-zone tab cycle with the `▸`/→/← rule, job detail through the
  existing inspector, and agent scope with a `↰ parent` row and a live input
  that routes to `agent_monitor(tell:)`. 8 TUI tests cover the zone.
- **history — built once, then deleted with slice D (see ❗0b):** step 1 was
  `JobInfo`/`jobState`/`Jobs()`/`jobsChanged()` in `bgjob.go`, `Config.OnJobs`,
  a `jobs` event mirroring `agents`, pushed on start and on finish and NOT in
  between (a push per output chunk repaints on every line of a build log), with
  times crossing as unix seconds so the UI ticks a running job's clock itself.
  Two tests, green under `-race`. `cmd/dun/activity.go` had the zone rendering.
  All of it went with the untracked files.
- **✅ live-verified against Qwen3.6-35B-A3B-MTP (2026-08-04).** Ran `dun -p`
  headless with task "spawn a child agent to count from 1 to 3". Verified:
  child agent #1 spawned with correct prompt, `agents` event emitted with
  state="running", tool call/result pair properly emitted, parent continued
  working after spawn, child properly dismissed on session exit. The new
  `/context` stats fields (`system_tokens`, `forced_calls`, `notifications_lifted`)
  are populated in the `usage` event. The child did not complete because the
  single LLM slot was occupied by the parent — expected with one slot.
- **risk that survived the build:** a child's own background jobs are NOT
  listed in agent scope. Its callbacks are nil by design, so the strip there
  shows only the way back. Honest, but it means "what is this child running"
  has no answer in the UI.
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

### ✅ F. A child wedged with no in-flight request — observability added (2026-08-03)
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
- **✅ make it observable (2026-08-03):** added `lastActivity` to `subAgent`, wired
  via `OnToolCall` callback (every tool call proves the child is not wedged) and
  `noteUsage` (every chat round). The heartbeat fire callback now checks
  `lastActivity`: if the child is `agentRunning` but inactive longer than the
  heartbeat interval, the alert escalates from "still running" to "may be wedged"
  with a suggestion to `agent_monitor(tail:40)`. `startRunLocked` also seeds
  `lastActivity`, so a child that never reaches its first tool call is caught.
- **next:** on the next occurrence, the alert will fire automatically. Still need
  a goroutine dump (`kill -QUIT`) to identify the exact frame.

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

### ✅ E. Data race in the TUI input stream — fixed by dun itself (2026-08-02)
Handed to a dun session as a real task, in an isolated worktree. It found the
cause (the race is in the TEST: `called` is a plain bool written by the scanner
goroutine and read by the test body, with only a `time.Sleep` between them),
fixed it with `atomic.Bool`, and proved it with `go test -race -count=1
./cmd/dun`. 13 turns, 141k tokens, 6.6k active. `go test -race ./...` is now
clean for the first time.
- **residual, not introduced by the fix:** the 50ms sleep is still there, so the
  test is timing-dependent by construction. A channel closed by the callback,
  selected on with a deadline, would remove both the race and the sleep.
- **what the run taught about recap** — the repetition cue fired 4 times on
  three `node_query` calls returning 44, 112 and 375 characters, and the model
  ignored it. Correctly: recapping a few hundred characters saves nothing and
  costs a tool call. Cue now requires the run to have COST something.


### ✅ D. Slice 5 — sub-agents
See `done.md` — steps 1–5 built, verified pre-deletion, rebuilt 2026-08-01.

## Decisions
- **A request nobody asked for must be VISIBLE before it is defensible (USER,
  2026-08-03).** Measured against a counting stub: one user message cost TWO
  LLM calls. The second was `Suggestions()`, fired by the engine at the end of
  every turn, and invisible twice over — it used `DefaultContextBuilder` rather
  than the session's `Build`, so it was the one request in dun that was never
  shaped (no LOD stubs, no compaction) AND never logged, which means every
  `built prompt` line in every session log undercounted the real traffic. Three
  changes: it goes through `h.Session.Build` (shaped + measured); the engine no
  longer volunteers it (`emitSuggestions` is reachable only from the `suggest`
  control command, detached from the reader goroutine because it is a full
  round-trip); and the UI decides when to ask, because the trigger depends on
  facts the engine cannot see. Now 1 call per message, verified the same way.
- **The suggestion trigger is an IDLE DEBOUNCE, not a turn hook (USER,
  2026-08-03).** It fires only on `done` (so nothing can land between a tool
  call and its result), after 3s with no keystroke, with the input box empty,
  and at most once per idle — `len(suggestions) == 0` is what makes "once" hold
  even though a keystroke restarts the clock. Armed from `tuiModel.Update`, the
  one funnel every key passes through, and never armed at all when it could not
  fire (suggestions off, empty conversation) so an ignored keypress stays
  ignored.
- **A heartbeat reports the ABSENCE of news and must not buy a turn (USER,
  2026-08-03).** It went through `notifyAndWake`, so the wake ran `continueTurn`
  — a full model call, plus (until the above) a suggestion call — to say that
  nothing had happened, on a schedule starting at one minute. Reminders now use
  `notifyQuietly`: `Aside` (which by construction can never cause a turn) plus
  the `onNotify` ping so the human still sees the line. The ONE exception is a
  child blocked on `ask_parent` — it waits on an answer only the parent can
  give, so a note the parent reads "next time it runs anyway" is one it may
  never read.
- **What the human sees must be what the MODEL saw.** `OnToolCall` fires inside
  each tool's wrapper, before the outer cap, so the TUI rendered a
  quarter-megabyte the model never read — and "why did it not see the last
  line?" was answered wrongly by the operator's own screen. Reporting goes
  through `cappedReporter`, which forces the clip to run twice, which forces it
  to be memoized by content: two clips would mean two spills under two refs, and
  the human told to read a copy the model has never heard of.
- **A read that finds the answer must not clip the answer away.** Grep on a
  98,894-character single line returned the whole line, the cap took the middle,
  and the match was in the middle — the one read that should have answered threw
  the answer away, and the model then hunted the spill file through `find`,
  which only worked because exec had a host to search. A match inside a long
  line comes back as a WINDOW around it, carrying its offset. Same test caught
  `tail:100` handing back an entire blob: the cap must hold through recap's own
  door, or the tool that manages the window is the one that blows it.
- **Line-based readers are useless on output with no lines, and a byte cut
  through UTF-8 is corruption.** A minified document or one enormous generated
  line defeats grep, head and tail alike — all three hand back the line that was
  the problem — so such a result was visible only at its two ends with no way to
  open the middle. `recap({ref, at:N})` pages it and each page says where the
  next begins. Both clip ends land on rune boundaries; invalid UTF-8 can break
  the transport rather than merely read badly. "Unlined" is the longest LINE,
  not the line count.
- **Bound what ENTERS the window before managing what is in it.** Background
  jobs streamed to a file and returned a bounded tail from the day they were
  written; the foreground path never did, so one `cat` put 255,720 characters
  (~64k tokens) in to answer a question whose answer was "30" — and every later
  turn paid again. Over 20k characters a foreground result is clipped to its
  START and its END, whole thing spilled beside the bg logs, path in the gap.
  Both ends because the foreground path sees both shapes: a failure is at the
  bottom, a listing is useful from the top. Cuts land on line boundaries, and
  the clip is applied AFTER Render so the `[exit: …]` verdict survives in the
  tail. Measured on one task: 456,772 tokens → 32,916, active 65,056 → 6,253,
  and recap became unnecessary. The ordering is the lesson — recap removes churn
  that already cost you, the cap stops it being charged in the first place.
- **A context-management suggestion must name what to remove, and arrive when
  the churn is made.** A prompt line describing `recap` produced zero uses. A
  window-size nudge was delivered and READ — it is in the transcript the model
  was working from — and ignored: a threshold says "you are spending a lot", not
  what to do about what just happened. Firing on the SHAPE of the churn worked
  on the first try (USER's idea): the same tool 3+ times in a row (flailing, and
  the earlier attempts are superseded by definition), or a ≥20k-char result the
  model has since called past (calling past it IS the evidence it took what it
  needed — nudging on arrival would mean discarding what it just asked for).
  Each cue is raised once; a repeated signal is one a model learns to skip.
  Measured, unprompted: 9 entries / 258,256 chars removed, active context
  65,056 → 1,057 tokens.
- **A `keep` term must mean what the model meant by it.** Live: `keep:["exec"]`
  preserved EVERY exec call including the 255,720-char `cat` the recap existed
  to remove, because the model reaches for the tool NAME it knows rather than a
  phrase. A bare name now keeps only that tool's most recent call. And the
  anchor matched an assistant turn ECHOING the phrase the nudge handed over, so
  the span collapsed to 85 characters beside a quarter-megabyte result — a USER
  message is preferred as the anchor. Both were invisible to unit tests and
  obvious on the first live run.
- **The model may rewrite its own recent history, under three constraints.**
  `recap` replaces a span with the account the conversation SHOULD have had:
  cheaper than compaction (no summarizer call), more accurate (the whole span is
  in view), and it fixes the RECORD rather than merely shortening it — a
  misunderstanding removed there stops misleading every later turn. (1) It is
  VISIBLE, not confirmed (USER, 2026-08-03). It used to stop and ask a root's
  human before applying, and that prompt was the wrong shape: approving a
  rewrite of a span you would have to re-read to judge, mid-turn, where the
  honest answer is always yes. A question whose answer is never no is a delay,
  not a safeguard. So every recap applies — root and child alike — and REPORTS:
  `RecapNote.Note` is the citation line, `RecapNote.Detail` is
  `recapSpan.report()` (counts, what was kept, the replacement text), and the
  TUI renders it as a `▸` line that opens onto the detail. Never the removed
  content: re-rendering the churn would undo the recap. (2)
  Nothing is destroyed (USER): the span moves to `<session>.recapN.jsonl`,
  because churn is the evidence for fixing the tooling that produced it, and a
  sidecar that cannot be written ABORTS the recap. The transcript keeps a
  citation entry filtered out of `Context` — a pointer to removed churn that
  itself cost context would be absurd. (3) Tool pairs stay whole: a call without
  its result is the one shape a provider rejects, so the span is planned before
  it is applied, and the live recap call is never inside its own span. That last
  one bit immediately — the anchor search matched the recap call's own
  arguments, which quote `from` verbatim, so every span came back empty.
- **`/worktree` answers about the REPO, not about the isolation mode (USER,
  2026-08-03).** `status` used to reply "none (working in place)" whenever there
  was no dedicated worktree — the common case, since isolation is opt-in — which
  states the mode and answers nothing. It now reports `Worktree.Status()` (the
  porcelain branch line as a header, the changed files, a count) over a
  pass-through `WorktreeInPlace`, and `commit` commits in place too. `new` now
  passes `h.Mounts()` where it passed nil: a worktree lives under
  `.dun/worktrees/`, so a go.mod `replace => ../agentkit` only resolves through
  a symlink beside it, and one made mid-session had none — measured, same
  worktree either way: build OK with the symlink, `replacement directory
  ../agentkit does not exist` without. It reuses the session's RESOLVED list
  rather than re-loading, so the symlinks and the Docker volume mounts cannot
  describe different sets.
- **A commit message is WRITTEN by the model and APPROVED by the human (USER,
  2026-08-03).** `/worktree commit` committed with the literal string
  "/worktree commit" — the one artefact git keeps forever said less than the
  command that produced it. Now: one THROWAWAY tip session (`Harness.
  CommitMessage`, messages built in `commit.go`, never appended to the
  conversation) sees the porcelain status, the diff vs HEAD bounded to 24k with
  the `--stat` always intact, and the last 3 user messages as intent. Untracked
  CONTENTS are omitted — a new file is the largest and least informative part of
  a change. Then it is shown, with commit/regenerate/cancel and 3 rounds; unlike
  a recap, a commit message is written once and read forever. Default format is
  Conventional Commits, overridable per repo via `commit.format` /
  `commit.instruction` in dun.json.
- **A control command that asks MUST NOT run on the reader goroutine.** That
  goroutine is the only reader of `answer` events, so blocking it on a question
  means the answer can never arrive — the session hangs on its own prompt.
  `ctrlCmdAsks()` names the asking commands and `setCtrlCmd` detaches them. Kept
  beside the commands so adding an asking one is a change in that file rather
  than a hang found in production.
- **A worktree may be deleted when it holds no WORK, which is not the same as a
  clean tree.** Measured on dun's own repo: 37 registered, 36 on disk, 1.1 GB,
  one registration already pointing at a deleted directory. By `git status` 34
  of 37 looked dirty — almost all from one untracked index artifact — so a
  status-based prune would have reclaimed three. The criterion is commits the
  repo's HEAD cannot reach, plus edits to TRACKED files, ignoring artifact paths
  (`.dun/`, `.poly-lsp-mcp/`). Work that already merged is not work. Untracked
  files are not work: an agent that never added a file left nothing anyone can
  name. Anything holding work is REPORTED and never removed — a branch with
  unpushed commits may exist nowhere else.
- **Liveness must not disturb what it measures.** The prune skips trees touched
  within an hour, because a machine runs several dun sessions at once and a
  concurrent session's empty-so-far tree is exactly what would be deleted from
  under it. Two traps, both hit: a directory's mtime does NOT change when a file
  three levels down is edited, so liveness reads the newer of the directory and
  git's INDEX; and a plain `git status` REFRESHES the index, which is a write —
  the first scan stamped all 30 trees as "touched now" and the pruner would have
  believed every tree was live forever. `--no-optional-locks` exists for tools
  that poll. Verified by scanning twice and asserting no age got younger.
- **Metadata is written when the fact is known, not when the process ends.**
  `SaveSessionMeta` was a `defer` in main, so it only ran on a clean exit — the
  one path that does not happen when the TUI kills its engine or a human hits
  ctrl+C. Result: 57 sessions on this machine, 0 metadata files, so `--resume`
  could never reuse a worktree and nothing could say which tree belonged to
  what. The path and branch are known the moment the worktree exists.
- **Automatic cleanup may only ever delete what is worthless; a human deletes
  the rest.** The startup prune removes trees holding nothing and reports the
  ones holding work. `/close` is the explicit destructive counterpart — worktree,
  branch and transcript, so `/resume` cannot offer it back. The asymmetry is the
  point: no pass that runs without being asked may decide work is worthless.
- **Work that is not committed does not exist.** `git stash` does not take
  untracked files and `git clean` removes them; a repo with other agents working
  in it is a repo where either can happen at any moment. 3000 lines went that
  way in one command. Scaffolding commits are cheap — commit a new file the
  moment it compiles, before it is finished.
- **The plan is the backup of last resort, so decisions belong in it.**
  `subagent.go` was rewritten from the D section of this file. What could not be
  rewritten was `plan/subagents.md` itself, and the difference between "we still
  have this feature" and "we do not" was whether the reasoning lived somewhere
  other than the code.
- **Nothing dun spawns may wait for a human, and a terminal is not the only way
  it can.** `detach` severed the tty and set `GIT_TERMINAL_PROMPT=0`, which
  stops the credential prompt — and missed the EDITOR. `git rebase --continue`
  runs `git commit … -e`, that `-e` launches `$EDITOR`, and vim held a rebase
  for ten minutes until Go's test timeout killed the tree, after which the agent
  read "FAIL … 600.024s" and hung the same way on its next command. Setsid does
  NOT save you: an editor with nowhere to draw does not necessarily exit, it
  blocks somewhere else. `GIT_EDITOR=true` + `GIT_SEQUENCE_EDITOR=true`. Worst
  of all, it is environment-dependent in the invisible direction — with EDITOR
  unset git refuses with a readable error, so it never reproduces in CI.
- **exec runs a NON-LOGIN, non-interactive shell.** It was `sh -lc`, which
  sources the operator's profile — which is how `EDITOR=vim` reached the agent
  in the first place. The agent is not that human's interactive session and must
  not inherit their aliases, PATH edits or editor: none of it is reproducible on
  another machine and all of it can block. Cost accepted: a tool installed only
  via a profile is not found, which fails loudly instead of differing per user.
- **Anything unbounded must say it is still there, and be debounced by whatever
  it already says.** A background job (exempt from the deadline) and a sub-agent
  (resident until dismissed) are both unkillable-by-policy, so the foreground
  deadline's answer does not apply; they announce themselves instead. Heartbeats
  at 1m/5m/10m/30m/1h then hourly — highest value early, since most wedges are
  immediate; lowest cost late. Every notification the thing sends resets the
  clock, hooked at the ONE place notifications leave (`bgJob.notify`,
  `subAgent.notify`) so a new kind of report cannot forget to count as a sign of
  life. The reminder itself must NOT go through there, or it resets the silence
  it is reporting on. It carries no output: state and duration only.
- **`▸` means descendable, everywhere.** → opens and descends, ← ascends — the
  same gesture on the activity strip, on one of its rows, on a job's log, and on
  the docs blocks in the conversation that already worked this way. This
  replaced horizontal handlers where → descended into docs on one block and
  hopped panes on another. One affordance, learned once.
- **A selection is stored by IDENTITY, not by index.** Activity rows are rebuilt
  on every event and a new agent shifts every job row down, so an index would
  walk to a different row while the human was reading it. The same reason row
  order is stable rather than sorted by interest.
- **A child's conversation is PULLED, never pushed.** Child callbacks are nil by
  design (context isolation); un-nil'ing them to feed a UI would tag and forward
  N streams nobody is looking at. Pull also works unchanged for a STOPPED child,
  whose process is gone but whose transcript is not — which push cannot do at
  all. A reply for a scope already left is dropped.
- **A command's outcome is its EXIT CODE; the text is for the model.** Pass/fail
  was `strings.Contains(out, "[exit:")`, which a check that merely PRINTS that
  marker turns into a false failure and which reads as a silent PASS wherever the
  marker is lost. `ExecBackend.Run` returns `ExecResult`; `Failed()` is the only
  question callers ask. `Render()` still emits the marker — the model reads it,
  nothing parses it. Anyone adding a consumer must use the code, not the string.
- **Nothing the model runs in the foreground may block forever, and nothing it
  runs in the background may be cut off.** A foreground command waiting on input
  it can never receive is indistinguishable from slow work (measured: 14
  minutes). There is no automatic kill: a wedged foreground command is caught by
  the monitor heartbeat (same mechanism as background jobs), which notifies
  rather than kills — avoiding expensive throwaway work. A background job exists
  precisely to be long, so it is exempt. A caller with its own deadline (ship's
  `checkTimeout`) keeps it.
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
  constrained.
- **Ship the branch you are ON, against ITS OWN upstream** (USER, 2026-08-01).
  dun never switches branches — deliberately — so the branch HEAD is on IS the
  branch being shipped and there is no second "base" to protect. `allowBasePush`
  is GONE: it refused to push whenever `head == base`, which in a
  `--no-worktree` session is true BY CONSTRUCTION (base is captured from HEAD at
  startup), so ship ran the entire pipeline and then declined to do its job on a
  branch that was the user's to push. A repo that does not want agents pushing
  says so with `allow`, not with a rule about branch identity. The rebase target
  is the branch's tracking ref; a branch with no upstream falls back to the base
  (what a fresh worktree branch is meant to integrate with), and with no remote
  for that either the checks are the whole of ship. The one head==base check
  that survives is in `pr` mode, because a PR from a branch onto itself is a
  contradiction rather than a policy.
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
