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
| strategy | an approach that worked and will work again | the model, `/memories add`; later the learning phase |
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
the same store code (`memoryStore`), the same `rag_ingest`, `rag_forget`
and `rag_sync` calls, and the same attestation log (refine, void,
confirm). Each index has its own link, so each syncs on its own.

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

## Learning phase (not built)

The design in `rag.md` (churn, resolved candidate, lesson) would write a
lesson with `strategyStore().apply(memoryArgs{Action: "add", Kind:
"strategy", ...})`, the same call the tool makes (hook noted at the top
of `notekinds.go`).

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
