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

### ✅ Tooling — version stamp + dev self-update
- **Problem:** `dun` re-execs itself (`os.Executable()`), so a stale on-PATH
  binary makes the WHOLE tree stale — easy to forget to reinstall.
- **`make install`** stamps `main.version` (git describe) + `main.srcDir` (module
  dir). `dun -version` and the TUI header show the stamp.
- **Self-update (`cmd/dun/selfupdate.go`):** a source-stamped build, on launch,
  compares source mtimes vs its own; if newer, rebuilds itself in place
  (`go build -o <exe>`) and re-execs the fresh binary. Guards: `srcDir==""`
  (release build) / `DUN_CHILD` (spawned -p/-tui children, env-tagged at spawn) /
  `DUN_AUTOBUILD_DONE` (post-rebuild re-exec) / `DUN_NO_AUTOBUILD=1` all skip; a
  failed rebuild is non-fatal (warn, run current). Verified: edit→rebuild+reexec,
  fresh→silent, each guard skips. Tests: buildInput, sourceNewerThan.

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
- **✅ pane focus + selection (tmux-style):** Tab toggles focus between the
  input and conversation panes; the focused pane wears a bright rounded border
  (the "half-edge" cue). In convo focus ↑/↓ move a message selection (left
  gutter highlight, viewport follows). The ask picker is the lower pane while
  answering: ↑/↓ choose an option, `n` attaches a detail, a trailing
  "✎ custom / chat…" row opens free-text — replacing the old type-a-number
  prompt. Unit-tested (focus toggle, selection clamp, option+note, custom).
- **✅ vim `/` search:** in convo focus, `/` opens a query line; matches drive
  the selection, ↑/↓ step between hits, esc exits (unit-tested).
- **✅ collapsible tool output:** call+result fold into one block (▸/▾); the
  collapsed preview is the glance.
- **✅ tool inspector overlay (`inspector.go`):** enter on a tool block opens a
  full-screen overlay — two bordered, focusable sub-frames (input / output),
  each independently scrollable, with `less`-style search: `/` forward, `?`
  backward, `n`/`N` repeat/reverse, `g`/`G` ends, tab switches frame, esc/q
  closes. Content pre-wrapped to the frame width (no clip); current match
  highlighted bright, others dim; footer shows `match a/b`. Fed by a `toolBlock`
  (name + raw input via `argFull` + complete output body) attached to the convo
  entry. This is the human drill-in counterpart to agentkit's `{OUTPUT}` (same
  complete bytes; agentkit surfaces them to the model's reply, the inspector to
  the user). Unit-tested: open, tab-focus, `/` search + `n` cycle, view render.
