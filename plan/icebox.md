# dun — icebox (deferred / opt-in)

> Next-steps deferred out of active work. Pull one into `plan.md` (with a
> concrete **next** + **risks**) when you start it. Every entry names a RESUME
> CONDITION — something checkable. An entry with no condition is a wish; delete
> it instead of parking it.
>
> Utility reviewed 2026-07-31. Three items were promoted out and are now DONE
> (exit codes, background-job visibility, the docker timeout leak — see
> `done.md`). What remains is either genuinely conditional or honestly
> low-value, and is marked as such — a long icebox of unranked "maybe"s is how a
> plan stops being read.

## Conditional — worth doing when the condition fires

### gh, restored on a call path that cannot hang
`ship` no longer invokes gh (see `done.md`). The removal is deliberate but
temporary: opening the PR was genuinely useful, and printing a command a human
must copy is a step backwards from that.
- **What made it unsafe:** gh 2.45 does not gate its OAuth device flow on
  `CanPrompt()`. With no tty it does not fail — it prints a one-time code into a
  pipe and polls GitHub until the code expires. There is no flag that says "never
  do that".
- **Conditions for bringing it back** (need all three): every call is a
  non-auth subcommand; auth is PRE-flighted (`gh auth status`, treat non-zero as
  "no PR today" rather than as something to fix); and the call is bounded by the
  same deadline everything else now has. Talking to the GitHub API directly with
  a token satisfies all three trivially and is the likelier answer.
- **Resume condition:** someone wants automatic PRs enough to accept the preflight
  cost — or dun grows a credential story of its own.

### MCP servers inside the container
poly-lsp-mcp and mcpshell run HOST-side over the mounted worktree, while the
`exec` tool runs contained. So "safety model = Docker container + git worktree"
is currently half true: the sandboxed tools are the ones that never needed the
sandbox.
- **Resume condition:** the first time `--docker` is used for actual containment
  (an untrusted task, a shared machine) rather than for a reproducible toolchain.
  Until then this buys nothing — the threat it addresses is not present.
- Note this is the larger half of what is left of Slice 3.

### Configurable foreground exec timeout
`defaultExecTimeout` is a 5m package constant, sized for a session a human is
watching. Sub-agents (plan D) and slow toolchains are both reasons it might need
to be per-repo (`dun.json`) or per-spawn.
- **Resume condition:** REACHED — sub-agents (plan D) are being built, and a
  child running a long build hits the limit with nobody watching. Settle it in
  that slice; it is no longer a knob nobody has needed.

### Co-reader detection on dun's own tty
dun could notice it is not the only process reading its terminal and say so; that
failure was invisible from inside the session and took /proc archaeology to find.
- **Deferred on purpose:** `detach()` should have made it impossible. Speculative
  instrumentation until a real report says otherwise — and note the 2026-07-31
  hang was the OTHER shape (a detached child that never wanted the tty at all),
  which this would not have caught.
- **Resume condition:** a user reports dropped keystrokes again.
- **Evidence when it happened:** grabber identified by `/proc/PID/fd` holding
  `/dev/tty` + `wchan == n_tty_read` + matching `tty_nr`. A naive check on
  controlling-tty alone false-positives on every child dun spawns — the
  blocked-in-tty-read condition is the discriminator. Portable partial
  alternative: watch for termios drift from what Bubble Tea set (git's
  `disable_echo` does `tcsetattr(TCSAFLUSH)`, which also discards queued input).

### Per-session resource caps (launcher)
- **Resume condition:** sub-agents land (plan D). One session per human is
  self-limiting; N spawned sessions sharing a machine is not.

## Low value — recorded so they stop being re-proposed

These are all real, all small, and none of them is worth a session on its own.
Do one only if you are already in that file.

- **Hot-reload Starlark renderers** — loaded once at TUI start; a restart is
  cheap and renderers change rarely.
- **`integrate` ship mode** (advance the local base after a push) — one command
  a human runs. If it ever happens it MUST use `git fetch . HEAD:<base>`, never
  `checkout` in the main repo; that was the bug in the old `FastForwardLocal`.
- **fsnotify instead of mtime polling** for the launcher's rebuild watch — a 2s
  poll is not costing anything measurable.
- **`/reload` for web sessions** — would re-exec inside the PTY; untested, and
  reconnecting works.
- **multi-viewer** (one driver + N watchers on a session) — v1 is 1:1 and no one
  has asked to watch.
- **`dun -d status` TUI** — the current line list is legible.
- **cross-host launcher control** — remote reach already exists via `dun -serve`.

## Recap → a project memory in raglit (USER, 2026-08-02)

A recap already produces the one thing a project memory wants: a distilled,
corrected account of a stretch of work, written by the agent that did it and
confirmed by the human. Today it dies with the session. Ingesting it into the
project's raglit index would make it available to every LATER session through
the proactive-RAG path that already exists (`agent.DocFinder` → agentkit
`FinderPreparer`), which is how it would reach a model without anyone
remembering to ask.

**Why it fits:** the confirmation step is already a human saying "this account is
correct", which is a higher bar than anything else dun would ingest. And the
sidecar keeps the raw churn, so a memory can always be checked against what
actually happened.

**Why it is not free — the reason this is iceboxed rather than done:**

- **A recap can be WRONG.** Measured twice on the same task: the model wrote
  "3000 lines" for a 4000-line file, and the human (me, driving a script)
  approved it. In a session that is a local error; in a project index it is a
  permanent one that surfaces in unrelated sessions months later, carrying the
  authority of having been confirmed. The failure mode is worse than not
  indexing at all.
- **Invalidation needs an owner (USER named this).** update/delete implies
  something notices a memory has gone stale — code changed, a decision was
  reversed. Nothing in dun watches for that today. The cheap version is a memory
  citing the commit it was written against, so a reader can see how far the tree
  has moved since.
- **Scope.** A memory is about a PROJECT, but a session runs in a worktree that
  is thrown away. Whatever is written has to land in the checkout's index, not
  the worktree's — the same rule `.dun/dun.local.json` already follows for
  /rag auto.

**Sketch:** `recap({…, remember: "…"})` — an explicit second field, not the
summary itself, so what enters the index is chosen rather than inherited. Then
`/memories` in the TUI to list, edit and delete them, which is the only honest
answer to invalidation until something can detect staleness on its own.
