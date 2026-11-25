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

## Icon

Files in `release/acp/`:

- `icon.svg`: the ACP registry icon. A hard hat, 16x16 viewBox, one
  colour (`currentColor`), no hardcoded colours. Meets the registry's
  validation (FORMAT.md, CONTRIBUTING.md "Icon Validation"). Use this one.
- `icon-mascot.svg`: the foreman (`internal/tui/mascot.go`, 13x11 pixel
  grid) as a one-colour pixel silhouette, same rules. Candidate; the hat
  reads better at 16 px.
- `icon-color.svg`: the hard hat in yellow, hardcoded colours. For places
  that allow colour (site, org console). Not valid for the registry.

How an icon reaches an editor: only through the registry. A registry
`agent.json` has `"icon": "<url of icon.svg>"` and the registry repo holds
`<agent-id>/icon.svg`. The ACP schema's `initialize` response carries no
icon (`agentInfo` is name, title, version, `_meta`). JetBrains
`~/.jetbrains/acp.json` and Zed `agent_servers` document no `icon` field
(checked against their docs, 2026-11-24), so a locally configured agent
shows the editor's default icon. No change to `internal/acp` helps.

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
| `plan` | `plan`: the tree in order, a repeat row once, dropped rows left out; todo and blocked are `pending` (ACP has no blocked), doing `in_progress`, done `completed`; a ready todo is priority `high`, the rest `medium`. An editor that advertises `clientCapabilities.plan` (or JetBrains Air's `planContentDelta` or `planFile` in `_meta.jetbrains.air.capabilities`) gets the same entries as the unstable `plan_update` instead (`plan.type` `items`, `planId` `dun-<session id>`: a session has one plan), as the plan-operations RFD says to. The plan persists: the engine says it once at start when it has rows (so dund's snapshot and `session/load` carry it), and the bridge keeps the last plan sent and sends it again at the start and at the end of each prompt (an editor may clear its plan per turn; each update replaces the plan, so a resend is harmless) |
| `commands` | `available_commands_update`; a command's usage (or its choices) is the input hint |
| `user` (another client's) | `user_message_chunk` |
| `request` (done) and `usage` | `usage_update`, see "Usage, context and cost" |

Tool kinds: `read` file_read, file_list, node_read; `edit` file_edit,
file_write, node_edit, node_refactor; `delete` file_delete, node_delete;
`move` file_move, node_rename_file; `search` file_grep, node_query,
search, docs_search, lookup; `execute` exec, eval; `think` recap, asks,
plan_*; `fetch` fetch, web_fetch; anything else `other`. An MCP server's
tool (`<server>__<tool>`) is looked up by its own name. Only the tools
`event-protocol.md` "File edits" lists carry diffs: exec, eval,
node_refactor and node_rename_file change files the editor is not shown a
diff for.

### Usage, context and cost

`usage_update {size, used, cost}` goes out after each model round
(`request` `done`: `used` = its prompt + generated tokens, `size` its
window) and at each turn's end (`usage`: `used` = `active`, the context now;
`size` = `window`). An update equal to the last one is not sent.
`session/load` sends one with the session's last `usage`. Nothing is sent
until a window size is known.

`cost` is `usage.spend.session_cost`, the session's cumulative spend. It is
sent only when dun has one in `USD` or `USD estimate` and not mixed with
another currency (other currencies, such as old "credits", are not ISO 4217
codes and are not sent). Both go out as `USD`; for `USD estimate` (a price
dun computed itself) the cost's `_meta` is `{"estimate": true}`, since ACP's
Cost has no estimate flag. It is a sum of the costs the gateway reported per
response. A client that wants "exact" must not assume it. Rounds from before the cost was reported, or a
provider that reports none, are not in it.

