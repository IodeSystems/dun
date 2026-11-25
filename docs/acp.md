# dun in editors: the Agent Client Protocol

`dun acp` is dun as an [ACP](https://agentclientprotocol.com) agent: an
editor starts it and speaks ACP (JSON-RPC 2.0, one message per line) on its
stdin and stdout. The bridge is a client of dund over dund's unix socket, as
the TUI is, and starts dund when nothing listens. dund owns the sessions;
the bridge translates events and commands and keeps only what a translation
needs. Code: `internal/acp`, `cmd/dun/acp.go`. Why: `decisions.md`, "ACP
bridge".

## Configure an editor

`command` must find dun: `dun` on the editor's PATH, or an absolute path
(`which dun`). Log lines go to stderr, which the editor keeps in its agent
log; `dun acp --debug` adds the SDK's diagnostics.

**Zed** (`settings.json`; or Agent Settings, External Agents, Add Custom
Agent). Format checked against zed.dev/docs/ai/external-agents, 2026-11-24:

```json
{
  "agent_servers": {
    "dun": {"type": "custom", "command": "dun", "args": ["acp"], "env": {}}
  }
}
```

Zed's command palette `dev: open acp logs` shows the messages.

**JetBrains IDEs, Air in IDEs and AI Assistant** (`~/.jetbrains/acp.json`;
Agents page, Edit acp.json; merge with entries already there, then restart
the IDE). Format checked against jetbrains.com/help/air
(air-in-ides-manage-agents), 2026-11-24. The command must be an absolute
path:

```json
{
  "agent_servers": {
    "dun": {"command": "/home/you/.local/bin/dun", "args": ["acp"], "use_idea_mcp": false, "use_custom_mcp": false}
  }
}
```

`use_idea_mcp` and `use_custom_mcp` give the agent the IDE's MCP servers;
dun does not use them (below), so both are false. **Unverified:** the
standalone JetBrains Air app; its docs name custom ACP agents only through
an organization's Agents page (a JSON entry in the ACP registry format).
Not run in any JetBrains product yet.

**Neovim** (CodeCompanion, avante.nvim and others): each plugin has its own
adapter config; give it the command `dun` and the args `["acp"]`.
**Unverified.**

## What an ACP session is

- `session/new` creates a dund session in `cwd`, **in place**: no
  worktree, so dun edits the files the editor has open, on disk (dun's
  default; the bridge passes no `--worktree`). The engine runs with
  `--no-suggest`: no ACP client shows suggestions, and each one is a side
  call. The session id is dund's: `dun --resume <id>`, the TUI's session
  list and the web app open the same session, and what one client does
  every other shows (another client's message is a `user_message_chunk`).
- `session/load` takes a dund session id. A session dund does not have
  (stopped, or older than dund) is adopted from the workspace's session
  file. The conversation is streamed before the answer: user messages,
  thinking, replies, and each tool call with its result and diffs. dun's
  own notices are left out.
- Closing the editor detaches; dund keeps the session. `session/close`
  (advertised) cancels the session's turn as `session/cancel` does, ends
  its prompts `cancelled`, and detaches.
- An unknown session id (prompt, or a load with no dund session and no
  session file in the workspace) is `-32002` with `data.sessionId`; a
  request before `initialize` is `-32600`.
- A lost dund connection (dund restarting into a new build) is redialed
  for 2 minutes and resubscribed after the last event seen; a prompt
  waiting meanwhile is answered when its turn ends.

## Updates

| dun event | ACP `session/update` |
|---|---|
| `token` | `agent_message_chunk`; a final `message` is sent only when its text came whole |
| `reasoning` | `agent_thought_chunk` |
| `tool_call` | `tool_call`, `in_progress`, kind from the table below, `title` the tool and its main argument, `locations` the files its arguments name, `rawInput` the arguments |
| `tool_result` | `tool_call_update`, `completed` or `failed` (`is_error`); content: the result text, then a `diff` per `edits[]` file (`oldText` absent for a created file); a `skipped` edit is a text line naming the file and why |
| `plan` | `plan`: the tree in order, a repeat row once, dropped rows left out; todo and blocked are `pending` (ACP has no blocked), doing `in_progress`, done `completed`; a ready todo is priority `high`, the rest `medium` |
| `commands` | `available_commands_update`; a command's usage (or its choices) is the input hint |
| `user` (another client's) | `user_message_chunk` |

Tool kinds: `read` file_read, file_list, node_read; `edit` file_edit,
file_write, node_edit, node_refactor; `delete` file_delete, node_delete;
`move` file_move, node_rename_file; `search` file_grep, node_query,
search, docs_search, lookup; `execute` exec, eval; `think` recap, asks,
plan_*; `fetch` fetch, web_fetch; anything else `other`. An MCP server's
tool (`<server>__<tool>`) is looked up by its own name. Only the tools
`event-protocol.md` "File edits" lists carry diffs: exec, eval,
node_refactor and node_rename_file change files the editor is not shown a
diff for.

## Prompts

A prompt's text blocks are the message; a `resource_link` is
`[file: <path>]` (the model reads it with its tools); an embedded text
resource is quoted with its path. dun advertises no image or audio input.
A prompt that starts with `/` and names one of dun's commands is run as
that command (control `slash`): its reply is the agent's text, and the
prompt ends with it, or after 3 s for a command that says nothing, or with
the turn when the command starts one.

### Prompts while a turn runs

ACP does not say what an agent does with a `session/prompt` that comes
while one is running (prompt-turn spec, 2026-11-24). dun queues it, as for
any client (`mid-turn-messages.md`): the message is delivered into the
running turn's next tool result, or, if the turn ends first, it is the next
turn's input. **A prompt is answered when the turn that gave its message to
the model ends**, with that turn's stop reason:

- a prompt that started a turn (`user`, not queued): that turn's end;
- a queued prompt delivered into the running turn (`delivered` with a
  call): that turn's end, so one `done` can answer several prompts;
- a queued prompt the turn ended before (`delivered` without a call, at
  the next turn's start): the next turn's end;
- a queued prompt taken back (`withdrawn`): `cancelled`.

A prompt is matched to dun's `user` event by its text, oldest first: dun
names a message only when it takes it. The SDK cancels a prompt's context
when a newer prompt of the session arrives; the bridge ignores that
context, so the earlier prompt still waits for its turn.

### Stop reasons

`done` is `end_turn`; `done {reason: interrupted}`, or any turn end after
a `session/cancel`, is `cancelled`. A turn that fails (`error`) answers its
prompts with a JSON-RPC error carrying dun's message. dun reports no token
limit or refusal as a turn end, so `max_tokens`, `max_turn_requests` and
`refusal` are not sent.

### session/cancel

Prompts dun has not taken yet are answered `cancelled` at once. Queued ones
are taken back (control `unqueue`; it takes back every queued message of
the session, another client's too) and the turn is interrupted; its `done`
answers the rest `cancelled`. A cancel with no prompt of the editor's in a
turn sends nothing: another client's turn is not stopped. Race left open:
a prompt sent but not yet echoed when the cancel comes is answered
`cancelled`, but dun may still take it and run a turn for it.

## Questions

dun's `ask` (an `ask_user` call, or a file tool's question about a path
outside the workspace, `harness/fileaccess.go`):

- **Options, one answer**: `session/request_permission`, one option per
  answer (id = its index), on the tool call that asked. Kinds are read from
  the words: "no" is `reject_once`; "yes, for this project" and "yes,
  everywhere" are `allow_always`; anything else `allow_once` (for an
  `ask_user` choice the kind is only the button's look). The chosen option
  is sent as the `answer`. `cancelled` sends nothing: the editor cancels
  the turn with `session/cancel`, and the interrupt ends the question. A
  question answered by another client withdraws the request.
- **Multiple choice, no options, or a request the editor failed**: the
  question and its options as the agent's text; the next prompt is sent as
  the `answer`, not as a message, and is answered with the turn it
  continues.

## Not supported

- The editor's MCP servers (`mcpServers` in session/new and load): ignored,
  with a log line. dun runs its own servers (`dun.json`, `/mcp`).
- Client `fs/*` and `terminal/*`: dun reads and writes the disk and runs
  commands itself; the editor sees the files change on disk.
- Session modes, config options, `session/list`, `session/resume`,
  `session/delete`, authentication (none is needed: the bridge is the
  user's own process and dund checks the socket's peer).
- Images and audio in prompts; usage and cost updates.

## Verified

- Unit: `internal/acp` tests drive the bridge with the SDK's client side
  and a fake dund (initialize, new, prompt with a diff, cancel, queued
  prompts, load replay, permission, text question, slash command).
- Live (2026-11-24, against a running dund): initialize, session/new, a
  prompt streamed to `end_turn`, session/load of that session replaying it.
  Not yet live: cancel, queued prompts, permission requests, any editor.

## Conformance kit (manual)

[acp-tck](https://github.com/agentclientprotocol/acp-tck) (v0.5.0, commit
7c71c4d, 2026-11-24) launches the agent as a stdio subprocess, one process
per test. It needs network to fetch its Python 3.14 and dependencies (uv),
so there is no make target. Run it with a private dund and a stub LLM, never
the real dund: build `go build -o $D/dun ./cmd/dun` and `go build -o $D/ux
./tools/ux`; start `$D/ux llm --port-file $D/llm.addr`; make a wrapper that
`exec env HOME=$D/home DUN_HOME=$D/home/.dun DUN_URL=http://<addr>/v1
DUN_MODEL=<a model the stub lists> DUN_LLM_KEY=x $D/dun acp`; then, in a
clone of the kit:

    uv run acp-tck --agent-cwd $D/wd --timeout 60 --test-timeout 180 -- $D/agent.sh

Stop the private dund (`DUN_HOME=$D/home/.dun $D/dun dund stop`, or kill by
PID) and the stub afterwards. Result 2026-11-24: 0 mandatory failures. Two
advisory failures remain, both out of scope: ACP-PLAN-002 (the bridge sends
the baseline `plan` update, not the unstable `plan_update`, when the client
advertises `plan`) and ACP-SESSION-004 (the editor's MCP servers are not
started). Many capability tests skip: modes, config options, list, resume,
delete, images, subagents.
