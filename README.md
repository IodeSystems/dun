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

# interactive Bubble Tea UI — the DEFAULT, no flag needed
DUN_LLM_KEY=... dun --workspace ./my-project

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

**A dead turn is not a dead session, and a dead engine is not a dead TUI.**
`--timeout` (default 30m) bounds a *turn* in an interactive session and the
*whole run* one-shot. It is a pausable clock, not a deadline: time you spend
answering `ask_user` is not dun working and is not charged to it, so a question
left open never kills the turn that asked it. A turn that hangs is cut off; the
engine stays up and the next message starts a fresh turn.

If the engine dies anyway — crash, OOM, kill, or a config it refuses to start
with — the TUI restarts it, reattaches to the same session id, keeps your
scrollback, and turns your `/rag` and `/lsp` servers back on. That includes the
*first* engine: a TUI that will not open is a TUI that cannot show you the
error, so it comes up regardless and retries from inside. Three attempts per two
minutes, then it stops and waits for `/reconnect` (`dun --continue` still has
everything). An engine that *chose* to exit (ctrl-C, `/quit`) says so and is
left alone.

**Context shaping is off unless you say otherwise, and never silent.** Set
`DUN_CONTEXT_TOKENS` to your model's window to enable it (dun shapes to 90% of
it: oversized old tool results become stubs first, and only then is the oldest
history folded into a summary). **Unset means no shaping at all** — nothing can
tell a 32k window from a 180k one without minutes of multi-megabyte probing, and
guessing low compacts a large window for nothing.

Every fold is reported — `🗜 compacted 12 entries: 30000 → 8000 tokens (saved
22000)` — in the TUI, on stderr, and as a `compaction` event on `-p`. More than
one fold in a single turn is called out as thrashing, because that means the
budget cannot fit one turn's floor (system prompt + tool schemas + the pristine
tail) and every turn will re-summarize what it just summarized.

**The UI does not compete with your keyboard.** The worst offender was not a
loop at all: glamour's auto-style asks the terminal for its background colour
and waits up to **five seconds** for a reply, tmux frequently never answers, and
dun rebuilt the renderer on every `WindowSizeMsg` — so every resize froze the UI
for 5s with keystrokes queued behind it. The style is now resolved once, before
the event loop starts, from `COLORFGBG` or a single query, and skipped entirely
inside a multiplexer (`DUN_MD_STYLE=dark|light` overrides, `DUN_MD_QUERY=1`
forces the round trip).

 Beyond that: redraws are capped at 30Hz while text
streams in, each conversation block caches its wrapped render, and the viewport's
own render is memoized on (content, offset, size) — Bubble Tea calls `View()`
after *every* message, so without that last one a token still cost 353µs of
re-slicing whatever the pacing did.

| | before | after |
|---|---|---|
| a resize (silent terminal) | 5.0 s | 0.3 ms |
| `View()`, 200 blocks | 353 µs | 23 µs |
| one 100-token reply | 973 ms | 4.8 ms |
| per token | 9.7 ms | 1.7 µs |

`/perf` reports per-stage timings (update / view / refresh, with p50/p95/max) and
names the message type behind the slowest frame. dun also notices its own
stutter: past a tenth of frames over 16ms it says so once and writes a dump.
`DUN_PPROF=127.0.0.1:6060` serves `net/http/pprof` from any mode.

**Switch sessions without leaving.** `/resume` opens a picker of every saved
session for the workspace — newest first, each with its age, entry count and
**opening message**, because an id is a timestamp and a timestamp says when but
never which. `/resume <id>` goes straight there. Switching restarts the engine on
that conversation and replays its transcript; the scrollback you were looking at
is cleared, since it belongs to the session you just left.

**Record a session, replay it exactly.** `DUN_TRACE=t.jsonl dun` writes every
engine event with its offset from the first one; `dun --replay t.jsonl` feeds
them back into the real TUI at the original pacing — minus the dead air. Only
the *short* gaps carry load: tokens 25ms apart are what stutters, while the 90
seconds you spent reading the last reply reproduce nothing but 90 seconds. Gaps
over `--max-gap` (default 2s) collapse to it and every burst stays verbatim, so
a 4m33s session replays in 9s and still reproduces the load:

    replayed 128 events over 4m33s · 3 idle gap(s) compressed, replayed in 9s

