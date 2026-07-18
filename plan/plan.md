# dun — plan

> Living plan: current state + active work + decisions ONLY. Completed trees →
> done.md (pointer left behind). Deferred → icebox.md. Marks: ◻ todo · ◐ in
> progress · ✅ done · ⏸ parked · ❓ blocked.

## What this is

`dun` — a coding-agent harness (a Claude-Code-in-Go) that composes iode's own
pieces into one agent that works a task inside an isolated workspace:

- **agentkit** — the engine: tool loop, context shaping/compaction, proactive-RAG
  hook (`FinderPreparer`), validation, token accounting. (Owns tablestakes, NOT
  orchestration — dun IS the orchestration.)
- **poly-lsp-mcp** — semantic code: `node_query` (navigate + call-graph),
  `node_read`, `node_edit` (rename/refactor, diagnostics on edit). gopls/tsserver/
  pylsp + tree-sitter.
- **mcpshell** — sandboxed compute: one `eval` tool (arithmetic, data wrangling,
  jailed file ops, SQL). Not a raw shell.
- **raglit** — knowledge: `search`/`ingest` + proactive suggestions (its
  `agent.DocFinder` → agentkit `FinderPreparer`).

Name: sounds like "done" (gets it *dun*); a dun is a workhorse. `github.com/
iodesystems/dun`.

## Architecture

```
dun (host: Bubble Tea TUI + agentkit Session + LLM → corrallm/bonsai)
  ├─ per task: git WORKTREE of the repo (isolated mutable surface)
  ├─ Docker CONTAINER (toolchain + the 3 iode tools), worktree mounted in
  ├─ mcpmgr spawns servers via `docker exec -i`:
  │     poly-lsp-mcp mcp --root /work · mcpshell mcp --files-dir /work · raglit serve
  ├─ exec tool = `docker exec` → build/test/git (safe, contained)
  └─ end: worktree diff → review in TUI → branch/PR
```

The tool composition is ~2 functions (mcpToolDefs + mcpDispatcher over 3 servers).
The NEW work is the TUI, the Docker+worktree lifecycle, the exec tool, and
system-prompt composition.

## Active work

### ✅ Slice 1 — headless composition (PROVEN LIVE)
- `harness.go` — `Start` spawns the 3 servers (`DefaultServers`), `waitForTools`
  polls discovery, builds an `agent.Session` over the merged tools. `Ask` injects
  a task + runs the loop. `defaultSystem` = coding persona + tool guidance.
- `mcp.go` — `mcpToolDefs` + `mcpDispatcher` (route by tool→server, errors→model,
  `onCall` hook for the UI).
- `store.go` — in-memory `agent.Store` (durable persistence is a later slice).
- `cmd/dun` — headless CLI: `dun --workspace DIR "task"`, streams tokens + tool
  calls to the terminal.
- **Verified live vs bonsai:** spawned all 3 (10 tools), `ternary-bonsai-27b`
  DOES tool-call; the agent self-corrected a bad `node_query` selector → read →
  answered. Workspace = a tiny Go pkg; poly-lsp used gopls.

### ◻ Slice 2 — Bubble Tea TUI
- Charm stack (bubbletea/bubbles/lipgloss). Conversation view, streaming
  assistant tokens, tool-call stream, input box, `/` commands. Wrap the `Ask`
  loop in an event model (tokens/tool-calls as msgs). Read-and-propose (diffs
  shown, not auto-applied) until Slice 3's approval.

### ◻ Slice 3 — Docker + git-worktree isolation + exec
- Create a git worktree per task; Docker container with the toolchain + tools,
  worktree mounted; spawn MCP servers via `docker exec -i`; add a gated `exec`
  tool (build/test/git) — the container is the sandbox, so exec/mutations are
  contained (approval = the isolation, not per-action prompts).

### ◻ Slice 4 — persistence + workspace→PR
- Durable session store (resume, history); worktree diff → review → branch → PR.
- Auto-ingest the workspace into raglit on start; wire `FinderPreparer` for
  proactive code/doc pings.

### ◻ Slice 5 — roles / task DAG (if wanted)
- Planner/coder/reviewer; multi-Session orchestration (autowork3-style).

## Decisions
- MVP is a Claude-Code-like **Bubble Tea TUI** (not a one-shot CLI) — but built
  engine-first (Slice 1 headless) then wrapped.
- Safety model = **Docker container + git worktree** isolation, not per-action
  approval prompts.
- The 3 tools are sibling MCP servers bridged into ONE Session (NOT nested inside
  mcpshell's `--mcp` composition — the model should call node_edit/search
  directly).
- LLM: any OpenAI-compatible endpoint; default corrallm/bonsai
  (`ternary-bonsai-27b`, confirmed tool-calling).
- The tools must be on PATH (poly-lsp-mcp, mcpshell, raglit) — Slice 3 moves them
  into the container image.

## The gap it fills
None of the four runs arbitrary commands (build/test/git) — mcpshell is
sandboxed, poly-lsp only gives diagnostics. dun's exec tool (Slice 3) is the
command-runner, made safe by the container.
