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

## MCP servers

dun knows three tool servers, expected on `PATH`: `code` (poly-lsp-mcp), `shell`
(mcpshell) and `docs` (raglit).

**Only `shell` starts on its own.** `code` and `docs` are opt-in: each costs real
startup time (poly-lsp-mcp indexes the repo; raglit has to ingest the workspace
before it answers anything), and a machine that lacks one of those binaries
should still get a working dun. Turn them on per session, or once for good:

| command | does |
|---|---|
| `/rag` · `/lsp` | status: running? autostart? and the command that changes each |
| `/rag on` · `/lsp on` | start it now, this session |
| `/rag off` · `/lsp off` | stop it now |
| `/rag auto` · `/lsp auto` | start it now **and** every future session (saved) |
| `/rag manual` · `/lsp manual` | stop starting it automatically (leaves it running) |

`--rag` / `--lsp` force one on for a single run (`--rag=false` forces it off),
above whatever is saved. Startup names what is off, so a missing tool family is
never a silent absence. A server that fails to start is reported and skipped —
it costs you that family, not your session.

Four optional config files, layered:

| file | committed? | describes |
|---|---|---|
| `dun.json`, `.dun/dun.json` | yes | the PROJECT — "this repo needs a db tool" |
| `dun.local.json`, `.dun/dun.local.json` | **no** | THIS MACHINE — binary paths, DSNs, anything secret |

Precedence extends what dun already documents for LLM settings:

    built-in defaults  <  dun.json  <  .dun/dun.json  <  dun.local.json  <  .dun/dun.local.json  <  Servers set in Go

`/rag auto` writes `.dun/dun.local.json` (0600, with a `.gitignore` beside it).
The root-level pair is the older layout and is still read.

Servers merge **by id**, and omitted fields inherit — overriding one binary path
does not mean restating its args, and adding a fourth server does not mean
re-listing the first three. Set `"disabled": true` to drop one, `"autostart":
true` to spawn one at startup. `{{workspace}}` and `{{raglit_home}}` are
substituted at spawn.

See `dun.example.json`.

## Status

**Slice 1 (headless composition) works.** dun spawns the three MCP tool servers,
bridges their tools into an agentkit loop, and works a task against any
OpenAI-compatible endpoint.

```sh
make install   # puts a self-rebuilding LAUNCHER on PATH ($GOPATH/bin/dun)
# mcpshell must be on PATH; poly-lsp-mcp and raglit only if you use /lsp, /rag

dun --setup   # Bubble Tea wizard: LLM url / masked key / model (navigable list
              # of the endpoint's models). Saves to ~/.dun/config.json; re-run
              # any time. Precedence: --flag > env > config > built-in default.

# `dun` on PATH is a LAUNCHER script (tools/dun.sh), not the binary: on each
# launch it rebuilds from the source tree if any .go / go.mod changed — this
# repo AND any local `replace` target in go.mod, so editing agentkit counts —
# then execs the result. stdin, the tty, signals, argv and the exit code pass
# straight through. ~55ms when nothing changed. The binary it manages lives in
# ~/.cache/dun (override: DUN_BIN); skip the check with DUN_NO_AUTOBUILD=1.
#
# Why a script: a binary on PATH is only as fresh as the last reinstall. The
# in-binary updater (cmd/dun/selfupdate.go) still works for a stamped build —
# `make install-bin` — but a plain `go install ./cmd/dun` leaves srcDir empty
# and then silently never updates again.
# Plain build without install:  go build -o dun ./cmd/dun

# interactive Bubble Tea UI
DUN_LLM_KEY=... dun -tui --workspace ./my-project

# one-shot, human-readable
DUN_LLM_KEY=... dun --workspace ./my-project "find the greet function and explain it"

# programmatic: line-delimited JSON events in/out (the TUI is a client of this)
echo '{"type":"user","content":"..."}' | dun -p --workspace ./my-project
```

The engine speaks a small JSON event protocol (`-p`): out `ready`/`token`/
`tool_call`/`tool_result`/`message`/`usage`/`done`/`error` + `ask`/`notification`/
`retry`/`queued`; in `{"type":"user",...}` / `{"type":"answer","value":...}` /
`{"type":"stop"}`. The TUI is just a client of it, so the engine stays headless
and scriptable.

**Waiting on the provider:** a 429, a 5xx, or a gateway that isn't answering is
retried with exponential backoff, and every wait is reported as a `retry` event —
attempt, delay, elapsed/budget, and (from a fair-share proxy like corrallm) the
queue itself: slots busy, requests ahead, and why. The TUI shows a live banner
that counts down instead of a frozen cursor. A stream that dies MID-generation is
retried at the TURN level, resuming from the conversation on disk so completed
tool calls are not redone; the half-streamed reply is discarded. If it eventually
gives up, the session is still intact — sending another message pairs off whatever
was interrupted and picks up from there. The policy is the operator's, not
hardcoded: `DUN_RETRY_BUDGET` (negative = unbounded, right for a single-slot local
endpoint) and `DUN_RETRY_5XX_ATTEMPTS` set the per-request policy;
`DUN_TURN_RETRY_BUDGET` bounds the turn-level retry (`0` disables it) and
otherwise inherits the per-request budget.

**Talking mid-turn:** you don't have to wait for the agent to finish. A message
sent while it's working is buffered and lifted into the next tool result, so it
lands *inside* the running turn — no extra round-trip — and several batch in
order.

**A changed tool set announces itself, for free.** Turning rag or lsp on or off
mid-session buffers an *aside* — which tools appeared, which are gone. The
schemas already travel in every request; what the model cannot see there is that
they CHANGED, and one that reasoned earlier about not having `search` will keep
acting on that. The aside rides the next tool result, or joins the next turn
that was going to run anyway. It never schedules one of its own: an LLM
round-trip whose only output is "ok, noted" is not worth buying.

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

With `--pr`, the agent can **submit its work as a pull request** — it commits the
worktree branch, pushes it, and runs `gh pr create` (an `open_pr` tool it calls
when the task is done and verified). Without `--pr`, the changes just stay on the
branch for you to review and PR yourself.

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
