# Memory

Notes a later session on a project needs and cannot get from the code, the git
history or the docs. Code: `memory.go` (tool, store, scan), `memattest.go`
(refine, void, confirm), `memtrigger.go` (triggers), `fork.go` and `suggest.go`
(the fork's prefix), `notify.go` (surfacing). The retrieval rule itself is in
`surfacing.md`; this file is why memory is built around it.

## The value is surfacing, not writing

(USER 2026-10-19.) A note is worth writing only if a raglit index can put it
into the conversation unasked: the model reads it inside a tool result it was
already going to read, without calling `search`. The path already existed
(`docsPreparer` appends hits as one Entry, which keeps the cached prefix), so
surfacing was proved on hand-written notes before any write path was built. The
memory tool exists only while the docs server runs, because without the search
a note is never read again.

**Surfaced text is not a one-time cost.** The Entry stays in history, so it is
re-sent every turn until compaction drops it. That is why `surfacing.go` counts
bytes, tokens, latency and CARRIED tokens (entries still in history, per
round; a fold stops an entry accruing), shown in `/context`. Early token
numbers ran about 4x high until the token meter settled
(`providers-and-cost.md`); bytes are exact.

## What surfaces, and the first measurements

M0 (2026-10-20): 11 hand-written notes, 25 probes, an index holding only the
notes, image captioning off (an embedded serve captions mid-run and killed
sessions left 3 of 11 notes captioned, which biased results). Through the CLI,
lexical bm25 found the right note on 10 of 11 probes (10 at rank 1), hybrid on
11. The same through real `dun -p --rag` sessions: 10 of 11. Caveats: 11 notes,
probes written by the author of the notes.

- **Noise was the problem.** With no floor and two slots, 11 of 21 surfaced
  notes were wrong and every negative probe surfaced two.
- BM25 scores are not corpus-independent (an absolute 3.5 worked on 11 notes
  and will not transfer), and hybrid scores are rank fusion (about 0.033 for
  right and wrong alike), so neither can be thresholded. The corpus-independent
  rule was **top hit only, and only if at least 1.5x the runner-up**: 10 of 11
  right, 0 wrong live. The embedding-era rule that replaced it for indexes with
  embeddings is in `surfacing.md`.
- **What surfaced was a pointer** (title plus first line), so the model had to
  call `search` to read it. Notes are short by design, so a document of at most
  1500 chars (`inlineMaxChars`) is inlined whole via `get_document`; longer
  stays a pointer. This applies to all proactive retrieval, not only memory.
  Note files stay under it (`memoryMaxChars` 1200 plus frontmatter).
- A document with two fragments (text plus caption) took both slots, so a pass
  keeps one slot per document, the best fragment.
- **Per group.** BM25 scores do not compare across indexes: one stash note
  scored 8.5e-6 in the memory index while a code file scored 2.3e-5 in the code
  index, so the code file won every pass. The rule runs per group (memory notes
  versus the rest) and a pass can surface one of each. A surfaced note is
  labelled as saved by an earlier session.
- With one or two notes the margin has no runner-up, so any word overlap
  surfaces a note; the rule works as notes accumulate.

## The write side

**Hermes Agent's background review** was the model (read from
NousResearch/hermes-agent @ 0b10b9f8). The main turn carries no memory
instruction. Either the agent calls a normal `memory` tool in-turn, or a fork
fires after the turn. **Cache parity is the whole trick**: the fork reuses the
parent's system prompt verbatim and a byte-identical `tools[]`, limits what it
may do by a dispatch-side whitelist, and appends its instruction as one user
message after the snapshot. Request one is a warm cache read and the generation
is small. A different model has a cold cache, so it would need a digest instead.
Hermes triggers by a user-turn counter (default every 10 turns without a write).

dun's decisions:

- **No turn counter** (USER 2026-10-19). Triggers: the model calls `memory`
  explicitly; a trigger word forces it; optionally before compaction. Fork also
  as `/remember [text]`.
- **Writes go straight to disk, no approval step** (USER), after a scan that
  refuses control and format characters, chat-template tokens, and over 1200
  chars. A note is one file in `~/.dun/memory/<project>/` (frontmatter:
  question, date, session, commit, public), indexed in the project's `memory`
  index. Local first (USER 2026-10-21); `public` is recorded and changes
  nothing yet.
- **The fork shares the turn's prefix.** Measured by replaying a captured
  6 592-token turn (M1, 2026-10-21): no tools, 0 cached of 3 878; the turn's
  tools, 6 426 cached and 166 processed. `tool_choice` `none`, `required` and a
  named tool all keep the prefix (they change the grammar only);
  `response_format: json_object` together with tools is HTTP 400 "failed to
  parse grammar". Live dun found a second break the replay hid: suggest built
  with an empty system prompt, 0 cached of 4 474. `forkSystem` now mirrors the
  turn's prompt and `forkTools` sends the turn's tools with `tool_choice: none`;
  `TestSuggest_SharesTheTurnsPrefix` fails if the prefix differs. So every tool
  stays in the request and only the forced tool's calls are run. Fork right
  after `done`, back to back (past about 0.75 s the slot is likely gone on a
  shared server), and hold `turnMu`.
