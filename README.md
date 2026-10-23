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

dun knows three tool servers, expected on `PATH`: `poly-lsp` (poly-lsp-mcp), `shell`
(mcpshell) and `raglit`.

**Only `shell` starts on its own.** `poly-lsp` and `raglit` are opt-in: each costs real
startup time (poly-lsp-mcp indexes the repo; raglit has to ingest the workspace
before it answers anything), and a machine that lacks one of those binaries
should still get a working dun. Turn them on per session, or once for good:

| command | does |
|---|---|
| `/raglit` · `/poly-lsp` | status: on or off, which switch decided, and the command that changes each |
| `/raglit on` · `/poly-lsp on` | on for this session (kept across a resume) |
| `/raglit off` · `/poly-lsp off` | off for this session |
| `/raglit auto` · `/poly-lsp auto` | on for this project, every session (= `on --project`) |
| `/raglit manual` · `/poly-lsp manual` | off for this project (= `off --project`); it stops now |

**Every server has three switches** — global (`$DUN_HOME/mcp.json`, every
workspace), project (`$DUN_HOME/projects/<id>/dun.local.json`) and session
(the session's `.mcp.json` beside its `.jsonl`). The session's wins if set,
then the project's, then the global one; global is on or off, the other two
can also be unset. The same three scopes hold a server's **mode** —
standalone (dun connects to it; every tool schema rides every request) or
scripted (mcpshell hosts it; the model calls it through `eval`) — and each
tool's on/off. A running session follows a global or project change at its
next turn, unless it set that switch itself.

| command | does |
|---|---|
| `/mcp` | the table: `server · proto · global · project · session · mode · → state`; in the TUI `g`/`p`/`s` cycle a column, `m` the mode (`M` then `g`/`p`/`s` picks its scope) |
| `/mcp on\|off <id> [--global\|--project\|--session]` | a switch (bare: this session) |
| `/mcp unset <id> --project\|--session` | clear one |
| `/mcp mode <id> standalone\|scripted\|unset [--scope]` | where it runs (`via`, `move`, `shell`/`direct` still work) |
| `/mcp toggle <id> <tool> on\|off\|unset [--scope]` | one tool |
| `/mcp add [preset]` | a form; presets: playwright, chrome-devtools, mobile, figma |

A repo's committed `dun.json` `autostart`/`via` is not a fourth scope: it is
the project value while the project's own switch is unset.

**What `/raglit` indexes.** In a git repo, the files git shows (tracked plus
untracked-not-ignored); outside one, a walk that honors `.gitignore`. Then,
automatically skipped: dependency, build and cache directories by name
(`node_modules`, `.venv`, `dist`, `build`, `__pycache__`, `target` beside a
`Cargo.toml`/`pom.xml`, ...), any directory whose files add up to more than
5,000 or 200 MB (skipped whole, at the highest such directory — never the
workspace root), files over 1 MB, and binary files. `/raglit on`, the ready line
and a bare `/raglit` say what was skipped:

    docs: indexing 1,234 files · skipped node_modules/ (dependencies), data/ (12,345 files, 2.3 GB),
    14 files over 1 MB (largest: dump.json 310 MB) — to include one: "rag": {"include": ["data/"]} in dun.json

```jsonc
// dun.json — include overrides every automatic skip; exclude wins over include
"rag": {"include": ["data/", "docs/big.json"], "exclude": ["fixtures/"],
        "max_file_kb": 1024, "max_dir_files": 5000, "max_dir_mb": 200}
```

Patterns are gitignore syntax and add up across layers. Inside a dependency
directory only a pattern naming it or a path in it (`node_modules/foo/`)
reaches; a bare `*.json` does not reopen every `node_modules`.

`--rag` / `--lsp` force one on for a single run (`--rag=false` forces it off),
above whatever is saved. Startup names what is off, so a missing tool family is
never a silent absence. A server that fails to start is reported and skipped —
it costs you that family, not your session.

**dun writes nothing into the repos it works on** — no `.dun/`, no
`.gitignore` entry to ask of anyone. What it keeps per project lives in
`$DUN_HOME/projects/<id>/` (default `~/.dun`):

    $DUN_HOME/projects.json                  absolute repo dir → id (minted on first sight)
    $DUN_HOME/projects/<id>/dun.json         optional, yours to write
    $DUN_HOME/projects/<id>/dun.local.json   the project switches (/raglit auto, /mcp on --project) (0600)
    $DUN_HOME/projects/<id>/agents_read      the AGENTS.md guard's memory
    $DUN_HOME/projects/<id>/worktrees/       --worktree session trees
    $DUN_HOME/projects/<id>/overlays/<dir>/  one linked worktree's own state

The id is the directory's name plus a random suffix. A git worktree (dun's,
`git worktree add`, Claude Code's) belongs to its source repo's project, and
what dun saves while working in it goes to that worktree's overlay, which wins
over the project's. A go.mod `replace => ../x` still resolves inside a session
worktree: a symlink `worktrees/x` points at the real sibling.

A repo MAY commit `dun.json` or `.dun/dun.json` (the PROJECT — "this repo
needs a db tool"); dun reads them and never writes them. Precedence:

    built-in defaults (or Servers set in Go)  <  $DUN_HOME/mcp.json  <  dun.json
      <  .dun/dun.json  <  projects/<id>/dun.json, dun.local.json  <  overlay
      <  the session's switches

(mcp.json is the global scope; everything after it up to the overlay is the
project scope.)

A `dun.local.json` or `.dun/dun.local.json` in the repo is the older layout:
still read (just below the project dir), and copied into the project dir the
first time dun sees the repo. dun says so once; the repo copy can then go.
Session worktrees an older dun made in `<repo>/.dun/worktrees/` keep working
(resume, prune) where they are.

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
make install   # builds the stamped binary into ~/.local/bin/dun (BINDIR=… to change)
# mcpshell must be on PATH; poly-lsp-mcp and raglit only if you use /poly-lsp, /raglit

dun --setup   # Bubble Tea wizard: log in to a provider (an OpenAI-compatible
              # URL, bearer optional) and pick a model. In the TUI, web or
              # app: /login [name], /logout <name>, /model. Saves to
              # ~/.dun/config.json. There is no built-in provider: dun talks
              # to no model until you log in. Precedence: --flag > env > config.

# The stamped binary updates itself from this source tree
# (cmd/dun/selfupdate.go). A plain `go install ./cmd/dun` leaves srcDir empty
# and never updates.
#
# `make install-launcher` instead puts a LAUNCHER script (tools/dun.sh) on
# $GOPATH/bin: on each launch it rebuilds from the source tree if any .go /
# go.mod changed — this repo AND any local `replace` target in go.mod — then
# execs the result. ~55ms when nothing changed. The binary it manages lives in
# ~/.cache/dun (override: DUN_BIN); skip the check with DUN_NO_AUTOBUILD=1.
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
scrollback, and turns your `/raglit` and `/poly-lsp` servers back on. That includes the
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

**A plain `dun` picks up where you left.** Started without `--resume` or
`--continue`, dun asks dund for this workspace's newest session that no client
(another terminal, a browser tab) is attached to, and resumes it — so sessions
opened and left do not pile up. The first line says so: `resumed <id> (5m ago,
4 messages) — /new for a fresh session`. Only a session started in the same
directory with the same flags counts: one under another worktree or with
`--docker` is another workspace. dund makes the pick, so two terminals started at
once never land in the same session. `/new` (or `dun --new`) starts a fresh one.

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

**A changed tool set announces itself, for free.** Turning raglit or poly-lsp on or off
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
FinderPreparer). **Background work:** an `exec` that is still running after 30s hands itself over
to a background job (the full test suite, a long build); when it finishes the
agent is notified and reacts on its own — no blocking, nothing is killed. Their output
streams to a log file whose path the agent gets (so a big log is grepped, not
pasted into the context), and `exec_monitor` tunes what a running job reports —
`buffer_bytes` to hear from it periodically, `grep` to hear only matching lines,
`ignore` to mute it, `kill` to end it and everything it started. A job says
nothing until it exits unless asked to. You stop one with `/jobs kill <n>` (bare
`/jobs` lists them, `/jobs kill all` ends every one after a y/n) or `x` on its row
in the activity pane. An interrupt does not stop the turn's jobs; it lists the
ones still running, with the kill command.

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
dun --workspace ./repo --docker=true             # entire process in bundled docker image
dun --workspace ./repo --docker golang:1.26      # entire process in a specific image
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

### File tools

`file_read`, `file_edit`, `file_write`, `file_grep` and `file_list` are built
in (agentkit's `files` package), rooted at the session's working tree — the
worktree when there is one. A path that climbs out, by `..`, an absolute path
or a symlink, is refused. Reads are line-numbered and capped (8KB with no
range, 64KB with one, and the result says how to page); edits replace text that
must match exactly once and return the diff; grep respects `.gitignore` and uses
`rg` when it is on PATH. `file_delete` and `file_move` exist but are off unless
listed:

```jsonc
// dun.json
"files": {"tools": ["read", "grep", "list"], "ignore": ["testdata/golden/"]}
```

`"tools": []` turns them off; `"read_only": true` keeps read, grep and list.

## Vision

```
dun (host: TUI + agent loop + LLM)
  ├─ git worktree of the repo          → isolated changes
  ├─ Docker container (toolchain)      → safe exec/build/test
  ├─ poly-lsp + mcpshell + raglit      → code · compute · knowledge
  └─ end: review the diff → branch/PR
```
