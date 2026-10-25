# dun: design

dun is a coding-agent harness in Go. It works a task inside an isolated
workspace, using tools that iode builds as separate programs. The name sounds
like "done"; a dun is also a workhorse. Module: `github.com/iodesystems/dun`.

Other docs: `README.md` (use), `decisions.md` (why the code is the way it is),
`ui-decisions.md` (TUI rules), `surfacing.md` (proactive retrieval),
`event-protocol.md` (engine wire format), `mid-turn-messages.md`,
`tui-architecture.md`, `node_query.md` (tool cheat sheet),
`sessions-and-dund.md`, `providers-and-cost.md`, `mcp-and-tools.md`,
`memory.md`.

## What dun composes

- **agentkit** (`github.com/iodesystems/agentkit`): the engine. Tool loop,
  context shaping and compaction, the proactive-retrieval hook, validation,
  token accounting. It owns table stakes, not orchestration. dun is the
  orchestration.
- **poly-lsp-mcp**: semantic code. `node_query` (navigate, call graph),
  `node_read`, `node_edit` (rename and refactor, diagnostics on edit). Uses
  gopls, tsserver, pylsp and tree-sitter.
- **mcpshell**: sandboxed compute. One `eval` tool: arithmetic, data
  wrangling, jailed file ops, SQL. It is not a raw shell.
- **raglit**: knowledge. `search` and `ingest`, plus proactive suggestions.

dun adds what none of them has: the TUI, the session service, the worktree and
Docker lifecycle, the `exec` and `ship` tools, sub-agents, and system-prompt
composition. The tool composition itself is small: `mcpToolDefs` and
`mcpDispatcher` (`mcp.go`) merge the servers' tools into one agentkit session.

## Architecture

```
clients: TUI (cmd/dun) · browser · phone (mobile/)
   │  attach over a unix socket / HTTP
   ▼
dund — session service (internal/dund). Owns sessions; they outlive clients.
   │  stdio, line-delimited JSON (docs/event-protocol.md)
   ▼
engine: `dun -p` — one process per session (harness.go)
   ├─ agentkit Session + LLM client (any OpenAI-compatible endpoint)
   ├─ git worktree of the repo (isolated mutable surface; opt out: --no-worktree)
   ├─ mcpmgr spawns tool servers on the host over stdio JSON-RPC:
   │     poly-lsp-mcp · mcpshell (--files-dir) · raglit serve · user-added
   ├─ exec tool: on the host, or in a Docker container when docker is on
   └─ end: ship (fetch, rebase, checks, push) → branch/PR
```

- The engine is headless. The TUI is a client of its event protocol, so the UI
  is presentation only and any other client can drive the same engine.
- dund runs each engine as a child process, numbers and stores its events, and
  lets any number of clients attach, send commands and follow live. A session
  belongs to the principal that created it. dund also routes dun-to-dun
  messages (`dun_list`, `dun_msg`). `dun -d` (the launcher, `cmd/dun/launcher.go`)
  is a separate supervisor for source rebuild and hot reload.
- Tool servers are sibling MCP servers bridged into one session. They are not
  nested inside mcpshell's `--mcp` composition, because the model should call
  `node_edit` and `search` directly. A server can instead be hosted inside
  mcpshell ("scripted" mode) to save context; see `decisions.md`.
- Servers are declarative: built-in defaults, then `dun.json` (committed,
  describes the project), then `dun.local.json` (gitignored, describes this
  machine), then Go-set values. The extension manifest (`extension.go`) says
  how a server plays a role (search, index) and which slash commands it adds.
- Per-workspace state lives in `.dun/` (worktrees, local config).
  Per-user state lives in `$DUN_HOME` (default `~/.dun`): config, sessions,
  spend ledger, bug-report captures.
- Safety model: isolation, not per-action approval prompts. The worktree
  contains file changes to a branch. A Docker container contains `exec`. The
  agent lands work only through `ship`, whose allowed modes the repo declares.

## The gap it fills

None of the three composed tools runs arbitrary commands. mcpshell is
sandboxed, and poly-lsp gives diagnostics only. A coding agent must build,
test and use git. dun's `exec` tool is that command runner. Docker and the
worktree make it safe to give the model. `ship` makes landing the work
verified: nothing is pushed that was not checked in the state it will land in.
