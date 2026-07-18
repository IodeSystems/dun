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
`tool_call`/`tool_result`/`message`/`usage`/`done`/`error`, in
`{"type":"user",...}`/`{"type":"stop"}`. The TUI is just a client of it, so the
engine stays headless and scriptable.

Next: Docker-container + git-worktree isolation (the container is the sandbox,
so the agent can build/test/edit safely) + a gated exec tool. See `plan/plan.md`.

## Vision

```
dun (host: TUI + agent loop + LLM)
  ├─ git worktree of the repo          → isolated changes
  ├─ Docker container (toolchain)      → safe exec/build/test
  ├─ poly-lsp + mcpshell + raglit      → code · compute · knowledge
  └─ end: review the diff → branch/PR
```
