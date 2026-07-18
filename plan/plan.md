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
- `cmd/dun` — CLI, two modes:
  - human: `dun --workspace DIR "task"` streams tokens + tool calls to the terminal.
  - **`-p` programmatic:** line-delimited JSON events — OUT `ready`/`token`/
    `tool_call`/`tool_result`/`message`/`usage`/`done`/`error`; IN
    `{"type":"user","content":...}` / `{"type":"stop"}`. This is the engine
    PROTOCOL and the decoupling seam: the Slice-2 TUI is a CLIENT of it.
- **Verified live vs bonsai** (both modes): spawned all 3 (10 tools),
  `ternary-bonsai-27b` DOES tool-call; the agent self-corrected a bad
  `node_query` selector → read → answered; `-p` took a stdin user event and
  emitted the full event stream.

### ✅ Slice 2 — Bubble Tea TUI (client of the `-p` protocol)
- `cmd/dun/tui.go` — `dun -tui` re-execs `dun -p` (forwarding --workspace/model/
  url/key), reads its JSON event stream via a goroutine→channel→`tea.Msg`, and
  writes `user` events to its stdin. Renders: header (workspace), scrollable
  viewport (conversation + live-streaming tokens + tool-call/⚙ lines), input
  box, status spinner (spawning… / working… / ready). Charm stack
  (bubbletea/bubbles/lipgloss). `-tui` flag wired in main.go.
- **Engine stays headless** — the UI is pure presentation over the protocol.
- `cur` is a plain string, NOT strings.Builder (Bubble Tea copies the model each
  Update; a copied Builder panics — caught pre-flight).
- Tests: `tui_test.go` drives the event logic headless (ready→token→tool_call→
  done builds the convo + clears flags; error clears busy). Full TUI rendering
  needs a real terminal (no-TTY exits cleanly, no panic).
- **◻ next in this slice:** `/` commands, diff rendering for edits, key nav/history.

### ✅ Slice 3 — worktree isolation + exec tool
- `worktree.go` — `NewWorktree(repo)` creates a `git worktree add -b dun/<ts>`
  off HEAD (isolates file changes to a branch; `main` untouched). `Diff()`,
  `Cleanup()` (keeps the branch so work isn't lost); pass-through when not a git
  repo.
- `exec.go` — `ExecBackend`: `HostExec` (host, trusted/throwaway) and
  `DockerExec` (`docker run --rm -v wt:/work -w /work --network none IMAGE …` —
  the container IS the sandbox, model-authored commands can't touch the host).
  `execToolDef` + `withExec` (route "exec" locally, everything else to MCP).
- `harness.go` — `Config.Exec` adds the exec tool + composes the dispatcher.
  System prompt tells the agent to verify edits with build/test via exec.
- cmd: `--docker IMAGE` (else host), `--no-worktree`; creates the worktree,
  reports branch + final diff; emits a `workspace` event in -p; TUI shows `⎇ branch`.
- **Verified:** worktree isolation (edit doesn't leak to main checkout) + host
  exec (unit); DockerExec plumbing (mounted worktree, `--network none`); LIVE —
  agent ran `exec(ls/git status/git branch)` on the dun/… branch, 11 tools.
- **Model = isolation, not prompts** (per user): the container/worktree contain
  exec + mutations, so no per-action approval gate.
- **◻ deferred to 3b/4:** run the MCP servers themselves INSIDE the container
  (`docker exec -i`) so poly-lsp/mcpshell also see the contained FS; worktree→
  commit→PR.

### ✅ Slice 4a — human-in-the-loop + proactive notifications
- **ask_user** (`ask.go`): the agent calls `ask_user{question, options}`; the
  turn PAUSES at that tool call until answered — `-p` emits an `ask` event, a
  UI picker / terminal prompt collects the answer, it's returned as the tool
  result and the turn resumes. `Config.Ask` + `withAsk` dispatcher wrapper.
- **Proactive notifications** (`notify.go`): `docsFinder` wraps raglit's search
  tool as an `agent.DocFinder` (ragnotify.MCPFinder); `Session.Preparer =
  FinderPreparer` pings relevant docs before each turn. Injected
  KindNotification → `store.onNotify` → `notification` event. MinScore 0 (raglit
  BM25 scores aren't normalized; a MATCH only returns hits, MaxHits caps).
- **Workspace auto-index:** dun lexically ingests the workspace into raglit at
  startup so search + proactive pings have content.
- **-p protocol grew:** OUT `ask`/`notification`; IN `{"type":"answer","value"}`.
  `runProgrammatic` restructured — a stdin goroutine routes user/stop→turns,
  answer→the paused Ask (so an ask mid-turn can be answered). TUI renders ❓ ask
  pickers (number picks an option) + 🔔 notifications.
- **Verified live:** ask_user round-trip (agent paused → answered MIT → resumed);
  proactive 🔔 fired on a workspace README match (watching the worktree). Unit:
  onNotify fires only for notifications.
- **✅ background exec → notification convergence:** `exec{background:true}`
  runs async via the SAME backend (the Docker container when --docker), returns
  "started job #N" immediately; on completion `Harness.startBackground` injects a
  KindNotification and signals `Wake()`. The driver runs a `Continue` turn (no
  new user message) so the agent reacts autonomously — the agentkit converge
  pattern. `-p` loop + human path drain wakes; `Harness.Notify/Wake/Continue/
  BackgroundRunning`. **Verified live** with `--docker alpine`: bg job in the
  container → 🔔 → autonomous turn where the agent acknowledged TESTS_PASSED.
- **◻ deferred:** TUI markdown (glamour) + diff view.

### ◻ Slice 4b — persistence + workspace→PR
- Durable session store (resume, history); worktree diff → review → branch → PR.

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
