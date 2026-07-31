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

## Active work

_None in flight._ Next slice is a decision (see below). Carry-over ◻ items live
in `icebox.md` — pull one here (with next/risks) when picking it up.

### ◻ Slice 5 — roles / task DAG (if wanted)
- Planner/coder/reviewer; multi-Session orchestration (autowork3-style).
- **next:** decide whether this is the direction before speccing — it's the one
  substantial unbuilt slice. Otherwise the backlog is polish (see `icebox.md`).
- **risks:** multi-Session orchestration is a large surface; agentkit owns the
  tool loop but not cross-session hand-off — that's net-new.
- **blocking decision (USER):** is dun a single-agent harness (done) or does it
  grow into a role DAG? Everything in `icebox.md` is viable without Slice 5.

## Decisions
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