The replay always says what it did to the recording — a replay that silently
rewrites time is not evidence. `--max-gap 0` replays the real wall clock,
`--replay-speed 4` scales proportionally, `--input-delay 0` fast-forwards (CI)
and `--input-delay 5ms` imposes a fixed gap (hunting a load ceiling). No LLM, no subprocess, the same events in the same
order with the same gaps.

This exists because every performance number above had to be inferred from
benchmarks written after the fact — a 5s stall was found by attributing frames
to message types, not by reproducing it. `dun --replay t.jsonl` then `/perf` is
a measurement anyone can repeat. It is also the only honest way to test the UI
against a *real* session rather than one someone imagined.

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
the agent is notified and reacts on its own — no blocking. A foreground `exec` is
killed after 5 minutes, because a command waiting on input it can never receive
is indistinguishable from slow work; background jobs have no limit. Their output
streams to a log file whose path the agent gets (so a big log is grepped, not
pasted into the context), and `exec_monitor` tunes what a running job reports —
`buffer_bytes` to hear from it periodically, `grep` to hear only matching lines,
`ignore` to mute it. A job says nothing until it exits unless asked to.

**Sub-agents:** `agent{prompt}` spawns a child agent to spend ITS context instead
of yours — read a huge log, fetch and digest a page, survey how something is used
— and hand back the conclusion. A child works in the parent's worktree and shares
its tool servers (no second index), runs on `--child-model` if you set one, and
cannot spawn agents of its own or reach you directly. It reports with
`tell_parent` (a `status` that overwrites, a `message` that accumulates, a
`final` answer) and can ask the parent — never the human — with `ask_parent`.
Children stay resident after answering, so `agent_monitor` can tell one a
follow-up far more cheaply than spawning another; it also peeks, dismisses, and
restarts children left stopped by a previous session. The TUI lists them live
with what each has spent.

## Isolation

dun works in an isolated **git worktree** (a fresh `dun/<ts>` branch off HEAD),
so the agent's edits never touch your checked-out branch — review the diff at the
end and turn the branch into a PR. Its `exec` tool (build/test/git) runs on the
host by default, or **contained in a Docker container** with the worktree
mounted and no network:

```sh
dun --workspace ./repo --docker=true             # exec runs in ubuntu:24.04
dun --workspace ./repo --docker golang:1.26      # exec runs in a specific image
dun --workspace ./repo                        # exec runs on the host
dun --no-worktree ...                               # work in place (no isolation)
```

The container is the sandbox, so model-authored commands can't reach the host —
no per-action approval prompts, the isolation does the work.

The agent gets one way to land work — a `ship` tool that fetches origin, rebases
onto the base branch, runs the project's checks, and *then* pushes. The order is
the point: what gets verified is exactly what gets pushed. `--pr` is shorthand
for `pr` mode, which pushes and then reports the `gh pr create` line for you to
run. dun does not invoke `gh` itself — an expired gh token does not fail, it
silently starts an OAuth device flow and polls for a code no one is reading,
which is a hang, not an error.

Ship is ON by default, because an agent that finishes work and cannot land it
has done half a job. What a repo is willing to allow is declared in `ship.allow`
below, which is the real policy surface; `--no-ship` withholds the tool entirely,
for a session that should only ever look.

Modes are the terminal state — `verify` (checks only, pushes nothing), `push`,
`pr` — and a repo declares which ones are allowed:

```jsonc
// dun.json
"ship": {
  "allow":   ["verify", "pr"],       // this repo's agents land branches for review, they don't push the base
  "default": "pr",
  "checks": [                        // serial waves; each object runs in parallel
    {"compile": "go build ./..."},
    {"lint": "golangci-lint run", "vet": "go vet ./..."},
    {"smoke": "go test -short ./..."}
  ]
}
```

Ship requires a clean tree (an untracked file is usually one the agent forgot to
add), resumes a conflicted rebase on its own, and re-verifies if origin moved
while the checks were running.

It ships the branch you are ON, against that branch's own upstream — dun never
switches branches. A branch with no upstream has nothing to verify against, so
it falls back to the base branch, and with no remote for that either the checks
are the whole of ship. What an agent may do is declared with `allow`, not by
guessing from branch names.

Next: run the MCP servers inside the container too. See `plan/plan.md`.

## Vision

```
dun (host: TUI + agent loop + LLM)
  ├─ git worktree of the repo          → isolated changes
  ├─ Docker container (toolchain)      → safe exec/build/test
  ├─ poly-lsp + mcpshell + raglit      → code · compute · knowledge
  └─ end: review the diff → branch/PR
```
