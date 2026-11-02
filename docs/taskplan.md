# The task plan

USER 2026-11-02: a plan system, a DAG of tasks a person can review, so agents are
more directed while talking with the user, and the main agent can delegate subgraphs
to sub-agents. A dun-internal tool group, not a separate server.

The worklog (`/worklog`) records what happened and is person-driven. The plan is
what is meant to happen, and the model keeps it. They stay separate.

## Layers

| Layer | Where | Holds |
|---|---|---|
| Graph | `internal/taskgraph` | Nodes, `needs`, cycle check, ready set, status rules, subgraph ownership. Stdlib only, no `Harness` |
| Tool | `internal/taskgraph` | The `plan` tool's definition and handler, over a `Store` interface |
| Host | root `dun` | Storage, the turn line, the tool group, delegation |
| Screen | `internal/tui` | `/plan` |

agentkit's rules say it does not own a task DAG (agentkit `CLAUDE.md`). So the graph
starts here. aw4 said no: its DAG is a projection of an event log. agentkit's owner
reads a reduced package as belonging there, as an optional sibling like `files` and
`loopdetector`, after delegation ships; that needs Carl's edit to agentkit's "no task
DAG" rule. So `internal/taskgraph` imports stdlib and agentkit's `llm` only, and knows
nothing of dun: no plan mode, no approval, no sub-agent numbering. Delegation and
scheduling stay in the host.

## A node

```
id, title, goal, status, needs[], owner, done_when, notes, files[], replaces, why, account
```

- `status`: `todo`, `doing`, `done`, `blocked`, `dropped`. The changes allowed are a
  table (`transitions` in `graph.go`), not checks in callers.
- `needs`: ids this node waits on. Adding a cycle is refused. A node is READY when it
  is `todo` and everything it needs is `done`. `dropped` does NOT release a dependent
  (aw4 learned this): it stays waiting, the reason shows on it, and the action is to
  replace the edge. A dependent starting without its prerequisite is worse than one
  waiting.
- Waiting is computed from the edges and never stored. `blocked` is stored and means
  only that someone said "stuck", with a reason (`block`). A node cannot go stale
  when a dependency moves.
- ONE function answers "can this start" (`Graph.CanStart`): the ready set, the tool,
  the screen and the turn's line all read it. aw4's two readings disagreed.
- `done_when`: what done means, set when the node is made (required). `done` takes an
  account of how it was met; without one it is refused. This is what keeps an agent
  directed.
- `drop` keeps the node (with its reason). `replaces` (optional id, of a dropped node)
  lets a new node stand in for it; the dependents are re-pointed by an `update` that
  changes their `needs`.
- `files`: the paths the node expects to change. Nothing enforces it yet; parallel runs
  need it (aw4 had nine landings refused in one night from siblings editing one test
  file). Kept in dun until the package moves.
- `owner`: an opaque string the graph round-trips and `Ready` filters on. "" is the
  session's main agent. Who may change a node is a hook the host gives the tool
  (`WithMayChange`), not a rule of the graph. `Subtree(id)` is the node and all that
  needs it; `Assign` gives a subtree to an owner.

## The tool

One tool, `plan`, in a built-in group `plan` (`dun:plan`), with an `action`:
`add`, `update`, `show`, `ready`, `done`, `block`, `drop`. `delegate` comes later.
`update` also sets `status` to `todo` or `doing` (begin a node, or take back a block).
The package's entry is `Tool(store, actor, opts...)` returning the definition and a
handler, after agentkit's `SessionSearchTool`. The host adds its own sentences to the
description (`WithNote`): plan mode and `files` are the host's to explain.

Every result is short: the node touched and what is ready now.

## Storage

`<session>.plan.json`, beside the session, written atomically, so a plan survives
folds and resume. A session with no file (an in-memory session) keeps it in memory.

## The person

`/plan` opens a screen with the graph as a tree. A node with several parents shows
under each, marked as a repeat. From it a person can approve the plan, or edit, drop
or re-order nodes. What they change reaches the model as a notification.

**Plan mode.** Approval is asked for only in plan mode: `/plan mode on|off`, off by
default, so a small task stays free of it. In plan mode, tool calls that change the
workspace are refused until the plan is approved, with a result saying so.
`/plan approve` approves. Editing an approved plan through the tool keeps it
approved; the person sees the change.

## Directing the agent

While a plan has open nodes, each turn carries a short line: the node in progress and
the next ready ones, at most 5 lines. It goes in as an aside at the tail, so it does
not disturb the cached prefix, and it is read fresh after a fold.

WHEN: once as each turn begins (`prepareTurn`), not per tool result. The model's own
`plan` calls already answer with the node touched and what is ready, and a change the
person makes arrives as its own note. A turn with no open nodes gets none. Cost: at
most 5 short lines (about 100 tokens) a turn.

## Delegation (later)

`plan delegate <node>` spawns a child owning that node and all that hangs from it. The
child sees only its subgraph and can change only that; it may add nodes under it, and
those show to the person. Status flows back; when it finishes, the parent is told and
new nodes are ready. Each node may name a model and effort. Unrelated nodes can run in
parallel with worktree isolation.

## Build order

1. `internal/taskgraph`, the tool, storage, the group. ✅
2. `/plan` in the TUI, plan mode and approval. ◐
3. Delegation.
4. Parallel runs and per-node models.
