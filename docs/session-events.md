# Session events: the explicit API

What a client may rely on without inferring it (Carl 2026-11-14: "work the TUI
does by inference moves into dund/the engine as explicit events, so the API
makes the right thing obvious"). Wire format and the full type list:
`event-protocol.md`, `proto/events.go`. Why dund numbers and snapshots events:
`sessions-and-dund.md`.

## A person's messages

- `user {id, content, queued?}`: the engine took a message. `id` names it from
  here on. `queued` says it went into the running turn instead of starting one.
- `delivered {ids, call_id?}`: queued messages reached the model. With
  `call_id`, the tool result of that call carried them, and this event comes
  right after that `tool_result`; without it, a turn took them as its input. A
  client turns each queued row into an ordinary message **after what it shows
  so far**: history has it there too (a `user` item with `queued: true` right
  after the `tool_result`), so live and replay agree.
- `withdrawn {ids}`: queued messages taken back (control `unqueue`), to every
  client. Each drops those rows. The client that sent `unqueue` puts their text
  in its composer, from the `user` events it holds; the texts are not sent
  again.
- A queued message stays queued until one of those two events names it. Not
  the next `tool_result`, not `done`: an interrupted or failed turn leaves its
  queue for the next turn, which delivers it.
- Choice: `delivered` is its own event rather than a second `user` event,
  so a client that does not know it ignores it instead of showing the message
  twice.
- Old logs: user items in history always have an id (the store's). A message a
  tool result carried before this change exists only inside that result. A
  queued `user` event with no id (a trace from an older engine) is settled at
  the turn's end, as clients used to; that fallback is marked in both clients.

## Tool calls

- `tool_call {call_id, tool, args}` goes out when the call **starts**
  (dispatch begins), so a client shows it running. Every dispatched call
  has one, a refused one too.
- `tool_result {call_id, tool, result, is_error?, elapsed_ms}` when it
  returns. `result` is the text a person is shown (capped as the model's
  is); `is_error` is the engine's verdict (an error, `ERROR…`, exec's
  `[exit: …]`), omitted when false. History's `tool_call`/`tool_result`
  items carry the same `call_id`, and `tool_result` items `is_error`.
- A client pairs by `call_id`. Fallback, marked in both clients, for an
  event log from before ids only: a result with no `call_id` goes to the
  newest pending call of the same tool.
- Order inside a call: `tool_call`, then (while it runs) whatever else
  happens, then `tool_result`, then `delivered` for the messages it carried.

## Sub-agents on calls and notices

- `agent`: the sub-agent number, so a client opens that child's view
  (the `agent {id, action:"view"}` command) without reading text. On an `agent` tool
  call: `tool_call.agent` is its `id` argument (a follow-up: tell, node,
  resume, dismiss, report), absent on a spawn, whose number exists only
  once it started; `tool_result.agent` is the child the call started or
  acted on, absent when it reached none (a refused spawn, `no agent #N`).
  On a `notification`: the child it is from or about (its message, report,
  quiet reminder, plan publish), from the head the engine writes
  (`dun.NoticeAgent`); absent otherwise.
- Replay: history's `tool_call`, `tool_result` and `notification` items
  carry the same `agent`. A session entry has no field for it, so a spawn's
  number comes from the child's sidecar (`<sub>.agent.json` `call_id`, the
  spawning call's id), a follow-up's from its `id` argument, and a notice's
  from its text head, as live. A spawn from before the sidecar kept the call
  id replays with no `agent` on it; its follow-ups still have one.

## turn_state

`turn_state {phase, since, queued:[{id,text}], ask?, mount?, retry?, discard?,
tool?, call_id?}` is where the turn is, sent on every change and kept as
state, so a snapshot carries the latest (also past the 256-event replay
window). Clients read busy, the turn's clock, the open question, an
interrupt under way, a provider wait and the queue from it; they do not
derive them from `turn`, tokens, tool calls or `done`.

- `phase`: `idle` · `working` · `waiting_answer` (`ask` or `mount` is open) ·
  `interrupting` (an interrupt reached the turn; its `done` ends it) ·
  `retrying` (`retry` is the provider wait). `giveup` goes to `idle`.
- `discard`: a turn-scope retry redoes a round whose stream died; the text it
  streamed is dropped (the TUI leaves a "regenerating" marker).
- `since`: when the turn began, Unix ms; 0 when idle.
- `queued`: every message waiting for the model, oldest first, with its
  text. It outlives an interrupted or failed turn (the next turn takes it).
- Owner: the engine (`cmd/dun/turnstate.go`), because only it knows the turn,
  the interrupt, the question and the queue. It follows its own events as it
  writes them (one rule, `proto.StepTurnState`), so no path can change the
  turn without the state following. dund sends `idle` with an empty queue
  when the engine stops or dies (nothing of a turn outlives it), and
  `waiting_answer` for a question it restored after a restart.
- A queued row whose id the state no longer lists, with no `delivered` or
  `withdrawn` for it, while idle: lost with its engine; clients say so.
- dund reads it too: the idle stop and an upgrade's waits take "in a turn"
  and "a question is open" from `turn_state` (`session.track`). An engine
  that has sent none since it started (an older build) is read from `turn`,
  `done`, `error`, `ask`, `mount` and `answered`, a marked fallback.
- Old logs: an event log with no `turn_state` reads as idle.

## Snapshots and reconnects

- Every state event in a `snapshot`'s `state` carries `replayed: true`
  (an envelope field). It is state, not news: a client applies it and adds
  no line for it (a server's message, a branch line, an engine restart), so
  a reconnect does not repeat notes. `turn_state` is applied whole, queued
  rows included.
- The snapshot's `items` and its `after` seq are read to agree: dund reads
  the session file again when an event was written meanwhile (a few tries).
  The engine writes an entry and sends its event apart, so dund also drops
  from the replay after a snapshot what the items hold: a `user` event by
  id, a `tool_call`/`tool_result` by `call_id`, a `message` by `id` (the
  reply's entry; its `assistant` history item carries the same `id`). An
  entry is in the snapshot or the replay, not both, and two identical
  replies are two entries. Fallback, marked in dund, for an engine older
  than reply ids: a `message` with no `id` equal to the last reply in the
  items is dropped.
- A client that gets a snapshot on a reconnect replaces its conversation
  with the snapshot's items: what happened while it was away is only there.
  The TUI keeps the splash, the draft, the open question and its picker,
  and the scroll (at the bottom stays at the bottom); its own lines (notes,
  command output) are not in a snapshot and go (`internal/tui/rebuild.go`).

## Questions

- `ask {ask_id, call_id, question, options, multi, recommended,
  recommended_all, no_custom}`: the open question. `call_id` is the asking
  `ask_user` call. The `ask` event is the question's row live; history shows
  it from that call's `tool_call` item (a client that has the row for the
  call does not add another), and the answer from its `tool_result` item
  as the person's row, as `answered` does live. A result `ERROR: could not
  get an answer…` (interrupted) adds no row; any other `ERROR` result is a
  call refused before it asked, shown live as nothing, and the TUI drops its
  row. `multi`: any number may be picked,
  `recommended_all` checked to start with, sent back comma-separated.
  `no_custom`: the answer must be an option (meaningless without options).
- `answer {value, ask_id}` (command): an answer naming a question that is no
  longer open is dropped by the engine, not taken by the next question. dund
  sets `by` from the sender's principal.
- `answered {ask_id, value, by}`: the question is closed; its answer is the
  person's row, live. `ask_user`'s live `tool_call`/`tool_result` add no row.
- `mount {ask_id, …}` is answered the same way.

## Request stats

- `request {phase, …, session}`: one model round's figures (`proto.Request`,
  `dun/reqstats.go`), and in `session_stats` the session's so far
  (`proto.RequestSession`, folded by the engine, `proto.RequestFold`):
  `ttft_ms`, `pp`, `tg` the last measured (a start clears none; `tg` moves
  while a reply streams), `pp_avg`/`tg_avg` the averages over finished
  rounds weighted by tokens (prompt less cached for pp, generated for tg),
  `cache_seen` (some round reported the cache field; an unreported cache is
  not 0%), `cache_pct` the last finished round's cached share (absent until
  one reported it).
- `request` is state: a snapshot keeps the last one, and it is the whole
  status bar. Clients show it and add nothing up, so a reconnect does not
  start the averages again.
- Order: a round's `request` `done` (and its spend and turn_stats share) comes
  before the turn's `usage` and `done`, the last round too. The engine holds
  a round's final stream chunk until the client has priced the round
  (`reasoningTee`).
- Scope: the session, across engines (owner, 2026-11-14). An engine restart
  (a crash, an idle stop and a wake, a model switch) does not start the
  figures again: the engine seeds its fold at start, before its first
  request, from the session's stats file (see "Session totals across
  engines" below), so its first `request` already carries the session's
  figures. The last-measured `ttft_ms`/`pp`/`tg` are the last engine's
  until a new round measures them.
- Old logs: a `request` with no `session_stats` (an older engine) shows its own
  figures and no averages; marked in both clients.

## Usage

- `usage {total, cached, processed, generated, turns, active, …, spend,
  turn_stats}` before each `done`. The token counts and `turns` (model
  rounds) are the session's, **cumulative** (agentkit's tally); `active` is
  the window now; `turn_stats` is the turn's own share; `spend` the
  session's money and limits (`proto.Spend`).
- A client **sets** them, it does not add them up: the last `usage` is the
  whole count, a snapshot keeps it (state), so a reconnected client is
  right. (Both clients used to add the counts, which counted every earlier
  turn again on each turn.) A turn cut before its first round sends the
  last totals, not zeros.
- Scope: the session, as `request`: the token counts, `turns` and the
  spend's session part (session, this hour, today, the interval, the last
  turn, the calls) continue across an engine restart and a model switch.
  The engine's first `usage` and `spend` after one carry the session's whole
  count. `active` is the new engine's window.
- Model switch: one session total over every model the session ran on; no
  per-session breakdown by model. The per-model rows in the cost view are
  the ledger's (`ledger.by_model`: this machine's month, by model), as
  before.

### Session totals across engines

- What an engine seeds from at start (`cmd/dun/tally.go`):
  - money: the spend ledger's lines for the session. Each line names its
    session id and how many of the session's turns had ended (`session`,
    `turn`; `internal/spendmeter/ledger.go`), so the session, hour, today,
    interval and last-turn sums are the ledger's (`spendmeter.Meter.Seed`).
  - tokens, rounds, the request fold's sums and the meter's other counts
    (calls, tokens over all calls): the session's stats file,
    `<id>.stats.json` beside `<id>.jsonl` (`dun.StatsFile`), rewritten after
    every round and every turn. Beside the history, not in it: a fold or a
    recap removes the rounds it summarizes, and their per-entry usage.
- Fallback (an older engine's session, no stats file): the per-entry usage
  in the history (`Harness.RoundUsages`). Tokens and rounds of the entries
  still in it; `pp_avg`/`tg_avg` only from rounds with the server's timings
  (the client-side ttft and stream times were never saved); no money (older
  ledger lines name no session).
- Fallback (no stats file, no per-entry usage): from 0.
- `/clear` keeps the totals: the money was spent.
- `spend` between turns, and `usage.spend`, carry the whole cost view: what
  the session spent (and this hour, today, the interval), the last turn,
  the key's budget, the rate-limit windows, and this machine's ledger. The
  TUI's /usage and the web client's Context sheet (`lib/costview.ts`) show
  the same rows.
- Old logs: same fields, same meaning; nothing changed on the wire.

## Thinking rounds

- A round is one model request of the turn (its `request` `start` begins
  it; side calls and sub-agents are not rounds). `reasoning {text, round}`,
  `token {text, round}` and `tool_call {…, round}` name theirs; the first
  round of an engine is 1.
- `reasoning_end {round, cut?}`: round `round`'s thinking is over. Sent once
  per round that thought, right before the first event that ends it: the
  round's first token or tool call, its request's `done`, the next round's
  `start`, a `message`, `ask`, `mount`, `error`, `retry` or `done`. `cut`:
  the turn was interrupted mid-thought (the `done` with reason
  `interrupted` follows).
- A client opens a thought at a round's first `reasoning`, grows it, and
  closes it at `reasoning_end`; it does not end a thought on a token, a
  tool call or the turn's end. A `reasoning` of another round than the
  live thought's opens a new one. Where a thought is shown (its own block,
  or a step of the open tool run), its title (`# Thinking:` / `# Summary:`)
  and the run's wording stay presentation, client-side.
- Owner: the engine (`proto.StepRound`, applied in `cmd/dun/emit.go` as each
  event is written), so the rule is one place, not one list per client.
- Old logs: events with no `round` and no `reasoning_end` (an older engine)
  end a thought on the events listed above, guessed as before; marked in
  both clients (`endThinkingOld`, `endsThinkingOld`), used only until an
  event names its round.

## History reason

- `history {items, reason}`: an engine's replay of the conversation it
  started on. dund sets `reason` (`internal/dund/session.go`
  historyReason), because only dund knows why that engine started:
  - `resume`: the session's first engine in this dund's life, on a stored
    conversation. Shown, with the "resumed N entries" marker; a client that
    already shows it (read from disk while dund was away) replaces it.
  - `restart`: another engine on the conversation the clients already have:
    a crash restart, an idle stop then a wake, a restart with the same
    flags, the first engine after dund itself restarted. A client that
    shows a conversation keeps it, and the lines it added since (the
    restart's notice); one that shows nothing draws it, with no marker.
  - `model_switch`: a restart on another `--model` or `--url`. A client
    replaces what it shows, with no marker: nothing was resumed.
- The TUI no longer guesses this (it used to skip the next history after
  "restarting", and mark its own model pick to replay quietly).
- Old logs: a history with no reason (a dund before reasons) reads as
  `resume`, except in the TUI right after an `engine {restarting}`, where
  it is skipped as before (marked fallback, `restartedOld`).

## MCP login

- `mcp_login {server, url, phase, error?}`: an OAuth MCP server's login.
  `phase: started` with the URL to open (`url` "" while a login another
  session started is waited on); then `done` when it went through, or
  `failed` with `error`. One end per start the client was shown.
- Sent by whoever runs the login: the engine (its own token, `OnLogin` /
  `OnLoginEnd`, `internal/oauth/mcpoauth.go`), or dund (the broker for an engine's token,
  `internal/dund/oauth.go`; the gateway for a shared server, `mcpgw.go`).
- A client settles the start's notice on the end; it does not infer the
  outcome from a later `mcp_tools` listing (the web client did, and could
  not show a failure).
- Old logs: an `mcp_login` with no phase is a start; its notice is settled
  by the server's tool list as before (marked fallback, mobile `legacy`).
