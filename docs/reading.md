# Reading: virtual paths and a budgeted whole read

Built: the item paths (`memory:`, `strategy:`, `constraint:`; `strategies.md`,
`notekinds.go`) and the whole read (section 2, `wholeread.go`). Not built:
`doc:` paths. Code: agentkit `files/read.go` (the caps), dun `wholeread.go`
(the whole read), `recap.go` (large-result cues), `exec.go` (clipping). Related: `rag.md` (what
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

- `constraint:<name>`, `strategy:<name>`, `memory:<name>` — the stores in
  `strategies.md`, each a file with a short name and optional detail
  (built; "constraint", not "rule": the rules files are another thing);
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
| over a quarter, within the room | whole, plus a stronger aside: "This read is N tokens, over a quarter of the room left. Take what you need from it and recap it right after, this turn or the next." |
| would blow the context (over the room) | refused, with every way out that applies, in order (below) |

(USER 2026-11-09: "refuse a whole read only when it would blow the context."
The room is the free room minus the reply reserve; over a quarter of it the
aside is stronger. This replaced refusing everything over a quarter.)

The refusal says the numbers ("X is N tokens; M are free") and then:

1. **Compact, then retry** (USER 2026-11-08): "recap what is settled to free at
   least N−M tokens, then read it again." Always available.
2. **Delegate** — only when the `agent` tool is in this agent's tool set
   (children and some modes have none) and so is `plan` (a child starts only
   on a plan node, USER 2026-11-10): "add a plan node "read X and report …",
   then `agent({node: "<id>"})`: a child spends its own context and returns
   the findings."
   If X is larger than a child's whole window, say to narrow instead.
3. **Narrow**: grep it first, or read a range.

### As built (`wholeread.go`)

- **Wrapper, not agentkit.** `withFiles` routes the call as before (tree, tmp,
  `/file` paths) and hands a `whole` read to `Harness.readWhole`, which pages
  the file with ranged reads on that same toolset and joins the pages under
  one header. Root, refusals, the binary check and the read-before-write record
  stay agentkit's. A line longer than a page comes back cut, as a ranged read
  cuts it. The schema gains `whole` by wrapping the read def
  (`wholeReadDefs`): one boolean property and one description line:
  `{path:"docs/design.md", whole:true} reads all of a file, past those caps,
  when it fits the context left.` `whole` with offset/limit/at is refused;
  with grep it is a grep. Item paths answer first and ignore it.
- **The room**: `Window()`: window − max(last prompt, fixed overhead) −
  `outputReserve()` (the reply reserve already sent as `max_tokens`). Tool
  results since the last request are not counted.
- **Tokens**: file size at the meter's measured chars/token
  (`bytesToTokens`) for the check; the read's own text (`meter.Estimate`) for
  the aside's N.
- **Thresholds** (consts `wholeSmallTokens` = 5000, `wholeRoomShare` = 4):
  up to 5000 is served whatever the room (an ordinary ranged read is ~16k,
  unchecked); above that, up to a quarter of the room, served with the aside;
  up to the room, served with the stronger aside (`wholeAsideBig`); above,
  refused.
- **The aside is the large-result recap cue.** It claims the key that cue
  would claim later (`bigCueKey`, now by call id), so one large result gets
  one cue, and the cap's clip is skipped for that result (`keepWhole`): it was
  measured to fit.
- **The refusal's numbers.** M is the room (reserve already out), and the
  recap amount is N − M:

  ```
  ERROR: X is N tokens; M are free. Ways out:
  1. Recap what is settled to free at least N−M tokens, then read it again.
  2. Add a plan node "read X and report …", then agent({node: "<id>"}): a child spends its own context and returns the findings.
  3. Grep it first, or read a range.
  ```

  Recap is left out when it cannot free N − M (more than is in use past the
  fixed overhead, i.e. X is larger than an empty context's room): not "always
  available" as above, since it would be false advice.
  Delegation is offered only with `agent` in this agent's tool set; when X is
  larger than a child's whole window (window − overhead − reserve; a child is
  assumed to have this session's window) it reads "It is larger than a child's
  whole window: narrow it." instead.
- **No window known**: up to 5000 is served; above, refused with "the
  context window is unknown", and narrow.

## Cost and ownership

Mostly dun: the budget check, the aside and the refusal wrap agentkit's read;
the namespaces are a resolver in front of it. An agentkit change only if
`whole` and the cap override must live in agentkit's tool definition; that goes
through agentkit-f2.

## Open

- The thresholds (5k tokens, a quarter of the room, the reply reserve) are
  starting points; measure on real sessions (`wholeread.go`'s consts).
- A child's window is assumed equal to this session's; a child on another
  model may have another.
- Whether `doc:` reads go through dund's socket (rag.md) or the session's own
  store handle.
