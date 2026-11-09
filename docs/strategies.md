# Memories, strategies and constraints

Built. Code: `notekinds.go` (kinds, names, the standing list, the
`file_read` paths, `/memories`), `memory.go` (the tool and the store),
`memrecall.go` (dund link, sync, recall). Related: `memory.md`, `rag.md`
("Built: slice 5"), `surfacing.md`, `reading.md`.

## What they are

(USER 2026-11-08.) "Rules/strategies are: global, project and important.
And have short name and optional detail. Important rules are always
surfaced with short name." Then: memories get the same shape, and the
user-authored kind is called **constraint**, not rule.

| kind | what | written by |
|---|---|---|
| memory | a fact a later session needs (`memory.md`) | the model, `/remember`, `/memories add` |
| strategy | an approach that worked and will work again | the model, `/memories add`; the learning phase, as a candidate |
| constraint | a rule the user set for how to work | the user (`/memories add`), or the model when the user states it |

**Not the rules files.** dun's rules feature (`rules.go`: the workspace's
AGENTS.md and CLAUDE.md and their `@imports`, `/rules`) is unchanged and
separate. A rules file is a whole document the user maintains in the
repo; a constraint is one short named item in dun's own store. The name
"rule" was not used for the new kind because it clashes with those files.

## One format

A markdown file, frontmatter then the detail:

```
---
name: no-push-unless-asked
kind: constraint
important: true
date: 2026-11-08
tags: git
public: false
---
Do not push unless the user asks.
```

- `name` is a slug (lower case, words joined by `-`, at most 48 bytes),
  unique per kind and scope. A memory written before names is named by
  its file name without `.md`; a new one by its `name`, else the first
  words of its question.
- `kind` is absent for a memory.
- Scope is the directory: `memory/<project>/` and `memory/@global/` for
  memories, `strategies/<project>/` and `strategies/@global/` for
  strategies and constraints, all under `$DUN_HOME`. A strategy or
  constraint file is `<kind>-<name>.md`.
- The path the model and `/memories` use is `<kind>:<name>`. A name in
  both scopes resolves to the project's.

## Indexes

dund keeps four indexes (`rag.md`). Memories go to `memories`;
strategies and constraints both go to `strategies`, told apart by
`kind`, so a lesson's retrieval is not crowded by project facts. Both use
the same store code (`memoryStore`), the same `rag_ingest`, `rag_tag`,
`rag_forget` and `rag_sync` calls, and the same attestation log (refine,
void, confirm). Each index has its own link, so each syncs on its own.

Each item is tagged in its index (raglit `SetTags`: filter data, no
fragment) with its own tags, `kind:<kind>`, and `important` when marked.
A search with the tag filter `kind:strategy` finds strategies only.
An item is indexed as its name and detail (one fragment, no
frontmatter), so it is one hit. `/memories important` and `unimportant`
rewrite the file and send `rag_tag`, not an ingest; its text is the same,
so the next sync finds it current (`rag.md`, slice 5).

## What reaches the model

- **Important**: listed in the system prompt of every session, by path
  and scope (`- constraint:no-push-unless-asked (this project)`), with no
  retrieval. The detail is a `file_read` of the path. The list holds
  **12 items across the three kinds together**, in this order:
  constraints, strategies, memories; project before global; by name. Past
  12, a line says how many more there are; those are not listed and are
  found by retrieval like the rest. `/memories important` says when the
  list is full. A listed item is not surfaced again by retrieval.
- **Not important**: surfaced by retrieval under the rule in
  `surfacing.md`. Strategies and constraints are their own groups
  (project, global), apart from memory notes; project first.
- **Only the user marks an item important** (`/memories important`). The
  tool has no `important` parameter: a model-written list on every
  request was refused for memory (USER 2026-10-19, `memory.md`,
  Personalization).
- A change to the important set (the user command, or the tool voiding a
  listed item) rebuilds the system prompt after the turn.

## Reading

