# dun

A coding agent that gets things *dun* — a Claude-Code-in-Go that composes iode's
own tools into one agent working inside an isolated workspace:

- **[agentkit](https://github.com/iodesystems/agentkit)** — the engine (tool
  loop, context shaping, proactive RAG, token accounting).
- **poly-lsp-mcp** — semantic code: navigate, read, edit/rename/refactor with
  live diagnostics (gopls/tsserver/pylsp + tree-sitter).
- **mcpshell** — sandboxed compute (`eval`): arithmetic, data wrangling, jailed
  file ops, SQL.
- **[raglit](https://github.com/iodesystems/raglit)** — search your docs/code +
  proactive suggestions.

## Status

**Slice 1 (headless composition) works.** dun spawns the three MCP tool servers,
bridges their tools into an agentkit loop, and works a task against any
OpenAI-compatible endpoint.

```sh
go build -o dun ./cmd/dun
# poly-lsp-mcp, mcpshell, raglit must be on PATH

# interactive Bubble Tea UI
DUN_LLM_KEY=... dun -tui --workspace ./my-project

# one-shot, human-readable
DUN_LLM_KEY=... dun --workspace ./my-project "find the greet function and explain it"

# programmatic: line-delimited JSON events in/out (the TUI is a client of this)
echo '{"type":"user","content":"..."}' | dun -p --workspace ./my-project
```

The engine speaks a small JSON event protocol (`-p`): out `ready`/`token`/
`tool_call`/`tool_result`/`message`/`usage`/`done`/`error` + `ask`/`notification`;
in `{"type":"user",...}` / `{"type":"answer","value":...}` / `{"type":"stop"}`.
The TUI is just a client of it, so the engine stays headless and scriptable.

**Human-in-the-loop:** the agent can call `ask_user{question, options}` when a
decision is yours — the turn pauses, you're asked (a picker in the TUI), and it
resumes with your answer. **Proactive docs:** relevant docs are pushed as 🔔
notifications as the conversation moves (raglit's index watched via agentkit's
FinderPreparer). **Background work:** `exec{background:true}` runs a long command
(the full test suite, a build) asynchronously in the container; when it finishes
the agent is notified and reacts on its own — no blocking.

## Isolation

dun works in an isolated **git worktree** (a fresh `dun/<ts>` branch off HEAD),
so the agent's edits never touch your checked-out branch — review the diff at the
end and turn the branch into a PR. Its `exec` tool (build/test/git) runs on the
host by default, or **contained in a Docker container** with the worktree
mounted and no network:

```sh
dun -tui --workspace ./repo --docker golang:1.26   # exec runs in the container
dun -tui --workspace ./repo                         # exec runs on the host
dun --no-worktree ...                               # work in place (no isolation)
```

The container is the sandbox, so model-authored commands can't reach the host —
no per-action approval prompts, the isolation does the work.

Next: run the MCP servers inside the container too, and a worktree→PR flow. See
`plan/plan.md`.

## Vision

```
dun (host: TUI + agent loop + LLM)
  ├─ git worktree of the repo          → isolated changes
  ├─ Docker container (toolchain)      → safe exec/build/test
  ├─ poly-lsp + mcpshell + raglit      → code · compute · knowledge
  └─ end: review the diff → branch/PR
```
