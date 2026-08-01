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
