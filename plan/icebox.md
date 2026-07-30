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
- **worktree → commit → PR** polish beyond the current `open_pr`.

## Launcher (Slice L)
- **fsnotify instead of mtime polling** for the rebuild watch (2s poll today).
- **`/reload` for web sessions** — they'd re-exec inside the PTY; untested.
- **multi-viewer** — one driver + N watchers on a session (v1 is 1:1).
- **`dun -d status` TUI** — richer than the current line list.
- **per-session resource caps**, **cross-host launcher control** (remote reach
  stays via `dun -serve`).
