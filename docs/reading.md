# Reading: virtual paths and a budgeted whole read

A proposal, not built. Code today: agentkit `files/read.go` (the caps), dun
`recap.go` (large-result cues), `exec.go` (clipping). Related: `rag.md` (what
the virtual paths would read), `memory.md`.

## What exists

- `file_read` caps: an unranged read gives the first 8KB, or only the first
  2KB of a file of 64KB or more; a ranged read gives up to 64KB. The caps were
  set from measurement (agentkit `read.go`: 256KB reads moved turn latency from
  58s to 337s). Nothing overrides them.
- A tool result over 20000 characters reaches the model clipped to its start
  and end; the rest is saved to a file the model reads with `file_read`.
- Recall by reference was retired (`read_saved`, 0cd5c73): a clipped result now
  names a path. `recap` only compresses the conversation; it recalls nothing
  (USER 2026-11-08: "we killed it as a recall").
- `recap.go` already cues a recap for a large result once the model has moved
  on (`largeResultChars`, 20000).

## 1. Virtual paths

(USER 2026-11-08: "virtual file read then? Searchpath? Even for raglit.")
`file_read` takes namespaced paths besides files:

- `rule:<name>`, `strategy:<name>`, `memory:<question-slug>` — the stores in
  `rag.md`, each a file with a short name and optional detail;
- `doc:<index>/<path>` — a ragcore document, whole or ranged like a file;
- saved outputs, by the path a clipped result already names.

**Explicit prefixes, not a search path.** A bare name resolved across places is
ambiguous, and models invent ids they must carry (`memory.md`: `exec_2`, seen
live twice). Whatever dun surfaces carries the exact path, so the model copies
it. Same tool, same caps, same errors: nothing new to learn. An important rule
is surfaced as short name plus path; its detail is a `file_read` away.

## 2. A whole read, bounded by the context left

`file_read({path, whole: true})` breaks the fixed cap. The limit is the room
left: the window, minus what is in use, minus a reserve for the reply (the
window is tracked already: the `window` field of request events).

| size against room left | result |
|---|---|
| small | whole, as now |
| large, fits (over ~5k tokens and under ~a quarter of the room) | whole, plus an aside: "This read is N tokens. Take what you need from it, then recap it on your next turn." (the existing large-result recap cue) |
| would blow the context | refused, with every way out that applies, in order (below) |

The refusal says the numbers ("X is N tokens; M are free") and then:

1. **Compact, then retry** (USER 2026-11-08): "recap what is settled to free at
   least N−M tokens, then read it again." Always available.
2. **Delegate** — only when the `agent` tool is in this agent's tool set
   (children and some modes have none): "or `agent({prompt: "read X and
   report …"})`: a child spends its own context and returns the findings."
   If X is larger than a child's whole window, say to narrow instead.
3. **Narrow**: grep it first, or read a range.

## Cost and ownership

Mostly dun: the budget check, the aside and the refusal wrap agentkit's read;
the namespaces are a resolver in front of it. An agentkit change only if
`whole` and the cap override must live in agentkit's tool definition; that goes
through agentkit-f2.

## Open

- The thresholds (5k tokens, a quarter of the room, the reply reserve) are
  starting points; measure on real sessions.
- Whether `doc:` reads go through dund's socket (rag.md) or the session's own
  store handle.
