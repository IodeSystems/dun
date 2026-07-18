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

### ✅ tools/ttydrive — drive TUIs non-interactively (nested module)
- **Problem:** couldn't SEE/drive dun's Bubble Tea UIs headlessly (the SIGUSR1
  dump only covers the main TUI, not the `--setup` wizard).
- **`tools/ttydrive`** (own go.mod: creack/pty + hinshun/vt10x): runs a program
  in a PTY of a fixed size, reads a keystroke SCRIPT from stdin, and dumps the
  emulated screen as plain text. Directives: `send/type`, `key <name>…`, `wait`,
  `waitfor <substr>`, `resize`, `dump`. A vt10x emulator turns raw output into a
  readable grid — works for inline AND alt-screen bubbletea.
- **Proven:** drove `dun --setup` end to end (URL→key→the LIVE `/v1/models`
  list→navigate) and `dun -tui` (opened the command palette). Gotcha: don't grep
  the dump on a run of `─` (collides with TUI dividers) — match `┌`/`└`.
- `tools/` pruned from dun's self-update walk (separate modules, not dun's build).

### ✅ Config wizard (`dun --setup`) + config file
- **Problem:** LLM url/model/key came only from flags (hardcoded defaults) + one
  env var — re-typed or script-edited every run, painful when trying new models.
- **`cmd/dun/config.go`:** `~/.dun/config.json` (`$DUN_HOME`, 0600 — key is
  secret). **`dun --setup` is a Bubble Tea wizard** (`setup.go`, re-runnable):
  URL → masked key → model. The model step fetches the endpoint's `/v1/models`
  and shows a **navigable list** (↑/↓, enter) with a "type a name" row, so you
  pick a new model by eye (falls back to typing if offline). `/config` TUI
  command shows the live settings + points at `--setup`.
- **Precedence:** CLI flag > env (`DUN_URL`/`DUN_MODEL`/`DUN_LLM_KEY`) > config
  file > built-in — a one-off `--model X` still wins. Verified: wizard writes
  0600 json; file → flag defaults; env overrides file. Tests: round-trip,
  firstNonEmpty, maskKey.

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
  ambiguous → hint. Commands: `/help`, `/config`, `/quit`. Adding one = one
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

### ✅ Slice 4d — web UI = the TUI over xterm.js (`serveterm.go`)
- **`dun -serve --addr` serves the REAL bubbletea TUI in the browser.** On a
  `/term/ws` WebSocket connect, `termWS` spawns `dun -tui` in a pseudo-terminal
  (`creack/pty`) and pipes raw bytes both ways (`gorilla/websocket`). Framing
  browser→server: `0x00`+bytes = keystrokes → PTY; `0x01`+4 bytes = resize →
  `pty.Setsize`; server→browser: raw PTY output. Page served at `/` and `/term`;
  xterm.js/css + fit-addon **vendored** under `cmd/dun/web/` + `//go:embed` (no
  CDN). Each browser gets its OWN session (use `--continue` to resume).
- **Resize reflows for real:** xterm fit-addon → resize frame → `pty.Setsize` →
  PTY SIGWINCH → bubbletea `WindowSizeMsg` → the TUI reflows like a terminal.
- **Cross-host:** `--addr` takes any host; `reachableURLs` prints the LAN URL(s)
  (filters loopback + docker/vpn NICs); non-loopback bind logs a no-auth warning.
  Verified: `/term` opened from a browser on another host, real TUI booted, typed
  keystrokes reached bubbletea over WS→PTY.
- **Web sessions disable exit:** `termWS` spawns `dun -tui --disable-exit` so a
  stray ctrl+c/esc in the browser doesn't kill the session — you leave by closing
  the tab (drops the socket → process-group kill). See `--disable-exit` below.
- **Dropped option A** (the web-native HTML view over the `-p` SSE event stream,
  and the `/web` live-session-mirror slash command): the TUI-over-xterm is the
  keeper, so `serve.go`/`serve.html`/the hub/`proc.tap`/`lockedWriter` are gone.

### ✅ `--disable-exit`
- TUI flag: ctrl+c and esc no longer quit (guarded in Update + updateAsking);
  exit only via the deliberate `/quit`. Status bar shows "/quit to exit". Forced
  on for browser/web sessions (above). `procArgs` appends it for `-tui`.
  Unit-tested (`TestTUI_DisableExit`) + driven live via ttydrive (ctrl+c → TUI
  stays up).

### ✅ TUI screen dump (SIGUSR1)
- The alt-screen hides what the TUI shows, so debugging "what is it doing?" is
  hard. `kill -USR1 <tui-pid>` now appends a snapshot — a state header
  (focus/busy/asking/inspecting/sel/convo/size) + the ANSI-stripped `View()` —
  to `$DUN_DUMP_FILE` (default `$TMPDIR/dun-screen.txt`). Repeatable (re-armed;
  appends). NB `dun -tui` re-execs `dun -p`, so signal the PARENT pid.
  `waitForDump` cmd → `dumpMsg` → `writeDump`. Unit-tested (`TestTUI_ScreenDump`).

### ◻ Slice L — launcher / daemon (SPEC; supervised independent sessions)

> ⚠ **BLOCKER found while building L2 (needs a call).** dun gives each session
> its OWN git worktree (the isolation model), and every MCP server is bound to
> that worktree: `poly-lsp-mcp --root <worktree>`, `mcpshell --files-dir
> <worktree>` (+ per-session `export` eval state), raglit over a per-session temp
> home. So two sessions never share an effective workspace → **there is nothing
> to share**, and L2's "boot once per workspace" win does NOT apply to the
> default (worktree) mode. It only helps `--no-worktree` sessions on one dir
> (and even then mcpshell needs mcpmgr thread-scoping for its per-session eval
> state). Options: (1) scope L2 sharing to `--no-worktree` only; (2) build L1
> (supervisor/registry/kick-warning — worktree-agnostic) now and re-scope L2;
> (3) drop L2. See the exchange for the chosen path.

