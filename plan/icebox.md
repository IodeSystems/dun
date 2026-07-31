# dun — icebox (deferred / opt-in)

> Next-steps deferred out of active work. Pull one into `plan.md` (with a
> concrete **next** + **risks**) when you start it. Grouped by origin slice.

## TUI (Slice 2 / 4c)
- **Hot-reload renderers on file change** — Starlark renderers load once at TUI
  start today; watch `$DUN_HOME/renderers/*.star` and re-register.

## Exec / worktree (Slice 3 → 3b/4)
- **MCP servers inside the container** — run poly-lsp-mcp / mcpshell via
  `docker exec -i` so they see the contained FS (today they're host-side over the
  mounted worktree).
- **`integrate` ship mode** — advance the local base branch after a push. Must
  use `git fetch . HEAD:<base>` (moves the ref), never `checkout` in the main
  repo; that was the bug in the old `FastForwardLocal`.
- **Exit codes from `ExecBackend`** — check failure is currently detected by
  finding `[exit:` in the output string, which a check that prints that trips.
  Needs `Run` to return a code, not just text.

## Terminal / input (2026-07-31)
- **Co-reader detection on dun's own tty** — dun could notice it is not the only
  process reading its terminal and say so; that failure (see the detach decision)
  was invisible from inside the session and took /proc archaeology to find.
  **Deferred on purpose:** `detach()` should have made it impossible, so this is
  speculative instrumentation until a real report says otherwise.
  **Resume condition:** a user reports dropped keystrokes again.
  **Evidence when it happened:** grabber identified by `/proc/PID/fd` holding
  `/dev/tty` + `wchan == n_tty_read` + matching `tty_nr`. A naive check on
  controlling-tty alone false-positives on every child dun spawns — the
  blocked-in-tty-read condition is the discriminator. A portable partial
  alternative: watch for termios drift from what Bubble Tea set (git's
  `disable_echo` does `tcsetattr(TCSAFLUSH)`, which also discards queued input).

## Launcher (Slice L)
- **fsnotify instead of mtime polling** for the rebuild watch (2s poll today).
- **`/reload` for web sessions** — they'd re-exec inside the PTY; untested.
- **multi-viewer** — one driver + N watchers on a session (v1 is 1:1).
- **`dun -d status` TUI** — richer than the current line list.
- **per-session resource caps**, **cross-host launcher control** (remote reach
  stays via `dun -serve`).
