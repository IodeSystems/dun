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
confirmed by the human. Today it dies with the session.

**Shape (USER).** Memories are PASSIVE — surfaced by raglit search when they are
relevant, not loaded as a context file every session. Cost then scales with
relevance instead of with corpus size, which is what makes the whole thing
affordable as memories accumulate. One memory DOCUMENT per project; each memory
is a FRAGMENT; the summary and the answered-questions index are rebuilt as
fragments accrue.

**It maps onto what raglit already is.** `Document{Path, Title, Fragments}` with
idempotent replace-on-reingest IS "one document, memories as fragments, rebuilt
on append". And `origin='identity'` is already a machine-written summary indexed
as its own fragment and ranked beside real text — so the question index is one
more origin, not new machinery. Design lives in
`raglit/plan/answered-questions.md`; this file is the dun half.

**What dun owns:**

- **Writing.** `recap({…, remember: "…"})` — an explicit field, NOT the summary
  itself, so what enters the index is chosen rather than inherited. Recap is the
  right trigger precisely because a human has just confirmed the account is
  correct, which is a stronger signal than any heuristic for "worth keeping".
- **Read-suppression.** A memory already surfaced this session is not proposed
  again. This belongs HERE, in the `FinderPreparer` that does the surfacing, not
  in the index: several sessions share one index, so read-state is the
  consumer's, and raglit staying stateless about its readers is what lets two
  sessions suppress independently.
- **Provenance.** A fragment records the session and the commit it was written
  against. Without it, a wrong memory laundered into the rebuilt summary reads
  as the project's considered view with nothing to trace it back to.
- **Scope.** A session runs in a throwaway worktree, but a memory is about the
  PROJECT — so it lands in the checkout's index, the same rule
  `.dun/dun.local.json` already follows for /rag auto.

**The objection that survives.** A recap can be wrong: measured twice on one
task, the model wrote "3000 lines" for a 4000-line file and the human approved
it. In a session that is a local error; indexed, it resurfaces months later in
an unrelated session carrying the authority of having been confirmed. Deletion
is easy (one fragment, re-ingest converges) — NOTICING is the hard part, and
nothing here solves it. The cheap mitigations are provenance plus the standing
convention that a recalled memory naming a file or a flag is checked before it
is acted on.

**Do not save what the repo already records.** Code structure, past fixes, git
history: re-derivable, and the fastest to go stale. This is the sharp constraint,
because a recap summary is very often exactly a restatement of what the code and
the commit log already say. The memory-worthy residue is what is NOT in the
tree — why an approach was abandoned, what a measurement showed, which reading
of an ambiguous request turned out to be the right one.

**Then `/memories`** to list, read and delete — the only honest answer to
invalidation until something can detect staleness on its own.
