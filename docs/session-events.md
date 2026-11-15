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