`file_read` with `memory:<name>`, `strategy:<name>` or `constraint:<name>`
returns the file (frontmatter and detail). An unknown name is an error
that lists the names of that kind. Only these prefixes are built; the
rest of `reading.md` (whole reads, budgets, `doc:`) is not.

## Writing

- **Model**: the `memory` tool, with `kind` and `name` added. One tool,
  not a second one: the actions (add, refine, void, confirm), the scan
  and the attestation are the same for every kind, and the memory fork
  (`/remember`) shares the turn's tool list byte for byte. refine, void
  and confirm take the path as `old_text`.
- **User**: `/memories`.
  - `/memories` lists all three kinds, important ones marked.
  - `/memories add memory|strategy|constraint [global] <name> [detail]`;
    a name the scope already has is replaced (Decisions, below).
  - `/memories rm <kind:name>`: a void with the user as source (the file
    is kept as `*.md.void`).
  - `/memories important <kind:name>`, `/memories unimportant <kind:name>`.
  - `/memories candidates`, `/memories accept|reject <kind:name>`: the
    learning phase's proposals (below).

## Off

`DUN_MEMORY=0` turns off all three: no RagLink, so no tool, no recall,
no standing list, no `file_read` paths. The Harbor adapter sets it unless
the worker's environment sets `DUN_MEMORY` (`examples/harbor`). No second
switch: the three share one link and one tool.

## dund down

As for memory (`memory.md`): the file is written and the result says it
is not indexed yet; recall is a word search of the files; the next
contact syncs the set. The standing list reads the files, so it does not
depend on dund.

## Learning phase (first slice, built)

Code: `learn.go`; the streak is `plannudge.go`'s. Churn → resolved
candidate → lesson, written as a strategy the user reviews.

Constraints (USER 2026-11-09): lessons are generic (no task names, paths,
file contents or answers; a lesson useless without its task is dropped,
so a benchmark score measures retrieval, not memorization); only the user
marks an item important; off in evals; one small bounded call per
resolved episode, never per round; lessons from resolutions only.

- **Episode.** The plan nudge's failure streak (`nudge.fails`, the one
  counter; a failure is what it already counts: ERROR, a dispatcher
  error, a failed exec) of at least 3 results, ended by a success on the
  target of one of the failed calls. A target is the tool, and its path
  when the call has one (`file_edit:a.go`); an exec's target is `exec`,
  so another command that works is the fix. A streak that never ends, is
  shorter than 3, or ends with a success on another target, gives
  nothing. The streak keeps its last 6 failed calls, each clipped (240
  bytes of the command or arguments, 320 of the result).
- **Stall episode** (USER 2026-11-09). The stalled-plan nudge fired
  (`plannudge.go`), then the main agent's plan moved (a node added, a
  status, goal or done_when changed). It holds the rounds stalled, the
  open nodes the nudge named, the last 6 calls before it, and the move.
  Same cap, same gate.
- **Gate: held until the task verifies** (USER 2026-11-09). Episodes are
  held in the session (at most 12; the oldest go), and the lesson calls
  are made only when the task verifies:
  - the turn ended without an error, after an end-of-turn check in that
    turn (`idlecheck.go`: the requirement checklist, or the open-nodes
    note), and the main agent's plan has a done node and none open
    (dropped counts as closed: a dropped approach replaced by another is
    normal). The checklist has no structured answer, so "all met" is the
    model ending the turn after it;
  - or the user ends a worklog task as done (`/worklog end`, outcome
    done): the only user signal of "it is done" dun has. No "accept the
    result" command exists.
  Episodes that never verify go with the session: they are held in
  memory only.
- **Call.** Once verified (in the background, the turn does not wait),
  one side call per held episode, kind `learn` in `/context`: no tools, no
  history, `max_tokens` 400, about 1.2k tokens in. The prompt is the
  episode only; the task's text is not in it. The model answers
  `{name, detail, tags}` or `{"skip": true}`.
- **Cap: 3 lesson calls per session** (`learnCap`). It counts calls, not
  episodes held; once 3 calls are made, no more episodes are held.
