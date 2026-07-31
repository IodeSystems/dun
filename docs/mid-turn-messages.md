# Mid-Turn Messages

When the user types a message while the agent is already working, it doesn't
wait for the turn to finish. Instead, it's **buffered and lifted into the next
tool result**, so the model reads it inside the running turn — no extra
round-trip, no assistant message stacked on another.

## Engine Side (`harness.go`)

### `Harness.Say(text string)`

Buffers a user message into `h.queue` (a `[]queued` slice). The queue is
guarded by `h.noteMu`. Multiple messages batch without limit.

```go
func (h *Harness) Say(text string) {
    h.noteMu.Lock()
    h.queue = append(h.queue, queued{kind: queuedUser, text: text})
    h.noteMu.Unlock()
}
```

### `Harness.liftQueued(result string) string`

Drains the queue into a tool result. Called by `withLiftedQueue` wrapper around
every tool dispatch. Appends queued items (with prefix) to the tool's output,
separated by blank lines. Returns the result unchanged when nothing is buffered.

```go
func (h *Harness) liftQueued(result string) string {
    h.noteMu.Lock()
    items := h.queue
    h.queue = nil
    h.noteMu.Unlock()
    if len(items) == 0 {
        return result
    }
    var b strings.Builder
    b.WriteString(strings.TrimRight(result, "\n"))
    for _, q := range items {
        b.WriteString("\n\n")
        b.WriteString(q.prefix())
        b.WriteString(q.text)
    }
    return b.String()
}
```

### `withLiftedQueue(inner ToolDispatcher, h *Harness) ToolDispatcher`

Wraps the tool dispatcher so every tool result passes through `liftQueued`.
This is the injection point — the model is already reading the result, so the
news costs no extra turn.

### `Harness.flushQueued() int`

Called when a turn ends without the queue being picked up by `liftQueued`.
Publishes remaining items to the store inbox (user messages as `KindUser`,
notifications as `KindNotification`). Asides are kept in the queue (not flushed)
— they're context, not news.

### `queuedKind`

Three kinds of buffered items:
- `queuedUser` — a message the user typed mid-turn
- `queuedNotification` — a background job completion
- `queuedAside` — session context (e.g., tool set changed), never triggers a turn

## TUI Side (`cmd/dun/tui.go`)

### Flow

1. User types message while `busy` → `sendUser()` echoes it in convo and sends
   to engine via `proc.send()`.
2. Engine buffers it (via `Say`) and emits `{"type":"queued","text":"...","count":N}`.
3. TUI `handleEvent("queued")` removes the echo from convo, stores text in
   `queuedTexts`, updates `queuedMsgs` count.
4. `pendingView()` renders queued messages above the divider (dimmed).
5. On `done` or `error`, queued messages are moved into the convo at the
   delivery point.

### `queuedTexts []string`

Stores the text of each pending message. Populated on `queued` event, cleared
on `done`/`error`.

### `pendingView() string`

Renders queued messages above the divider. Each line is `stDim.Render("› "+txt)`.
Empty string when no messages are pending.

### Status Bar

`queuedHint()` appends to the busy status: `(1 message queued for this turn)`.

## Programmatic Mode (`cmd/dun/main.go`)

In `-p` mode, `inputStream.setMidTurn()` handles the buffering:

```go
in.setMidTurn(func(text string) bool {
    if !turnActive.Load() {
        return false
    }
    h.Say(text)
    em.emit(event{"type": "queued", "text": text, "count": h.Queued()})
    return true
})
```

Returns `true` if the message was buffered (turn active), `false` if it should
start a new turn. The `queued` event is emitted so the TUI knows.
