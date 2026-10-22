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
| `done` | Turn completed | `reason` (`interrupted` when the user stopped it) |
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
| `interrupt` | Stop the turn in flight, keep the session (see below) | `reason` (optional: what stopped it, when not the user) |
| `stop` | Stop processing, exit | — |
| `quit` | Quit entirely | — |

A message sent while a turn is running is **not** queued behind the turn. The
engine calls `Harness.Say()` which buffers it into the next tool result via
`liftQueued()`. Multiple messages batch without limit.

### Interrupting a turn

`interrupt` (also `/interrupt`, and in the TUI ctrl+c, ctrl+g or esc while a turn
runs) cancels the current turn only. The engine (`Harness.Interrupt`):

1. records the reply text the cut round had streamed as an assistant entry,
   answers a tool call left in flight as interrupted by the user, and appends
   a notification `[the user interrupted this turn] …` with a short note on
   the discarded reasoning (duration, length, last sentence — never the
   reasoning itself);
2. ends the turn with `done` `{"reason":"interrupted"}` — no `error`, no retry;
3. starts the next turn at once when messages were queued during the cut
   turn; otherwise it waits for the next message as usual.

With no turn running, the answer is a `control` event (`id` `interrupt`)
saying so.

With a `reason` (dund sends one when a restart cuts a turn short), the
notification starts `[<reason>] …` instead, a cut tool call's result names
the reason, and the note does not tell the model to wait for the user.

### dund upgrades (service frames)

dund broadcasts `upgrade` `{state, version, exe, waiting, deadline}` to every
connection, with no session in the envelope, when an upgrade is pending (its
own binary was replaced, or a client of another build sent `upgrade`), each
time `waiting` changes, and once as `state: "restarting"` before it restarts.
`waiting` lists `{session, what: turn|question|job|agent, id, detail, hint}`.
A new connection gets the current one at once; an HTTP session stream carries
it too. dund restarts when `waiting` is empty, on `upgrade {now: true}`
(`/upgrade now`, the web's Restart now), or at `deadline` (10 minutes).
A restart (or SIGTERM) that cuts work short interrupts each turn with the
reason, records the session in dund's registry, and the next dund sends that
session's engine a user message starting `[automatic] ` that says what was
cut and to resume. A session whose transcript ended mid-turn less than 30
minutes before dund starts (a crash) is resumed the same way.

## Engine Supervision

The TUI outlives its engine. When the engine process exits:

- **Announced exit** (`exit` event): the engine chose to leave (ctrl-C, stop,
  stdin closed). The TUI may respawn with `--continue` to reattach.
- **Unannounced exit** (EOF on stdout): the engine crashed. The TUI respawns
  up to `engineRestartMax` times within `engineRestartWindow`, then gives up
  with a fatal error.

A respawned engine replays the conversation history (`history` event). The TUI
sets `skipHistory = true` to avoid doubling the scrollback.
