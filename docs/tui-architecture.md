# TUI Architecture

The TUI uses [Bubble Tea](https://github.com/charmbracelet/bubbletea) (The Elm
Architecture in Go) with [Lip Gloss](https://github.com/charmbracelet/lipgloss)
for styling and [Glamour](https://github.com/charmbracelet/glamour) for markdown
rendering.

## Model (`tuiModel`)

The single state object. Key fields:

- **`convo []convoEntry`** — finalized conversation blocks (user messages,
  assistant replies, tool calls/results, notifications). Each entry has a
  `collapsed` view (always shown) and optionally a `full` view (expanded on
  enter).
- **`cur string`** — streaming assistant text not yet finalized. Flushed into
  `convo` on `done`/`error`/`ask` via `flushCur()`.
- **`pendingTool int`** — index of a tool call awaiting its result (-1 = none).
  Used to fold `tool_result` back into its matching `tool_call`.
- **`queuedTexts []string`** — messages typed mid-turn, shown in a pending area
  above the divider until delivered at turn end.
- **`busy bool`** — a turn is in flight.
- **`asking bool`** — agent is waiting on `ask_user` answer.
- **`focus int`** — `focusInput` (text entry) or `focusConvo` (scrollable
  conversation). Toggled with tab.
- **`sel int`** — selected message index in convo focus mode.
- **`vp viewport.Model`** — the scrollable conversation viewport.
- **`input textinput.Model`** — the text entry field.

## View Pipeline

```
View() → head + viewportView(vp) + pendingView() + divider + lowerView() + status
```

1. **head** — "dun" header with version, workspace, branch, tool count.
2. **viewportView(vp)** — the conversation viewport, cached via `viewportCache`
   to avoid re-rendering on every token. The cache key is `(contentGen,
   YOffset, Width, Height)`.
3. **pendingView()** — queued messages above the divider (dimmed, "› " prefix).
4. **divider** — a thin rule with half-edge focus cue (bright on the focused
   pane's side).
5. **lowerView()** — input line, or ask panel, or palette, or suggestions.
6. **status** — context-sensitive status bar (busy, ready, searching, etc.).

### Rendering Optimization

The viewport's rendered text is cached behind a **pointer** (`*viewportCache`)
on purpose. Bubble Tea calls `View()` after every message — including each
streamed token — and the viewport re-slices and re-joins its window on every
call. Without caching, a 100-token reply at 60 tokens/s would cost ~1s of CPU
just in viewport re-slicing, on the goroutine that also reads the keyboard.

`refresh()` is the only place that sets viewport content. It bumps `contentGen`,
invalidating the cache. The cache is checked in `viewportView()` before calling
`vp.View()`.

### Text Wrapping Cache

Each `convoEntry` caches its wrapped render (`wrapped`, `wrapW`, `wrapOpen`).
Invalidated by width change (resize) or open state (expand/collapse). Docs
blocks opt out — their inner per-doc state is not captured in the cache.

## Update Loop

`Update(msg)` dispatches to:
- `update()` — normal key handling, events, window resize, mouse scroll
- `updateAsking()` — ask_user answer mode (option picker, free text)
- `updateSearch()` — `/` search mode
- `updatePicking()` — session picker mode
- Inspector overlay — full-screen tool inspector (owns all keys while open)

Events from the engine arrive as `evMsg`. The `handleEvent()` method processes
them, returning a new model. A `renderTickMsg` paces the redraws.

## Layout

```
┌─────────────────────────────────────────────────────────┐
│ dun dev  /workspace  · 5 tools                          │  ← head (1 row)
├─────────────────────────────────────────────────────────┤
│ › user message                                          │
│ assistant reply (markdown rendered)                     │  ← viewport
│ ⚙ tool_call(args)                                      │  ← scrollable
│ ...                                                     │
├─────────────────────────────────────────────────────────┤  ← divider (1 row)
│ › pending message 1  (dimmed, if queued)                │  ← pending area
│ › pending message 2                                     │
├─────────────────────────────────────────────────────────┤  ← divider
│ › ask dun to do something…                              │  ← lower (input)
├─────────────────────────────────────────────────────────┤
│ ⣾  working… (1 message queued for this turn)  (ctrl+c) │  ← status (1 row)
└─────────────────────────────────────────────────────────┘
```

The conversation height is computed as:
`convoH = h - 3 - pendingH - lowerH` (head + divider + status = 3 fixed rows).

## Styles

| Style | Color | Purpose |
|-------|-------|---------|
| `stHeader` | 212 (yellow) bold | "dun" header |
| `stDim` | 240 (dark gray) | dim text, hints, secondary info |
| `stUser` | 39 (green) bold | user messages |
| `stTool` | 42 (green) | tool calls |
| `stErr` | 196 (red) | errors |
| `stNote` | 214 (yellow) | notifications, proactive RAG |
| `stAsk` | 213 (yellow) bold | ask_user prompts |
| `stSel` | 212 (yellow) bold | selection gutter |
| `stEdge` | 212 (yellow) | focused divider half |