- **Genericity filter** (`genericityError`, deterministic, after the
  call). Rejected:
  - shape: a detail over 400 bytes or 3 sentences, a name over 6 words;
  - always: a path (`/a`, `./a`, `~/a`, `a/b/c`, `C:\`), a file name with
    a common extension, a URL, a hash (7+ hex characters with a digit and
    a letter), a number of 5+ digits or an IP address, a code fence, a
    quoted span over 40 characters;
  - a workspace file or directory name (3 levels down) that looks like an
    identifier (`_`, a dot, a digit, an inner capital, or `-` with a
    capital);
  - a token the task marks: in a quoted span of the user's messages, or
    with a digit, `_`, a dot or an inner capital (`POV-Ray`, `illum1`);
  - five words in a row from the user's messages;
  - an identifier from the episode's calls (snake_case, camelCase,
    dotted.name; not an ALL_CAPS variable such as `LD_LIBRARY_PATH`).
  It over-rejects on purpose: "python3" in the task rejects a lesson that
  says python3. No model self-check.
- **Dedup and recurrence.** Not written when a strategy of the same name
  exists or was voided (a rejected lesson is not proposed again), or one
  shares 70% of the shorter text's words: found by the strategies index
  (dund down: the files), candidates included, and among the voided ones.
  When the match is a current candidate, a `recurred` entry (by
  `dun:learn`) goes into its log; `/memories candidates` shows the count
  ("seen 3×"). A recurrence never accepts it: only the user does.
- **Write.** `strategyStore().apply` with kind strategy, project scope,
  the lesson's tags plus `candidate`: the file, the dund ingest, as the
  memory tool's add. Its attestation log gets `proposed`, by `dun:learn`.
  Not important. The user is told by a notice.
- **Excluded from recall** by a tag filter in dun: the recall finder
  drops a hit whose file has the tag `candidate` (dund's search and the
  word search of the files). Not in raglit's filter: it keeps documents
  with every tag given and cannot exclude one. raglit is asked for an
  exclude-tag filter (`NotTags`, USER 2026-11-09); the drop in dun stays
  until dund passes it. A candidate still reads
  with `file_read strategy:<name>`.
- **Review** (`/memories`):
  - `/memories candidates` lists them;
  - `/memories accept strategy:<name>`: the tag goes (a retag, no
    ingest), a `confirmed` entry by the user; recall finds it from then;
  - `/memories reject strategy:<name>`: a void by the user;
  - `/memories important` refuses a candidate: accept it first.
  The model cannot set the tag (the memory tool refuses it) and a refine
  keeps it.
- **Off**: no strategies store (`DUN_MEMORY=0`, which the Harbor adapter
  sets; no dund link), and in a sub-agent.

Replay (2026-11-09, no model calls: `TestLearn_Replay` with
`DUN_CHURN_REPLAY` set to a glob of `agent/dun.jsonl`), 362 Terminal-Bench
trials in `examples/harbor/jobs`, the trial's reward standing in for the
gate:

- failure episodes only (first slice): 58 episodes in 44 trials; 189
  passed trials had 15, 158 failed had 39, 15 unscored had 4.
- with stall episodes (approximated from the plan calls; the stream has
  no plan state): 109 episodes in 62 trials, 51 of them stalls; passed
  trials 22, failed 83, unscored 4.
- lesson calls at 3 a trial: 99 without the gate, 22 with it. Most churn
  is in trials that never verify, and the gate drops it.

## Decisions (USER 2026-11-08)

- Only the user marks an item important; the tool cannot.
- The standing list holds 12 items shared across the kinds, constraints
  first.
- A name in both scopes resolves to the project's.
- The user's `/memories add` of a name that exists (same kind, scope and
  name) replaces it: the old detail goes into the attestation log
  (`corrected`, `was`), the file keeps its name, date and important flag,
  it is indexed again, and the reply says it replaced an item. The
  model's add of an existing name is refused.