- **✅ relevant-docs notifications (aggregated + nested nav):** dun's OWN
  aggregating preparer (`docsPreparer`, replacing agentkit's per-hit
  `FinderPreparer`) emits ONE summary per pass — "found = candidate hits,
  surfaced = top-MaxHits injected into the prompt". The `-p` notification event
  gains `kind:"docs"` + `found`/`surfaced`/`docs[]`; the store routes docs
  notifications to `Config.OnDocs` (skipping the plain onNotify). TUI renders a
  one-line 🔎 summary, expand on enter, → descends into the doc list (↑/↓ move,
  enter expands a doc's snippet, ←/esc ascends). Verified live: a WIDGETS.md
  match emitted `found:2 surfaced:2` with per-doc title/line/score.
- **✅ tall-message scroll:** when a selected message exceeds the viewport, ↑/↓
  scroll WITHIN it, stepping to the next message only at its edge; `scrollToSel`
  leaves a taller-than-window selection alone while any part is visible.
- **✅ mouse wheel:** `WithMouseCellMotion` so tmux/terminals forward wheel
  events to the viewport instead of scrolling their own scrollback.
- **✅ tool-result renderers (compiled-in + Starlark):** `ToolRenderer`
  registry keyed by tool name — `(tool, args, result) → (preview, full)` folded
  by the ▸/▾ block; unknown tools use a diff-aware generic. Built-ins:
  node_edit→diff+stat, search/node_query→pretty-JSON. Runtime layer: Starlark
  scripts in `$DUN_HOME/renderers/*.star` register over the SAME registry
  (override built-ins, last-write-wins), sandboxed, with helpers (dim/tool/bold/
  diff/clip/json); render errors fall back to generic. `examples/renderers/
  search.star` documents the API. NB Starlark's % has no precision (%.2f).
- **1-col selection gutter** (was 2) to halve focus-switch reflow.
- **✅ slash command interface:** input starting with `/` opens a live PALETTE
  above the input — matching commands + descriptions, ↑/↓ select, tab complete,
  enter run, esc dismiss (doesn't quit). Registry (`slashCommands`, populated in
  init to avoid the help↔registry init cycle); `/help` enumerates it; unknown /
  ambiguous → hint. Commands: `/help`, `/web [addr]`, `/quit`. Adding one = one
  registry entry (palette + /help pick it up). Unit-tested (`TestTUI_CommandPalette`).
- **◻ next in this slice:** TUI history replay on `--continue`; hot-reload
  renderers on file change (today: loaded once at TUI start).

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
- **ask_user** (`ask.go`): the agent calls `ask_user{question, options, multi}`;
  the turn PAUSES at that tool call until answered — `-p` emits an `ask` event, a
  UI picker / terminal prompt collects the answer, it's returned as the tool
  result and the turn resumes. `Config.Ask` + `withAsk` dispatcher wrapper. One
  question at a time (the answer to one guides the next); `multi:true` lets the
  user pick several. Answer is a single (joined) string, so the `answer`
  protocol is unchanged.
- **ask panel modes (TUI):**
  - *free-text* (no options): drops straight into text entry — you just type.
    (Fix: a no-options ask used to leave typing inert until enter opened a
    hidden field.)
  - *single-select*: ↑/↓ choose · enter selects · `n` attaches a detail.
  - *multi-select* (`multi:true`): ☐/☑ checkboxes; enter TOGGLES the highlighted
    option (space stays a typed char for the custom row), a trailing "✓ done —
    submit N selected" row submits the joined set. `n`/detail disabled here.
  - all modes keep the trailing "✎ custom answer / chat…" free-text row.
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
- **✅ TUI polish:** glamour markdown rendering for finalized assistant replies
  (stream raw → snap to rendered on finalize); diff colorization for tool
  results that look like unified diffs (shown in full, +green/-red); input
  history (↑/↓); busy-spinner inference for autonomous turns; header (workspace
  · ⎇ branch · N tools), a rule, and a status line with key hints; `› ` prompt.
  `render.go` (newMarkdown/renderMarkdown/isDiff/colorizeDiff) unit-tested.
  Visuals need a real terminal to confirm (no-TTY exits cleanly).

### ✅ Slice 4b — worktree → PR
- `pr.go` — built-in `open_pr{title, body, base}` tool: commits the worktree
  changes onto the session branch, pushes it, `gh pr create`. `withPR` dispatcher
  wrapper; `Config.Worktree` + `Config.EnablePR`. **Opt-in via `--pr`** (pushing +
  opening a PR is outward-facing); without it, changes just stay on the branch
  for manual review (Slice 3). System prompt gains "call open_pr when done".
- **Verified:** unit — openPR commits + pushes the branch to a local bare origin
  (with the change in the tree); no-worktree guard. LIVE (`--pr`, 13 tools) — the
  agent node_edit'd main.go, called open_pr, and the branch landed on origin with
  the edit; gh step reported the manual fallback (local remote isn't GitHub).

### ✅ Slice 4c — persistence (PROVEN LIVE)
- `store.go` — `sessionStore` replaces the in-memory store: mirrors the agent
  Entry list (the *active exchange*, the model's source of truth) to a JSONL
  under `~/.dun/sessions/<encoded-root>/<id>.jsonl`, scoped by the workspace
  ROOT (not the ephemeral worktree), à la ~/.claude. `$DUN_HOME` overrides.
- **One representation, not two:** the Entry list is both the model context AND
  the TUI history (each Entry rebuilds a rendered line) — no separate event log,
  so no dual-write corruption surface.
- **Atomic writes:** full rewrite to `<path>.tmp` + `os.Rename` — a crash leaves
  the whole old or whole new file, never torn (the "a/b" safety via rename).
- **File refs extracted:** entry contents > 8 KiB (a `node_read` of a whole
  file, a big diff, verbose exec output) go to content-addressed blobs
  (`blobs/<sha>.blob`); the JSONL keeps a ref. Disk-only — in-memory entries
  hold full content; load re-materializes. Identical reads dedup by hash.
- `session.go` — path helpers (`SessionsDir`/`RootDir`/`NewSessionFile`/
  `SessionFile`/`LatestSession`/`ListSessions`).
- cmd: `--continue` (resume latest for this root), `--resume <id>`, `--sessions`
  (list + exit); emits a `session` event (id + resumed count); TUI forwards the
  flags to its `-p` subprocess. `go install ./cmd/dun` → `dun` on PATH.
- **Verified live vs bonsai:** session 1 stored a fact → session 2 `--continue`
  reloaded 2 entries (same id) and the model recalled it. Unit: round-trip,
  blob extraction (not inlined, re-materialized), compaction persists.
- **◻ deferred:** TUI replays loaded entries as history on resume (today the
  model has the context but the TUI starts blank on `--continue`).

### ✅ Slice 4d — web UI (`dun serve`)
- **A second client of `-p`** — no engine change. `dun -serve --addr` spawns one
  `dun -p` and bridges it to the browser (`serve.go` + embedded `serve.html`):
  - `GET /` → the self-contained page.
  - `GET /events` → **SSE**: replays the event history to a (re)connecting
    browser, then streams live. Reload = full transcript rebuild.
  - `POST /input` → validates `{type:user|answer|stop}` and writes it to the
    engine stdin as one compact JSON line.
  - `serveHub` fans engine stdout to subscribers; a wedged client is dropped
    (not allowed to stall the engine) and can reload to replay.
  - **stdlib only** (net/http SSE + POST) — no websocket/framework dep.
- **Frontend** (vanilla JS, mirrors `tui.go`'s handleEvent): streamed assistant
  text; tool call/result as a native `<details>` with input/output `<pre>`
  panes (browser scroll + Ctrl-F replace the TUI inspector's `/?n`); ask picker
  as radios / **checkboxes for `multi`** / free-text (multi-select is trivial in
  HTML) + a custom-answer field; 🔎/🔔 notifications; token counter.
- **Shared arg-builder:** `pArgs(tuiOpts)` extracted from `startDunProc` so TUI
  and serve spawn `dun -p` identically.
- **Cross-host binding:** `--addr` takes any host (`0.0.0.0:8734`, `:8734`).
  `reachableURLs` prints the LAN-reachable URL(s) — filters loopback + virtual
  NICs (docker/br-/veth/vpn) so you get the real address, not a wall of 172.x.
  A non-loopback bind logs a "no auth" warning.
- **`/web [addr]` (TUI slash command):** starts an EMBEDDED server that mirrors
  the LIVE TUI session — `proc.tap` fans the engine's raw event stream to the
  web hub, and browser input writes to the SAME engine stdin (a `lockedWriter`
  + `dunProc.mu` serialize TUI vs browser writes). So a browser on another host
  watches and drives exactly what the TUI is doing. Defaults to `0.0.0.0:8734`
  (the point is remote access); prints the URLs + a no-auth warning into the
  convo. First `/` slash command in the TUI (unknown → hint).
- **Verified live (cross-host):** `dun -serve -addr 0.0.0.0:8734` on the dev box
  (192.168.1.76), opened from a browser on a DIFFERENT host — header showed the
  `ready` 12-tool list, the token counter (`usage`), and a streamed assistant
  message, all over SSE. Unit tests: `/web` start + tap + no-rebind, unknown
  slash, `reachableURLs`/`loopbackOnly`, hub fan-out/replay/input/drop.
- **◻ deferred:** multi-session (one server = one session); auth/TLS; markdown
  rendering of assistant replies (raw text now); `/web` history replay to a
  browser that connects BEFORE `/web` (it sees events from attach-time on).

### ✅ TUI screen dump (SIGUSR1)
- The alt-screen hides what the TUI shows, so debugging "what is it doing?" is
  hard. `kill -USR1 <tui-pid>` now appends a snapshot — a state header
  (focus/busy/asking/inspecting/sel/convo/size) + the ANSI-stripped `View()` —
  to `$DUN_DUMP_FILE` (default `$TMPDIR/dun-screen.txt`). Repeatable (re-armed;
  appends). NB `dun -tui` re-execs `dun -p`, so signal the PARENT pid.
  `waitForDump` cmd → `dumpMsg` → `writeDump`. Unit-tested (`TestTUI_ScreenDump`).

### ✅ Web option B — xterm.js terminal view (`serveterm.go`)
- **The REAL TUI in the browser**, alongside option A. `dun serve` now serves
  both: `/` = web-native HTML (option A), `/term` = the actual bubbletea TUI via
  xterm.js over a PTY. Cross-linked (each page links the other).
- On a `/term/ws` WebSocket connect, `termWS` spawns `dun -tui` in a pseudo-
  terminal (`creack/pty`) and pipes raw bytes both ways (`gorilla/websocket`).
  Framing browser→server: `0x00`+bytes = keystrokes → PTY; `0x01`+4 bytes =
  resize → `pty.Setsize`. Server→browser: raw PTY output as binary frames.
- **Resize reflows for real:** xterm fit-addon → resize frame → `pty.Setsize` →
  the PTY delivers SIGWINCH → bubbletea gets `WindowSizeMsg` → the TUI relays
  out. (Option A reflows via CSS; B via the terminal's own winsize — the point
  of the comparison.)
- xterm.js/css + fit-addon **vendored** under `cmd/dun/web/` and `//go:embed`ed
  (self-contained, no CDN). `procArgs(o, mode)` (was `pArgs`) builds `-p` or
  `-tui` argv. Process-group kill on WS close reaps `dun -tui` + its `dun -p`.
- **Verified live (cross-host):** opened `/term` from a browser on another host
  — the real TUI booted (header + 12-tool ready line + magenta focus divider +
  status bar), and typed keystrokes reached the bubbletea input over WS→PTY.
- **A vs B:** A = native web (selectable DOM, real checkboxes, mobile-friendlier,
  reimplements rendering); B = 100% TUI fidelity (inspector, colors, reflow) at
  the cost of a terminal-in-browser (xterm quirks, PTY dep).

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
