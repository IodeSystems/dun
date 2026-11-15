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
  id, a `tool_call`/`tool_result` by `call_id`, a `message` equal to the last
  reply. An entry is in the snapshot or the replay, not both.