The prompt response carries `usage` (ACP's unstable field, SDK v0.20.0):
the turn's own totals from `usage.turn_stats`, sub-agents and side calls
included. `inputTokens` = `prompt` (cached part included), `outputTokens` =
`completion`, `cachedReadTokens` = `cached`, `cachedWriteTokens` =
`cache_write` (both omitted when 0), `totalTokens` = input + output. dun
does not count reasoning tokens apart, so `thoughtTokens` is never sent. A
turn with no `turn_stats` (cut before its first round, or an older engine)
has no `usage`. When one turn's end answers several queued prompts, each
gets the same totals.

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

### Steering (`_session/steering`)

JetBrains Air sends a message typed while a turn runs as the extension
request `_session/steering`, not as a second `session/prompt`. dun takes
it as it takes a mid-turn prompt (above): a queued user message delivered
into the turn's next tool result, or the next turn's input. The request is
answered at once with `{"status": "queued"}`; nothing waits for a turn's
end. Any other `_` method is -32601; an unknown session -32002; params with
no text -32602. Code: `steer.go`.

**Inferred, not published:** an Air debug bundle (2026-11-24, keys
pseudonymised, values redacted) shows the params as `{sessionId, <string>,
<array of objects of two strings>}`, read as an id and content blocks
(`{type, text}`), and the response as an object of one string member whose
key is unknown. So the bridge takes the text from the first array that
reads as content blocks; else a string under `text`, `message`, `content`,
`prompt`, `steering` or `input`; else the params' one string. Params with
several strings and none of these are refused. Not yet run against Air.

### Stop reasons

`done` is `end_turn`; `done {reason: interrupted}`, or any turn end after
a `session/cancel`, is `cancelled`. A turn that fails (`error`) answers its
prompts with a JSON-RPC error carrying dun's message. dun reports no token
limit or refusal as a turn end, so `max_tokens`, `max_turn_requests` and
`refusal` are not sent.

`done {reason: suspended}` (the engine stopped with a question open: a
kill, dund's restart, a model switch) is **not** a turn end: the prompts
stay held, with no `cancelled` or `end_turn`. A form or permission request
still open stays open. The next engine asks the same question again (same
`ask_id` and `call_id`; `docs/event-protocol.md`, "Questions across a
restart"): one still open is kept, one the bridge dropped (a cancelled
request) is raised again, and an `ask` for another id is a new question.
The reply is sent as the `answer` with that `ask_id`; the continuation
turn (no new user message) ends the prompt with its stop reason and the
usage of both turns added up. A snapshot that shows the session idle while
suspended does not end the prompt either. Code: `turns.go` (`onDone`),
`ask.go` (`onAsk`).

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

An editor that advertises `clientCapabilities.elicitation.form` gets every
question as an `elicitation/create` form instead (`elicit.go`), scoped to
the session and the tool call that asked. The form's message is the
question; its fields:

- `answer`: the options as a string `enum` (one answer) or an `array` of
  the enum (multiple choice), the recommended option(s) as its `default`;
  with no options, a required text field.
- `other`: a text field for the person's own words, when the question has
  options and takes other answers (an `ask_user`; not the file-access
  question). Its `_meta` is `{"_askUserQuestionCustomAnswer": true,
  "jetbrains": {"air": {"customAnswer": true}}}`, the keys JetBrains Air's
  own agent puts on its custom-answer field (seen in an Air debug bundle,
  2026-11-24; the spec names no such field). With it neither field is
  required; without it `answer` is.

`accept` sends the answer: `other` when filled, else the pick; for
multiple choice the checked options and `other`, joined ", " as the TUI
joins them. `decline` sends the question's "no" option when it has one
(the file-access question). `cancel`, `decline` with no "no", or an
`accept` with no usable value leaves the question open as text, the next
prompt its answer (the TUI's esc parks a question the same way). A
question answered elsewhere cancels the request (the SDK sends
`$/cancel_request`; the spec has no withdraw) and a late reply is not
read. An editor that fails `elicitation/create` is asked as above.

## Config options

session/new and session/load answer with `configOptions` (ACP session
config options), all `select`, in this order:

| id | category | values | current | set sends |
|---|---|---|---|---|
| `model` | `model` | every saved provider's models (control `models`, as the web app's picker): one group per provider when there are several, a provider whose list failed left out; value `<provider>/<model id>` | the `settings` event's model and URL, listed even when its provider's list did not come | dund `switch_model {url, model}`: the engine restarts on the same conversation with its other flags (the web app's switchModel; the TUI's /model without saving a default) |
| `effort` | `thought_level` | the `effort` command's choices in the `commands` event (default, none, minimal … max; a level the model does not take says what is sent instead) | that command's state | control `effort <level>`: dun's /effort, which is saved for every session |
| `mode` ("Run until") | `mode` | `off`, `goal`, `plan`, `goal+plan` | the `until` event's conditions | control `until <line>` (dun's /until), with the turn cap's condition kept; `goal` waits for its text (below) |
| `turn_cap` | `_turn_cap` (custom) | `off`, `5`, `10`, `20`, `50`, and a cap set elsewhere (the TUI's `/until turns 15`) | the `until` event's `turns` condition | control `until <line>`, with the mode's conditions kept |

session/new waits up to 5 s for the model list, the settings and the
commands; an option whose events come later arrives as a
`config_option_update`. `session/set_config_option` waits for the engine to
report the new value (15 s; a model switch 2 min) and answers with the full
list. A change from any client (the TUI's /model or /effort) is a
`config_option_update` with the full list; the change a set answers is not
sent again.

Refused: a value not in the option's list, an unknown option id, or a
boolean value (`-32602`); a model switch while the model generates for a prompt of the editor's
(`-32600`: the restart would end the turn with no `done`; a switch is
allowed while every prompt waits on an open question, because the
restart suspends the question and the new engine asks it again, see "Stop
reasons"; a queued message or a command also waits, so it is refused); a switch
dund refuses or a value the engine does not report in time (`-32603`).

### Stop conditions (mode, turn_cap)

dun's `/until` (`decisions.md`, "Stop conditions") is a set of conditions;
a select holds one value, so two options. `mode` is what the run waits
for, `turn_cap` the most turns it takes:

- mode `off`, turn_cap `off`: one turn per prompt (`/until off`).
- mode `off`, turn_cap N: run N turns (`/until turns N`).
- mode `goal`, `plan` or `goal+plan`: until the goal is met or the plan is
  drained (no node ready or under way), whichever comes first; at most the
  turn cap's turns, 20 with the cap off.
- **A goal's text is a prompt's.** Chosen while a prompt of the editor's
  runs, the goal is that prompt's text and is set at once. Otherwise the
  set sends nothing, the option shows `goal` (pending), and the next
  prompt sends `/until goal "<its text>" …` before itself, so the turn it
  starts is the run's first. A `--check` command is not offered here; set
  it with the `/until` command (a prompt that starts with `/until`).
- The conditions stay for later prompts until changed. A goal stays the
  first prompt's text: to give a new goal, choose `off`, then `goal` again.
- **A prompt is answered when its run stops**, not when its first turn
  ends: a `done` while the `until` event says `running` is held, and the
  `until` event that stops the run answers it (`end_turn`, or `cancelled`
  after session/cancel). An error answers it at once, as before.
- Progress is agent text: `until: turn N of M` as each turn of the run
  starts, and `until: stopped · <why>` when it stops.
- session/cancel during a run interrupts the turn running, or, between
  turns (the goal being judged), stops the run.
- **A question pauses the run** (the engine's `ask_user` or file-access
  question, sent as session/request_permission): the `until` event says
  `paused` and still `running`, so the held prompt stays held. The answer
  resumes the run; session/cancel while paused cancels the request and
  stops the run (the prompt is answered `cancelled`). An engine that
  stopped with the question open comes back paused and owes no turn: the
  prompt stays held until the next prompt (the answer) runs, or
  session/cancel stops the run.

No `modes`: `session/set_mode` is method-not-found; the stop conditions are
the `mode` config option. Plan mode (`/plan mode on|off`) gates file
changes until approval; it is not a run type and is not shown.

## Not supported

- The editor's MCP servers (`mcpServers` in session/new and load): ignored,
  with a log line. dun runs its own servers (`dun.json`, `/mcp`).
- Client `fs/*` and `terminal/*`: dun reads and writes the disk and runs
  commands itself; the editor sees the files change on disk.
- Session modes, boolean config options, the unstable model selection
  (`models`, `session/set_model`: not in SDK v0.20.0), `session/list`,
  `session/resume`, `session/delete`.
- Sign-in: there is none. initialize advertises one `agent` auth method
  (id `dun`) because editors such as JetBrains Air wait for one and the ACP
  registry requires one. `authenticate` with `dun` succeeds with no check;
  any other id is -32602. Sessions do not need it. Providers are set in dun
  (`/login` in the TUI); the bridge is the user's own process and dund
  checks the socket's peer. `agentCapabilities.auth` is the SDK's zero
  value (`{}`), not set by dun.
- Images and audio in prompts; usage and cost updates.

## Verified

- Unit: `internal/acp` tests drive the bridge with the SDK's client side
  and a fake dund (initialize, new, prompt with a diff, cancel, queued
  prompts, load replay, permission, text question, slash command, config
  options: listed, set, refused, changed elsewhere; stop conditions: mode,
  turn_cap, a goal from the next prompt, a prompt held to its run's stop;
  for an editor that advertises them: plan_update, elicitation forms
  (select, multiple choice, text, custom answer, decline, cancel,
  withdrawn, fallback); `_session/steering`). Forms, plan_update and
  steering are not yet run in any editor.
- Live (2026-11-24, against a running dund): initialize, session/new, a
  prompt streamed to `end_turn`, session/load of that session replaying it.
  Not yet live: cancel, queued prompts, permission requests, any editor.
- Live (2026-11-24, a private dund and `ux llm`): `/until turns 3`, then a
  prompt: three turns ran, the progress notes and the stop reason came as
  agent text, and the prompt was answered once, `end_turn`, at the stop.

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

Stop the private dund (kill the PID in `$D/home/.dun/dund.pid`; dund has no stop command)
and the stub afterwards. Result 2026-11-24 (after plan_update, forms and
steering): 75 passed, 1 failed, 74 skipped; 0 mandatory failures.
ACP-PLAN-002 (plan_update when the client advertises `plan`) now passes.
One advisory failure remains, out of scope: ACP-SESSION-004 (the editor's
MCP servers are not started). Many capability tests skip: modes, config
options, list, resume, delete, images, subagents.
