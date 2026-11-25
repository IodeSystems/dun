# Event Protocol

The TUI is a **client** of the headless `dun --engine` engine. It re-execs `dun --engine`,
writes user events to its stdin, and renders the JSON event stream on stdout.
The engine stays headless; the UI is pure presentation.

## Direction: Engine → TUI (stdout, line-delimited JSON)

The engine emits events as line-delimited JSON objects. Each has a `"type"` field.

| Type | When | Key Fields |
|------|------|------------|
| `workspace` | Engine starts, reports worktree | `branch` |
| `session` | Session created/resumed | `id` |
| `ready` | The engine takes messages; servers still starting join later | `tools[]`, `servers[]`, `hint`, `startup` (`total_ms`, `phases[]` of `name`, `ms`: where the start's time went) |
| `server` | Server status changed (`/rag`, `/lsp`) | `id`, `action`, `message`, `tools[]`, `servers[]` |
| `token` | Streaming assistant text | `text` (chunk) |
| `tool_call` | A tool call started | `call_id`, `tool`, `args` |
| `tool_result` | Tool returned | `call_id`, `tool`, `result`, `is_error`, `elapsed_ms`, `edits[]` (see File edits) |
| `message` | Turn reply finalized | `role`, `content`, `id` (the reply's history item id) |
| `suggestions` | Predicted next messages | `items[]` |
| `notification` | Proactive notification (RAG, background) | `kind`, `text` |
| `ask` | Agent waiting on user input | `question`, `options[]`, `multi`, `recommended_all[]`, `no_custom` (answer must be an option) |
| `queued` | Message buffered mid-turn | `text`, `count` |
| `usage` | Token accounting | `total`, `active`, `cached`, `processed`, `generated`, `turns` |
| `done` | Turn completed | `reason` (`interrupted` when the user stopped it) |
| `until` | The stop conditions (`/until`) or their run changed; kept as state | `conds[]` (`kind` turns/goal/plan, `n`, `goal`, `check`, `progress`), `cap`, `turn`, `running`, `paused` (with `running`: a question to the person is open; the run goes on when it is answered), `stopped` (why the last run stopped), `line` (one line for a status bar, idle too: `until: plan · turns 15`, `until: turn 3/15 · plan 4 open, 2 ready`, `until: paused (question) · turn 3/15`). A run's stop is also a `notification` of `kind` `until` |
| `error` | Turn failed | `error`, `fatal` |
| `retry` | Provider outage, retrying | `note`, `due`, `seen` |
| `clearing` | A `/clear` came while a turn ran: the turn is interrupted, and the clear waits for it to stop. A message sent meanwhile is queued for the cleared conversation. The `reset` that follows ends it | `waiting` (what the turn was doing: a tool, `the model's reply`) |
| `clear_confirm` | A `/clear` was not done: background jobs run, or acknowledged asks or plan items are open (messages the model never acknowledged are always dropped by `/clear` and never asked about), and the line did not say what becomes of them. The client asks, then sends the slash line `clear jobs=keep\|stop plan=keep\|drop` (only the categories listed); sending nothing cancels. A turn running runs on until the answer. A client that does not ask shows `message`, which names that line | `jobs[]` (as in `jobs`), `plan[]` (`kind`: `ask`/`node`, `id`, `title`), `message` |
| `reset` | Store cleared (`/clear`); the queue is kept. Kept jobs and items are told to the new conversation in one `[clear]` aside on its first turn; stopped jobs send no completion notice | `timing` (`total_ms`, `phases[]`) |
| `control` | Control action done (`/isolate`, `/prompt`) | `id`, `action`, `message` |
| `compaction` | Context compacted | `text` |
| `compacting` | A fold began (`phase: start`); or ended without a `compaction` (`phase: end`, `error` when it failed or was cancelled, none when nothing was folded). Every start ends with one of the two | `phase`, `error` |
| `exit` | Engine leaving voluntarily | `reason` |
| `image_refused` | A message with images not sent (no vision, no `vision_model`; or an unreadable image) | `message`, `content`, `images[]` — the client puts the message back in the composer; `done` follows |

### Token Streaming

Tokens arrive as individual `{"type":"token","text":"..."}` events. The TUI
accumulates them in `m.cur` (a `string` field, not `strings.Builder` — Bubble
Tea copies the model each `Update`). Rendering is deferred via `renderDue` +
`renderTickMsg` to cap redraws at `renderHz` (~20 fps) instead of per-token.

### Tool Call / Result Pairing

`tool_call` is sent when the call starts, `tool_result` when it returns; both
carry `call_id`, and a client pairs them by it (`session-events.md`).

### File edits

A `tool_result` whose call changed files carries `edits[]`, one per file:
`{path, old_text, new_text, created, deleted, skipped}` (`proto.FileEdit`),
the whole text before and after, what an ACP `diff` tool-call content
(`{path, oldText, newText}`) needs. `path` is absolute. `created`: no file
before (`old_text` empty); `deleted`: none after (`new_text` empty). Every
field but `path` is omitted when empty; an engine before it sends no `edits`.

The engine reads the files itself, before and after the call
(`internal/harness/fileedits.go`), so the tool's own result is unchanged and
the model is not sent the texts. Read: dun's file tools that write (`edit`,
`write`, `delete`, `move`: a move is the old path deleted and the new one
created) and poly-lsp's `node_edit` and `node_delete` (`file`, or the file of
`node`). Not read, so no `edits`: `exec`, mcpshell's `eval`, `node_refactor`
and `node_rename_file` (they change files their arguments do not name), any
other MCP server's tools, and a sub-agent's calls. A file another process
changed during the call is in its edit too.

`skipped` names a file whose texts are left out: `too_large` (over 1 MiB,
before or after), `binary` (a NUL byte, or not UTF-8), `directory`,
`symlink`, `unreadable`, `call_cap` (the call's texts passed 2 MiB),
`history_cap` (in a replay, below).

The edits are kept in the session's `<id>.edits.jsonl` sidecar (a text over
8 KiB as a content-addressed blob, so one version of a file is kept once),
and the `history` event's `tool_result` items carry them, the newest within
8 MiB of texts (older ones `skipped: history_cap`). `/clear` removes the
sidecar with the transcript.

### Queued Messages

A message typed while a turn runs is echoed as `user {id, queued: true}`; the
engine later sends `delivered {ids}` or `withdrawn {ids}` for it
(`session-events.md`).
The TUI draws a queued message below everything the turn shows, the reply
still streaming included, and delivered makes it a user row at the end of the
conversation, where it already was (`internal/tui/tui_scroll.go` drawOrder).

## Direction: TUI → Engine (stdin, line-delimited JSON)

| Type | Purpose | Fields |
|------|---------|--------|
| `user` | Send a message | `content`, `images[]` (optional: `{path, label}` — a file on the engine's machine and the text in `content` that stands for it) |
| `answer` | Respond to `ask_user` | `value` |
| `server` | Control a server (`/rag`, `/lsp`) | `id`, `action` (`status`/`on`/`off`/`auto`/`manual`) |
| `reset` | Clear the session store: as a bare `/clear`, so it may be answered by `clear_confirm` (the slash line `clear …` carries the answer) | — |
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
