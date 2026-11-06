# The task plan

USER 2026-11-02: a plan system, a DAG of tasks a person can review, so agents are
more directed while talking with the user, and the main agent can delegate subgraphs
to sub-agents. A dun-internal tool group, not a separate server.

The worklog (`/worklog`) records what happened and is person-driven. The plan is
what is meant to happen, and the model keeps it. They stay separate.

## Layers

| Layer | Where | Holds |
|---|---|---|
| Graph | agentkit `taskgraph` | Nodes, `needs`, cycle check, ready set, status rules, subgraph ownership. Stdlib only, no `Harness` |
| Tool | agentkit `taskgraph` | The `plan` tool's definition and handler, over a `Store` interface |
| Host | root `dun` | The store (`planMemStore`), the turn line, the tool group, routing changes, delegation |
| Screen | `internal/tui` | `/plan` |

The graph started here (098c15b) and moved to agentkit in v0.22.0 (Carl, 2026-11-03),
as an optional sibling like `files` and `loopdetector`. It imports stdlib and
agentkit's `llm` only, and knows nothing of dun: no plan mode, no approval, no
sub-agent numbering. agentkit ships no store; the host provides one (one per plan,
shared by every session that works it). Delegation and scheduling stay in the host.

**Telling owners.** Every write the host makes goes through `taskgraph.Apply`, which
returns the changes and, for each, the owners to tell (never the actor). The tool
does the same through `WithOnChange`. `planTell` routes them: owner "" is the main
agent, told by an aside. The person is actor `user`, so their edit reaches every
owner it touches; the model's own change reaches no one. Until delegation gives a
subtree another owner, "" is the only one; delegation adds the delegator, which the
graph does not record.

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
under each, marked as a repeat ("again"). From it a person can approve the plan, edit
a node's title or goal, or drop a node. Re-ordering is not built: order is the edges,
and the screen does not edit them (not built). `e`, `g` and `d` fill the composer with
`/plan edit|drop …` to finish, so a drop is typed, not a keystroke. What the person
changes reaches the model as a note: an approval as a notification that wakes it,
the rest as asides that ride its next turn. The engine sends the graph as a `plan`
event (control `plan` `view`, then again on every change by anyone); the screen
never works out what can start.

**Plan mode.** Approval is asked for only in plan mode: `/plan mode on|off`, off by
default, so a small task stays free of it. In plan mode, tool calls that change the
workspace are refused until the plan is approved, with a result saying so.
`/plan approve` approves (an empty plan has nothing to approve). Editing an approved
plan through the tool keeps it approved; the person sees the change. Turning the mode
on again takes the approval back. Mode and approval are the host's, kept in the
sidecar beside the graph; the gate is a dispatcher wrapper (`withPlanGate`).

Refused: `exec` (a command cannot be told read-only), `file_edit`, `file_write`,
`file_delete`, `file_move`, `agent` (a child has all of them), `mount`, `loop_ctl`
(schedules runs of its own), `dun_msg` (acts in another session), every tool of the
shell server, and an MCP tool whose name has a write verb (write, edit, delete,
rename, create, apply, commit, push, run, …: a guess, since MCP tools do not say
whether they are read-only). Allowed: `file_read`, `file_grep`, `file_list`, `plan`,
`ask_user`, `session`, `read_saved`, `recap`, `skill`, `memory`, `jobs`,
`tell_parent`, `dun_list`, and other MCP tools.

## Directing the agent

While a plan has open nodes, each turn carries a short line: the node in progress and
the next ready ones, at most 5 lines. It goes in as an aside at the tail, so it does
not disturb the cached prefix, and it is read fresh after a fold.

WHEN: once as each turn begins (`prepareTurn`), not per tool result. The model's own
`plan` calls already answer with the node touched and what is ready, and a change the
person makes arrives as its own note. A turn with no open nodes gets none. Cost: at
most 5 short lines (about 100 tokens) a turn.

## A child owns a node (2026-11-05)

The light link, before full delegation. `agent({prompt, node})` gives the child the
node and everything that needs it (`Graph.Assign`, owner `agent:N`) BEFORE the child
starts, so its tool set has a plan tool: the parent's graph, as actor `agent:N`, with
`WithMayChange` allowing only that owner's nodes (`childPlanTool`). Its changes reach
the parent's screen at once and the main agent when they touch its nodes. Owned nodes
leave the main agent's ready list (`Ready("")`). Dismissing the child gives its
unfinished nodes back to owner ""; finished ones keep who did them.

`/plan tell <id> <text>` hands the person's words about a node to its owner, to
change the plan to match, instead of a direct edit (USER: "Decide x" nodes want
"it's this way"). The main agent hears it as a notification; a running child inside
its turn (`Say`); an idle one as its next run (`AgentTell`).

## Delegation (later)

`plan delegate <node>` spawns a child owning that node and all that hangs from it. The
child sees only its subgraph and can change only that; it may add nodes under it, and
those show to the person. Status flows back; when it finishes, the parent is told and
new nodes are ready. Each node may name a model and effort. Unrelated nodes can run in
parallel with worktree isolation.

## Build order

1. The graph, the tool, storage, the group. ✅ (graph and tool moved to agentkit v0.22.0)
2. `/plan` in the TUI, plan mode and approval. ✅
3. Delegation. ◐ a child owns a node and changes it (above); `plan delegate` is not built.
4. Parallel runs and per-node models.