- **Triggers were measured before built** (M3), over 274 dun logs plus the
  owner's Claude Code transcripts (3 047 user and 13 854 assistant messages).
  "Remember" opening a clause was a standing rule or project fact in 10 of 10
  deduplicated cases; mid-clause ("I remember", "don't remember") 0 of 4;
  about 0.3% of user messages. "I'll remember" appears 0 times in 18k assistant
  texts. So after each turn the new entries are scanned once; either signal
  with no memory call in the turn runs the fork in the background on a detached
  context (2 min bound). Children do not trigger. `DUN_MEMORY_TRIGGERS=0`
  turns it off. Corrections without the word ("don't push unless asked") are
  left to the model's own call.
- Side calls must read to the end of the stream: they stopped at Done, before
  the `include_usage` chunk, so their stats were 0.
- The note text must not invent a reason: asking for "why it holds"
  unconditionally made the model make one up.

## Staleness is checked at the moment of use

(USER 2026-08-02.) A note can be wrong and still carry the authority of having
been confirmed: measured twice on one task, the model wrote "3000 lines" for a
4000-line file and the human approved it. Noticing staleness in the abstract is
unsolvable. Noticing it while working in the code the note describes is easy:
the model holds the evidence at exactly that moment. So **the surfacing itself
carries the caveat and the invitation** (`notify.go`, group memory): a note
"records what was true then; if the user or the code now says otherwise, fix it
- memory action=refine (or void ...)".

- **A note is addressed by a phrase or its question, never an id** (`old_text`).
  The model invents ids when it must carry them (`exec_2`, which matched
  nothing, seen live twice). The question is what was surfaced, it is readable,
  and it is what a later user would ask.
- **A refine comes from the user or from reality, so it applies without
  asking** (USER, M4). Verdicts are `refine`, `void`, `confirm`, each with `why`
  (the evidence) and `source` (user or code). The note file stays the current
  answer, which is what the index serves. Every verdict is appended to
  `<note>.md.attest.jsonl` in raglit's `attest.Entry` shape plus `was` (the
  replaced text). Append-only, because a mutable record answers "what does this
  say now" and destroys how it got there; a note corrected twice and then
  confirmed is a different object from one that was right the first time. A
  voided file is kept as `*.md.void`; `/memories` lists notes and history.
- **void maps to `unsupported`, not `retract`.** In raglit's
  `attest/attestation.go`, `retract` withdraws a ruling and leaves the unit
  untouched. raglit's `answered-questions.md` table says retract and disagrees
  with its code; the code is what dun follows.
- **Staleness detects itself when the unit is a content id.** `unit` is the id
  of the question plus the answer it came from, so a confirmation of since-
  refined text shows as "confirmed before a later change", and raglit's
  `Orphaned` reports a verdict whose unit no longer exists. What remains is the
  note that was wrong from the start and never changed; the cue in the surfaced
  text is for that. Neither mechanism finds the other's case.
- Confirm quality depends on the model checking first. The tool text forbids
  invented dates and confirming without reading the source; that is wording, not
  enforcement.
- **Do not save what the repo records**: code structure, past fixes, git
  history are re-derivable and the fastest to go stale. The residue worth
  keeping is what is not in the tree: why an approach was abandoned, what a
  measurement showed, which reading of an ambiguous request was right.
- **Personalization**: no model-written pinned list (USER 2026-10-19). A pinned
  list reaches every turn through the system prompt, so a wrong model-inferred
  entry ("user prefers X") would steer every session and could override the
  current request. Standing preferences belong in a user-level file the user
  edits by hand (`~/.dun/AGENTS.md`, loaded after the workspace's root
  AGENTS.md and labelled as defaults the request overrides); that file is not
  built. A "remember I want..." goes to a private note and the reply tells the
  user to move it if it should apply every turn.

## Sharing (designed with raglit, not built in dun)

Two axes: scope (project or user) and visibility (private or public). Decisions
(USER 2026-10-19/20):

- A write is private unless the call says `public:true`: a leak cannot be
  undone, a missed share can.
- `/rag on` registers the project with the raglit server under a project id
  (the repo's root commit, label the normalized `origin`). No index for the id:
  it becomes the base. One exists: you get a top layer over it, and write only
  your own layer. Every layer sees public sibling layers in search.
- A maintainer service running as its own machine user owns and alone writes
  the base, folding public layer notes into it (`raglit maintain`). With
  several layers and no maintained base, raglit recommends one and reports the
  duplicate cost.
- Auth: dun passes the human's OIDC token (device flow for the CLI, browser
  login for the served UI), because there are no LLM-capable dev machines and
  the model, raglit and possibly dun run remote. Write is owner-only in raglit,
  so a machine-user dun would make the machine the owner of "private" notes;
  that is why layers exist.
