# Event Protocol

The TUI is a **client** of the headless `dun -p` engine. It re-execs `dun -p`,
writes user events to its stdin, and renders the JSON event stream on stdout.
The engine stays headless; the UI is pure presentation.

## Direction: Engine → TUI (stdout, line-delimited JSON)

The engine emits events as line-delimited JSON objects. Each has a `"type"` field.

| Type | When | Key Fields |
|------|------|------------|
| `workspace` | Engine starts, reports worktree | `branch` |
| `session` | Session created/resumed | `id` |
| `ready` | Servers spawned, tools available | `tools[]`, `servers[]`, `hint` |
| `server` | Server status changed (`/rag`, `/lsp`) | `id`, `action`, `message`, `tools[]`, `servers[]` |
| `token` | Streaming assistant text | `text` (chunk) |
| `tool_call` | Agent invoking a tool | `tool`, `args` |
| `tool_result` | Tool returned | `tool`, `result` |
| `message` | Turn reply finalized | `role`, `content` |
| `suggestions` | Predicted next messages | `items[]` |
| `notification` | Proactive notification (RAG, background) | `kind`, `text` |
| `ask` | Agent waiting on user input | `question`, `options[]`, `multi` |
| `queued` | Message buffered mid-turn | `text`, `count` |
| `usage` | Token accounting | `total`, `active`, `cached`, `processed`, `generated`, `turns` |
| `done` | Turn completed | — |
| `error` | Turn failed | `error`, `fatal` |
| `retry` | Provider outage, retrying | `note`, `due`, `seen` |
| `reset` | Store cleared (`/clear`) | — |
| `control` | Control action done (`/docker`, `/prompt`) | `id`, `action`, `message` |
| `compaction` | Context compacted | `text` |
| `exit` | Engine leaving voluntarily | `reason` |

### Token Streaming

Tokens arrive as individual `{"type":"token","text":"..."}` events. The TUI
accumulates them in `m.cur` (a `string` field, not `strings.Builder` — Bubble
Tea copies the model each `Update`). Rendering is deferred via `renderDue` +
`renderTickMsg` to cap redraws at `renderHz` (~20 fps) instead of per-token.

### Tool Call / Result Pairing

A `tool_call` event creates a `convoEntry` with `pendingTool` set to its index.
The matching `tool_result` folds into the same entry via `foldedTool()`, making
the pair one collapsible unit. If no pending call exists, the result is appended
as a new entry.

### Queued Messages

When the user types while a turn is running, the engine buffers the message
(via `Harness.Say`) and emits a `queued` event. The TUI:

1. Removes the echo from the conversation (added by `sendUser`)
2. Stores the text in `queuedTexts` (shown in a pending area above the divider)
3. On `done` or `error`, moves the queued messages into the conversation at the
   delivery point

## Direction: TUI → Engine (stdin, line-delimited JSON)

| Type | Purpose | Fields |
|------|---------|--------|
| `user` | Send a message | `content` |
| `answer` | Respond to `ask_user` | `value` |
| `server` | Control a server (`/rag`, `/lsp`) | `id`, `action` (`status`/`on`/`off`/`auto`/`manual`) |
| `reset` | Clear the session store | — |
| `stop` | Stop processing, exit | — |
| `quit` | Quit entirely | — |

A message sent while a turn is running is **not** queued behind the turn. The
engine calls `Harness.Say()` which buffers it into the next tool result via
`liftQueued()`. Multiple messages batch without limit.

## Engine Supervision

The TUI outlives its engine. When the engine process exits:

- **Announced exit** (`exit` event): the engine chose to leave (ctrl-C, stop,
  stdin closed). The TUI may respawn with `--continue` to reattach.
- **Unannounced exit** (EOF on stdout): the engine crashed. The TUI respawns
  up to `engineRestartMax` times within `engineRestartWindow`, then gives up
  with a fatal error.

A respawned engine replays the conversation history (`history` event). The TUI
sets `skipHistory = true` to avoid doubling the scrollback.