**Goal.** A thin, long-lived launcher that (1) owns the MCP servers ONCE per
workspace so sessions don't each pay the ~10s boot, (2) supervises independent
session engines it can list/kill/reload, and (3) knows what's attached so it can
warn before a shutdown kicks web sessions. Each session is its OWN conversation
(option b — no shared-conversation/turn semantics).

**Non-goals (v1):** shared conversation / collaborative drive; multiple clients
on one session (v1 is one client per session); cross-host control (the launcher
is local; remote reach stays via `dun -serve`, itself a launcher client); auth
(local unix socket, 0700).

**Process model.**
```
dun -d (launcher, thin, stable)            ← owns nothing that changes often
├─ mcpmgr + 3 MCP servers  (pool keyed by WORKSPACE — poly-lsp/raglit are
│                            workspace-bound, so share within a ws, not across)
├─ session S1 = `dun -p` subprocess (reloadable)  ─ tool calls RPC'd to launcher
├─ session S2 = `dun -p` subprocess               ─ …dispatched thread-scoped
└─ control socket  $DUN_HOME/launcher.sock (unix, 0700)
clients: `dun -tui` and `dun -serve`/xterm → connect, spawn/attach a session,
         proxy the session's -p event stream ↔ their UI.
```
Sessions are SUBPROCESSES (not goroutines) so the launcher stays thin and the
frequently-changing engine/tooling can be hot-reloaded without bouncing MCP.

**Two channels over the socket:**
1. *control/RPC* — line-JSON: `spawn{workspace,model,…}→{id}`, `list→[{id,ws,
   kind,pid,attached,startedAt}]`, `attach{id}`, `kill{id}`, `reload`,
   `shutdown{force?}→{attached,web}` (refuses if attached>0 unless force).
2. *tool dispatch* — a session's `ToolDispatcher` RPCs `{sessionID,toolCall}` to
   the launcher; launcher runs `mcpmgr.Call(threadID=sessionID, tc)` against the
   workspace's shared servers (thread-scoping already exists in mcpmgr) and
   returns the result. Latency: one local-socket hop (µs) on top of the MCP
   call — negligible.
The client's session I/O (the `-p` event stream + user/answer/stop input) is the
existing protocol, proxied by the launcher between the client socket and the
session subprocess's stdio.

**Phases (build + stop-anywhere):**
- **L1 — supervisor skeleton (no MCP sharing yet).** `dun -d` + control socket +
  session registry; `dun -tui`/`-serve` auto-start the launcher and ask it to
  `spawn`/`attach`; sessions are still today's full stacks. Delivers: one entry
  point, `dun --sessions`/`--kill` via the launcher, groundwork. (Modest on its
  own — no boot-time win yet.)
- **L2 — shared MCP (the payoff).** Launcher owns the per-workspace MCP pool;
  `dun -p` gains a "dispatch to launcher" ToolDispatcher instead of spawning its
  own mcpmgr. Session start drops ~10s → ~instant on a warm workspace. This is
  the reason to do the launcher.
- **L3 — hot reload.** `reload` (or a source-change watch) rebuilds `dun` and
  respawns session engines / signals UIs to reconnect, launcher + MCP staying up.
  Supersedes today's whole-tree self-update for launcher-managed sessions; the
  launcher itself updates only on explicit request (rare — its "surface" is
  stable). Standalone `dun -tui` (no launcher) keeps the current self-update.
- **L4 — kick-warning + graceful shutdown.** `dun -d shutdown` / Ctrl-C on the
  launcher counts attached sessions (esp. web) and refuses without `--force`,
  printing "N attached (M web)". Graceful: stop sessions, then MCP servers.

**Files (new):** `launcher.go` (daemon: registry, MCP pool, supervise),
`launchproto.go` (control + dispatch JSON), `launcherclient.go` (auto-start +
spawn/attach + proxy). `mcp.go` grows a "dispatch to launcher" ToolDispatcher
(L2). `dun -p` gains a flag to source tools from the launcher, not local mcpmgr.

**Decisions the USER owns (flagged before building):**
- **Auto-start vs explicit daemon:** recommend LAZY auto-start on first
  `dun -tui`/`-serve` (+ `dun -d` to pre-warm / keep resident). Confirm.
- **MCP pool lifetime:** keep a workspace's servers alive after its last session
  exits (warm for the next), with an idle TTL to reclaim — or stop immediately?
  Recommend idle-TTL (default ~10m). Confirm.
- **Session↔client cardinality:** v1 = 1:1 (attach only if free). Multi-viewer
  (one driver + N watchers) is a later add — OK to defer?

**Risks / untested:** the tool-dispatch RPC must preserve agentkit's error
contract (model-facing errors go INTO the result string; a real Go error aborts
the Turn) across the socket — the launcher must distinguish and re-encode.
mcpmgr thread-scoping under concurrent sessions on one server needs a load check.
Socket lifecycle (stale sock after a crash → detect + reclaim). Windows has no
unix-socket-by-default path (dun targets Linux, so fine).

**Optional extensions (out of scope):** cross-host launcher control; per-session
resource caps; a `dun -d status` TUI; sharing raglit's index across sessions
(already per-workspace, so mostly free once the pool is per-workspace).

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
