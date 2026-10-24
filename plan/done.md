# dun — done (archive)

> Completed work, moved out of plan.md. plan.md keeps a one-line pointer per
> tree. Newest-ish at bottom within a slice group; order mirrors build order.

## Tooling

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

## Slices

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
- **Verified live** (both modes): spawned all 3 (10 tools),
  the default LLM DOES tool-call; the agent self-corrected a bad
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
  events to the viewport instead of scrolling their own scrollback. LATER
  changed to `WithMouseAllMotion` (1003) to match claude — see "Termux taps +
  the kitty probe" below.
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
- **✅ horizontal arrow navigation (`tui.go`):** arrow keys hop panes at the
  edges — `[conversation] ← input → [suggestions]`. Left at the FRONT of the
  input focuses the conversation (bottom message); right from a plain convo
  message returns to the input (a docs summary keeps its own → to descend, never
  hops); right from an EMPTY input opens an arrow-navigable suggestion selector
  (↑/↓ highlight, enter sends, left/esc/type closes — digit 1-N still
  quick-picks). Tests: `TestTUI_ArrowNav` (edge hops + selector nav); docs-nav
  preserved.

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
> **SUPERSEDED TWICE.** `open_pr` was folded into `ship` as its `pr` mode (Ship
> rework), and then the `gh` call was removed entirely (No unbounded foreground
> exec). Nothing described below is live code — kept for why the shape changed.
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
- **Verified live:** session 1 stored a fact → session 2 `--continue`
  reloaded 2 entries (same id) and the model recalled it. Unit: round-trip,
  blob extraction (not inlined, re-materialized), compaction persists.
- **✅ TUI history replay on resume:** the model always had the resumed context,
  but the TUI started blank. `Harness.History()` maps loaded store entries →
  neutral `HistoryItem`s (chronological; drops compaction markers + empty
  assistant turns; decodes tool-call args from JSON; pairs call+result by id);
  `-p` emits a `history` event right after `session` (before any LLM turn). The
  TUI's `replay` rebuilds scrollback reusing the live render paths (user echo,
  assistant markdown, folded tool call/result via the shared `foldedTool`,
  notifications) + a "── resumed N entries ──" delimiter. **Verified live:**
  `dun -p --continue` on a real 2-entry session emitted the history event before
  startup finished. Unit: `TestHarness_History` (mapping) + `TestTUI_HistoryReplay`
  (replay, folded block, unmatched-result standalone).

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
  the tab (drops the socket → process-group kill). See `--disable-exit`.
- **Dropped option A** (the web-native HTML view over the `-p` SSE event stream,
  and the `/web` live-session-mirror slash command): the TUI-over-xterm is the
  keeper, so `serve.go`/`serve.html`/the hub/`proc.tap`/`lockedWriter` are gone.

### ✅ `--disable-exit`
- TUI flag: ctrl+c and esc no longer quit (guarded in Update + updateAsking);
  exit only via the deliberate `/quit`. Status bar shows "/quit to exit". Forced
  on for browser/web sessions. `procArgs` appends it for `-tui`.
  Unit-tested (`TestTUI_DisableExit`) + driven live via ttydrive (ctrl+c → TUI
  stays up).

### ✅ TUI screen dump (SIGUSR1)
- The alt-screen hides what the TUI shows, so debugging "what is it doing?" is
  hard. `kill -USR1 <tui-pid>` now appends a snapshot — a state header
  (focus/busy/asking/inspecting/sel/convo/size) + the ANSI-stripped `View()` —
  to `$DUN_DUMP_FILE` (default `$TMPDIR/dun-screen.txt`). Repeatable (re-armed;
  appends). NB `dun -tui` re-execs `dun -p`, so signal the PARENT pid.
  `waitForDump` cmd → `dumpMsg` → `writeDump`. Unit-tested (`TestTUI_ScreenDump`).

### ✅ Slice L — launcher / daemon (L1 supervisor + hot-reload; L2 dropped)

**Repivot.** L2 (shared MCP) was **dropped**: dun gives each session its own git
worktree, and every MCP server is worktree-bound (`poly-lsp-mcp --root`,
`mcpshell --files-dir` + per-session `export` eval state, raglit per-session
home) — so two sessions never share an effective workspace, and there was
nothing to share. The launcher's real value here is supervision + central
build/reload, which is what got built.

- **`dun -d` launcher** (`launcher.go` + `launchproto.go` + `launcherclient.go`):
  a thin daemon on a unix socket (`$DUN_HOME/launcher.sock`, 0700). Lazy
  auto-start (first `dun -tui`/`-serve` spawns it detached); idle-exits after
  10m with no sessions; self-cleaning socket (stale-sock reclaim on start).
- **Registry (L1).** Every session registers on start over a long-lived conn
  (its close = "left"); `dun -d status` lists them (id/kind/pid/version/age/ws);
  `dun -d shutdown` **refuses while sessions are attached unless `--force`** —
  and reports the web count (the kick-warning, launcher side).
- **Central build + hot-reload.** The launcher owns the source watch + rebuild
  (one watcher/build for the whole box, vs every `dun` self-checking): every 2s
  it rebuilds the shared binary when the tree changed (`rebuildDun`, shared with
  selfUpdate) and PUSHES `reload` to registered sessions. The TUI shows a
  `↻ <ver> (/reload)` header indicator; `/reload` restarts cleanly into the
  fresh binary (bubbletea restores the terminal, then `runTUI` re-execs). The
  launcher itself never re-execs (stable "surface"); a release build (srcDir="")
  does no watch/build.
- **Kick-warning (serve side).** `dun -serve` counts live browser PTYs
  (`activeWeb`); Ctrl-C with sessions attached warns "N attached — Ctrl-C again
  to drop them" and a second confirms.
- **Verified live:** launcher auto-started from a `dun -tui`; `dun -d status`
  showed the session; touching a source file → launcher rebuilt `~/go/bin/dun`
  (mtime bumped, log "rebuilt → … notifying 1 sessions") → the running TUI's
  header lit `↻ … (/reload)`. Unit: `TestLauncher_Registry` (register/status/
  shutdown-refusal/deregister).

### ✅ Next-message suggestions (`--suggest`)
- After each turn the engine predicts the 3–4 messages the USER is most likely to
  send next, each with a rough probability, and the TUI offers them as quick
  picks. One extra non-tool LLM round-trip per turn → **opt-in** (`--suggest`).
- **Engine (`suggest.go`):** `Harness.Suggestions(ctx)` builds the conversation
  (DefaultContextBuilder, no system) + an instruction, calls the LLM once with
  JSON-mode ResponseFormat, and `parseSuggestions` defensively extracts the JSON
  (small models wrap it in prose), clamps prob to [0,1], drops empties, sorts
  desc, caps 4. `main.go` emits a `suggestions` event after `done`
  (turn + continueTurn); propagated through `procArgs` so web sessions get it.
- **TUI:** the picker shows ONLY when idle with an empty input (never fights a
  running turn or typing); a new turn's `token`/`tool_call` clears it. Digit
  `1–N` sends that suggestion (via the shared `sendUser`); probabilities shown as
  `%`. Status: "next? · 1–N pick · or type". (Arrow-driven selector: see Slice 2
  horizontal arrow navigation.)
- **Verified:** unit — `parseSuggestions` (prose-wrapped/sort/clamp/junk),
  `TestTUI_Suggestions` (event→picker→digit-send→clear), picker render. Live LLM
  round-trip blocked at test time by a 429-saturated endpoint (best-effort:
  suggestions silently skip on error).

### ✅ Declarative MCP servers (`dun.json` + `dun.local.json`)
- **Problem:** the 3 tool servers were hardcoded in `DefaultServers` and had to
  be on PATH under exactly those names — breaks when binaries live elsewhere, a
  project wants a 4th server, a project wants raglit off (no docs corpus), or a
  server needs a per-machine env var.
- **Two files, two KINDS of fact:** `dun.json` (committed — describes the
  PROJECT) and `dun.local.json` (gitignored — describes THIS MACHINE: binary
  paths, DSNs, secrets).
- **Precedence** extends the LLM-settings model: built-in defaults < `dun.json` <
  `dun.local.json` < `Servers` set in Go. Servers merge **BY ID**, omitted fields
  **INHERIT** (override one binary's path without restating its args; add a 4th
  without re-listing the first 3 — avoids silent fork-and-drift).
  `"disabled": true` drops a built-in in one line.
- **Two deliberate asymmetries:** (1) `disabled` is only ever turned ON by a
  later layer — a bool can't tell "false" from "absent", so a local file can't
  silently resurrect a server the project turned off (re-enable = remove the
  disabling entry). (2) A parse error names its own file (unattributed JSON
  errors misdirect the reader in a two-file config).
- `{{workspace}}` / `{{raglit_home}}` substituted at spawn (same token llm-bench
  toolsets use). `Server` gains `Env` + `Timeout` (threaded into
  `mcpmgr.MCPConfig`); `Config` gains `ConfigDir` (the workspace may be an
  isolated worktree while the config lives with the developer's checkout). A
  commandless server is rejected at load, not obscurely at exec.
- Files: `servers_config.go`, `servers_config_test.go`, `dun.example.json`,
  README + `.gitignore`. `harness.go` wired to load+merge. Unit-tested.

### ✅ Retry UX + mid-turn messages (provider failures stop killing the session)
- **Problem:** a provider fault printed one generic line and `dun` EXITED. The
  retries were already there — agentkit's `postWithRetry` handles 429 (honoring
  Retry-After), 5xx (attempt-capped), and transport failures with exp backoff
  1s→30s under a 5m budget — but every decision was a `log.Printf`, and
  `tui.go` sends engine stderr to a temp file. So the user watched a frozen
  cursor for up to 5 minutes and then got "a generic single failure to connect".
  Worse, a stream that died MID-generation was not retried at all (not resumable
  at the client: tokens are already out), so it killed the turn outright.
- **agentkit — `llm.RetryEvent` + `Client.OnRetry`** (`llm/retryevent.go`): one
  callback per retry decision plus a terminal `recovered`/`giveup`, carrying
  attempt, status, delay, elapsed/budget, the server's own first body line, and
  the 5xx attempt cap. Fired from all three branches and every give-up path.
  `Client.RetryPolicy()` exports the schedule + budget so an OUTER retry reuses
  ONE policy instead of inventing a second set of numbers.
- **agentkit — rich 429s are no longer thrown away** (`llm/backpressure.go`):
  corrallm answers a saturated backend with `Retry-After` +
  `X-RateLimit-Capacity/-InFlight/-Waiting` + a JSON body (`reason`:
  rejected|queue-timeout|spill|exhausted, capacity/in_flight/waiting). The client
  read only `retry_after` and discarded the rest, so a user queued behind other
  work was told "429" when the server had said "4/4 slots busy, 2 ahead, come
  back in 10s". `ServerAsked` marks a delay as the server's estimate rather than
  our guess.
- **dun — turn-scope retry** (`retry.go`): `runTurn` re-runs a turn while
  `llm.TransientUpstream` says the PROVIDER failed, bounded by the client's own
  budget (`DUN_TURN_RETRY_BUDGET` overrides; `0` disables). Safe because the
  conversation is persisted — the retry rebuilds context, so completed tool calls
  are not redone, only the interrupted generation. Cancellation is checked
  explicitly: a cancelled request arrives wrapped in `*url.Error`, which
  satisfies `net.Error` and therefore LOOKS transient.
- **dun — the session survives a give-up.** `prepareTurn` (before every attempt
  and every Ask/Continue) pairs off any tool CALL with no result: providers
  deserialize historical tool_calls, so an unanswered one makes every later
  request fail identically. `store.pairToolResults` closes the mirror case at
  load (a result whose call `sanitizeOnLoad` re-kinded). Net effect: sending
  another message resumes a dead session.
- **dun — mid-turn messages ride the lift path.** `Harness.Say` buffers a message
  the way `Notify` buffers a background completion, and `withLiftedQueue` appends
  the whole buffer (labelled `[user]` / `[background]`) to the next tool result —
  so a message typed while the agent works lands INSIDE the running turn, costs
  no round-trip, and any number batch in order. The engine routes user events to
  `Say` while `turnActive`; the TUI no longer blocks send on `busy`. Whatever a
  turn doesn't pick up is published (user → user turn) and drained by the loop's
  `Pending()` check.
- **UI:** `-p` gains `retry` (scope request|turn, kind, attempt, delay/elapsed/
  budget ms, queue detail, ready-made `text`) and `queued`. TUI shows a status
  banner that COUNTS DOWN on the spinner tick, records the first wait + the
  outcome in scrollback (not every attempt — a long outage would bury the
  conversation), discards the half-streamed reply on a turn-scope retry, and
  reports "N messages queued for this turn". Human mode prints ⏳/✓/✗ lines and,
  on failure, the `dun --continue` command to resume.
- **Verified live** against a fake provider doing all of it in sequence: rich 429
  → 502 → recovered → mid-stream death → turn retry → clean reply, with the
  events observed on the wire; and two messages sent during a `sleep 3` exec
  arriving in ONE tool result (`built\n\n[user] …\n\n[user] …`), turns=2. Unit:
  `retry_test.go`, `queue_test.go`, `sanitize_test.go`, `tui_test.go` (banner,
  countdown, partial discard, give-up usability, send-while-busy),
  agentkit `llm/retryevent_test.go`.

### ✅ Ship rework — one tool, three modes (2026-07-31)
- **Problem:** `ship` was clean → fetch → rebase → push → ff local base → delete
  branch, and its checks were UNWIRED — `main.go` never populated `ShipCfg`, so
  `Checks` was always nil. It also ran `git checkout <base>` in the *main* repo
  (changing what branch the human was on, racing any other session) and deleted
  the branch it had just pushed while never pushing base: a "successful" ship
  left the work only in the local checkout.
- **Rebuilt around the invariant:** never push anything that was not verified in
  exactly the state it will land in. Order is fetch → rebase → checks → push,
  nothing mutating in between. Verifying BEFORE the rebase — the obvious order,
  and the one the old ship implied — tests a tree that will never exist anywhere.
- **Modes are the only thing that varies:** verify stops after the checks, push
  lands the branch, pr lands it and hands off the pull request. `open_pr` is GONE
  — push exists once. `ship.allow` is the policy surface (a sub-agent gets
  `allow:["verify"]`); `allowBasePush` defaults OFF, so commits made straight to
  main are detected and refused with the pipeline still run.
- **Origin moving during the checks** is the residual hole merge queues exist
  for: ship re-fetches after the checks and goes round again if the base moved,
  bounded at 3 rounds — a permanently busy trunk must fail loudly, not spin.
- **A conflicted rebase is a state, not an argument** (the old tool wanted
  `action:"continue-rebase"` from the model). Detected from `rebase-merge`/
  `rebase-apply` and resumed — and the resume must run BEFORE anything reads the
  branch, since a stopped rebase leaves HEAD detached and "what branch am I on"
  has no answer. A test caught that ordering; the first implementation had it
  backwards.
- **Checks are serial waves of parallel commands**, shape carrying the semantics
  (array ordered, object not). A wave reports EVERY failure in it; names sorted
  before reporting, or Go's map iteration makes two identical runs read as a
  changed result.
- **Verified:** `ship_test.go` runs the pipeline against a real bare origin —
  base-moved re-verify, rebase resume, failed-check-pushes-nothing, base-branch
  refusal.

### ✅ No unbounded foreground exec; gh removed from ship (2026-07-31)
- **Problem, measured:** a live session sat idle 14 minutes. The model hit an
  expired gh token, ran `gh auth login` through `exec`, and gh — stdin on
  `/dev/null`, stdout a pipe, no controlling tty — started an **OAuth device
  flow** and polled GitHub for a one-time code printed into a pipe no human was
  reading, until the code expired. Found by `ps --forest` + `/proc/PID/fd`
  (ESTAB socket to 140.82.116.4:443, `ep_poll`).
- **Not a dun tty-detection bug.** Reproduced standalone: gh 2.45 does not gate
  the device flow on `CanPrompt()`. Every interactivity check already failed.
  `detach()` (Slice: no child keeps dun's terminal) turned what would have been a
  visible prompt into a silent block — a fix converting one failure mode into a
  quieter one.
- **Foreground exec is now bounded** (`exec.go`): `defaultExecTimeout` = 5m via a
  ctx deadline, which `killGroup` already propagates to the whole process group.
  A caller with its own deadline keeps it, so ship's `checkTimeout` is not
  silently shortened; `background:true` is exempt via `WithoutExecTimeout` —
  being unbounded is the entire point of a background job.
- **A timeout must still render `[exit: …]`.** `runChecks` string-matches that
  marker, so a timeout worded any other way would read as a PASSING check. The
  message names the timeout, says the output is partial, and tells the model that
  a command waiting for input will never get any.
- **ship no longer invokes gh at all.** `pr` mode pushes, then `handOffPR` prints
  the `gh pr create` line for a human. `ghRun`/`openOrUpdatePR`/`prFromCommits`
  deleted. The mode, the `--pr` flag and existing configs are unchanged, so this
  is revertible when gh gets a non-hanging call path.
- **Verified:** deadline kills and reports with the marker; `bound()` respects a
  caller's deadline and the background exemption; pr mode pushes and hands off
  without claiming to have opened a PR.
- **Known, not fixed:** killing `docker run` on a timeout does not stop the
  container — see the icebox.

### ✅ Exit codes, background-job visibility, container cleanup (2026-07-31)
Three items from the same root: the harness could not tell the model what was
actually happening.

- **A. `ExecBackend` returns a result, not a string.** Failure was
  `strings.Contains(out, "[exit:")` — so a check that PRINTED the marker (a test
  asserting on exec's own output) failed spuriously, and anything that dropped it
  read as a silent PASS. Two consumers had grown onto that string: `runChecks`
  and the foreground timeout, whose wording was chosen to satisfy it. `Run` now
  returns `ExecResult{Output, Code, TimedOut, Limit, Err}` with `Failed()` and
  `Render()`; the `[exit: …]` marker is still RENDERED for the model, just no
  longer parsed. A missing exec backend became `Code:-1` rather than a string
  that read as a passing check.
- **B. Background jobs stream, and can be tuned.** They were one terminal event:
  nothing until exit, stdout+stderr merged, success inferred from the ABSENCE of
  a marker, and the whole output inlined into the model's context.
  `ExecBackend.Run` grew an `io.Writer` tee (Stdout and Stderr set to the SAME
  writer value on purpose — os/exec gives interface-equal streams one pipe and
  one goroutine, which is what makes the interleaving faithful and race-free).
  `bgjob.go` holds the registry: output goes to a per-job LOG FILE whose path the
  model is given, completion carries the exit code and a bounded tail, and
  `exec_monitor(job, buffer_bytes, grep, ignore)` tunes a running job.
  - Keyed by job **#id**, not pid: `startBg` hands the model an id, and the pid
    is an `sh` it never sees.
  - **Silent by default.** Streaming every job unasked would narrate a build into
    the context window one line at a time; the knobs are opt-in per job.
  - Partial lines are held back — the model cannot act on half a line and a
    regexp cannot judge one.
  - An explicit `exec_monitor` read flushes what is buffered even under the
    threshold: asking is not the same as a scheduled report.
- **C. A container no longer outlives the timeout that killed it.** Killing the
  `docker run` client does not stop the container — it runs to completion and
  only then does `--rm` remove it. Survivable when the only canceller was session
  teardown; with a 5m deadline on every foreground exec it was a routine leak.
  Runs are now `--name dun-<pid>-<n>` and the cancel path `docker stop`s that
  name before killing the client.
- **Also:** the system prompt described `exec` even with no backend configured
  (a tool the model could plan around and never find), and quoted no deadline.
  Both fixed; the prompt now interpolates `defaultExecTimeout` rather than
  restating it.
- **Verified:** a command that prints `[exit:` passes; exit code 3 survives as
  `Code:3`; the tee sees output without consuming it; a job logs to disk and
  reports `FAILED (exit 2)` with the path; buffer/grep/ignore each do what they
  say; `docker stop` targets the run's own name. `go test -race ./.` clean.
### ✅ D. Slice 5 — sub-agents — **rebuilt 2026-08-01, verified pre-deletion**
`plan/subagents.md` did not survive, so this section is the entire spec. Steps
1–5 were verified live twice before deletion (context offloading, resident
re-askable children, tell_parent/ask_parent, TUI agents pane). Rebuild
reproduces the same code; no re-verification needed — the model (llm.iodesystems)
already does good splitting, and the unit tests cover the rebuild.
A sub-agent is a second `Harness` (Store and Shaper hang off Session, so context
isolation forces the split there).
- **Purpose (USER):** context offloading first, parallelism second. A child does
  the expensive thing — fetch a page, read a 30k-line log — and returns two
  sentences; the tokens that produced them die with it. That is why every default
  here is the cheap side.
- **Decided (USER, 2026-07-31):** custom MCP tools **built INTO dun**
  (in-process) · **tool sets chosen by ROLE** — root gets `agent` +
  `agent_monitor`, child gets `tell_parent` + `ask_parent` and no `ask_user`,
  which is also what enforces depth-1 · `tell_parent(status, message)` — status
  OVERWRITES, message is an event · `agent_monitor` also carries `tell`,
  `resume`, `quit` · async spawn, `wait:true` opts out · a child going idle FIRES
  the monitor, and that is news, so it drives a turn · children are **resident
  until dismissed** and re-askable · **no concurrency cap** · worktree per-spawn,
  default the parent's, and a child there **may write, unenforced** · model
  per-spawn with a config default · `ship` per-spawn bounded by `ship.allow` ·
  session end kills children; on resume they report **stopped** and restart via
  `agent_monitor(resume:)` · the human gets a **dedicated agents pane**.
- **The fact that decided the transport:** `mcpmgr` is request/response only
  (`CallTool` and nothing else — no server→client push). An out-of-process agents
  server therefore could not notify the parent at all. In-process makes
  `tell_parent` a direct `h.Notify` onto the lift path that already exists.
- Role-based tool sets are the ENFORCEMENT, not a convenience: depth-1 holds
  because a child is never handed `agent`, and a child cannot reach the human
  because `ask_user` is simply absent from its set.
- Sub-agents still do not get `ship`'s landing modes — `ship.allow` was already
  the policy surface, so a `work` child is handed `allow:["verify"]`.
- **✅ steps 1–5 (2026-07-31) — the slice is built.** `subagent.go` +
  `plan/subagents.md`.
  - `agent({prompt, model?, wait?})` spawns a child Harness in the parent's
    worktree **sharing its MCP manager** (no second index), on the configured
    child model via a per-model client cache in `cmd/dun`.
  - `tell_parent({status, message, final})` — status overwrites and never
    notifies, message is an event, final is the answer. `ask_parent` blocks;
    `agent_monitor({agent, tail, tell, wait, resume, quit})` answers it.
  - Going idle fires the report onto the lift path, and it is news, so it drives
    a turn. Children stay RESIDENT and re-askable; session end kills them; a
    resumed session reports them **stopped** and `resume:true` restarts them
    from their transcripts.
  - Role picks the tool set in `applyTools` — which is what makes depth-1
    structural and keeps `ask_user` away from children.
  - TUI: an agents pane, live, one row per child with state, elapsed, spend, and
    a loud marker for a blocked one, fed by a new `agents` event.
  - **Verified live** twice. Run 1: parent spawned, child ran
    `wc -l`, called `tell_parent{final:"137"}`, parent answered from the report —
    6.5k tokens spent in the child, none in the parent's window. Run 2:
    `wait:true` returned the count, `agent_monitor(tell:)` re-asked the SAME
    child for the last line (correct), tokens accumulated 10.9k → 30.8k across
    both turns, and the pane's events tracked running → idle → running → idle →
    dismissed.
  - **Two things the live runs taught, both fixed:** without `wait` the parent
    improvised `exec("sleep 3")`, and without `wait` on `tell` it called
    `agent_monitor` four times in a row to poll. Both now have it.
  - A nil-harness child would have panicked the process from its own goroutine;
    it now fails that one agent instead.
- **✅ tool renderers (2026-08-01):** `cmd/dun/renderers.go` grew renderers for
  every tool dun owns — `exec`, `exec_monitor`, `ship`, `agent`,
  `agent_monitor`, `tell_parent`, `ask_parent`, `ask_user`. They all collapsed
  to a clipped sentence that hid the verdict. Now the preview carries the
  verdict and the body stays verbatim: exec shows ✓/✗ with the exit status and
  the last INFORMATIVE line (a bare "FAIL" is a verdict, not a reason); ship
  names the checks that ran or failed, because "verified" without saying what
  ran is a claim it has not earned; agent shows state and SPEND, which is the
  argument for delegating at all; tell_parent/ask_* read the ARGS, since
  "status set." says nothing.
  - The exec renderer matches `[exit:` by POSITION (last line), not
    `strings.Contains` — the same trap the exec code was rewritten to avoid.
    Cosmetic here rather than control flow, but a preview that says ✗ about a
    passing command is still a lie.
- **next:** steps 6–7 — `worktree:"branch"` children (own tree, own servers,
  branch hand-off) and per-spawn `ship` bounded by `ship.allow`. Not started;
  the spec says to wait until shared-tree children have run on real tasks, so
  there is evidence about how often one should have been branched.
- **risks:** children are resident until dismissed and there is no cap, so
  forgotten Harnesses accumulate — the pane makes it visible, nothing prevents
  it. A shared-tree child writing freely means the parent's `ship` can find dirt
  that is not its own; the clean-tree refusal does NOT yet name running children.
  The 5m foreground exec timeout still applies inside a child with nobody
  watching (icebox: configurable timeout, condition now reached).

## Terminal input

### ✅ Termux taps + the kitty probe (both fixed off ONE pty capture)

Two symptoms, one measurement. Captured claude cli 2.1.246's startup in a pty
(`TERM=xterm-256color`, 100×40) and diffed it against dun's:

```
claude:  ESC7 ESC[r ESC8 · ?25h/?25l · ?2004h · ?1004h · ?2031h
         ESC[<u ESC[>1u · ESC[>4;2m · (no alt screen) · ESC[>0q ESC[c at the END
dun:     ESC]11;? ESC\ · ESC[6n · ESC[>0q · <nothing, for the whole capture>
```

- **Tapping a dun pane on Termux never raised the keyboard; tapping a claude
  pane did.** The mode that matters is **1003** (any-motion), not 1002. claude
  2.1.246 enables `1000h 1002h 1003h 1006h`; dun asked for 1002+1006
  (`WithMouseCellMotion`). tmux forwards the ACTIVE pane's mouse mode to the
  outer terminal, and with `mouse on` it upgrades a pane that asks for nothing
  to MODE_MOUSE_BUTTON — so 1002-or-nothing and 1003 hand Termux different
  sequences. Live flags on the device: every claude pane `mouse_all_flag=1`,
  dun's `0`. Fixed by `WithMouseAllMotion` in runTUI and replay; verified the
  new build's pane reports `all=1 any=1 sgr=1`, the same profile as claude's.
- **Measurement trap, twice fallen into.** A pty capture of claude taken from an
  UNTRUSTED cwd stops at the "trust this folder?" prompt and shows NO mouse mode
  at all. That artifact is what made 96f9436 drop mouse mode, and it made this
  round drop it again before the device's tmux flags corrected it. Capture from
  a trusted cwd, or the capture is of the trust dialog, not of claude.
- **The other half of the same coin: finger-scroll.** With tracking inactive on
  the alt screen, `TerminalView.doScroll` sends `KEYCODE_DPAD_UP/DOWN` instead
  of wheel codes — so a swipe arrived as a run of plain ↑/↓ and walked the
  composer's history. One rule now governs the whole vertical axis: **↑/↓ move
  INSIDE whatever you are in — composer caret, suggestion picker, command
  palette — and at its edge they fall through and scroll the conversation one
  row.** History moved to ctrl+↑/↓, which was already bound. (The picker keeps
  digits 1–9 for a direct pick; it just no longer swallows the arrows at the
  ends of the list, which is where a swipe would stall while reading options.)
  The ask/multiple-choice overlay follows the same rule — walk the options (or
  the caret, while typing a detail/custom answer), then scroll — because a
  question you cannot read the backlog behind is one you cannot answer.
- **ctrl+end / ctrl+home**: end a scroll-back (newest output, re-pinned to the
  stream) and jump to the top of the last user message (the current exchange,
  unpinned). Bound in both the main key path and the ask overlay. `/mouse [all|cell|off]` switches
  reporting live (bubbletea's Enable/Disable mouse commands) for anyone who
  wants the wheel back on the desktop: `cell` = wheel scrolls the log, no
  tap-to-type; `all` = the default.
- **dun printed garbage at startup and needed an enter to get past it.** The
  capture ends at `ESC[>0q` — `probeKitty` ran before raw mode and then did a
  BLOCKING read on a canonical tty: read(2) does not return until a newline
  (hence the enter) and ECHO painted the reply on screen (hence the garbage).
  It also counted any byte — that enter included — as kitty support.
- **The probe was asking the wrong question.** `CSI > 0 q` is XTVERSION, not a
  kitty query; claude sends it once at the END of startup to identify the
  terminal. And `CSI > 4 ; 2 m` is xterm modifyOtherKeys, not "kitty mode 4",
  so the pushed flags are now `CSI > 1 u` alone.
- **Rewritten probe:** termios raw+noecho for its own duration (restored on
  return), `CSI ? u` + `CSI c`, `unix.Poll` against the deadline, accept only a
  `CSI ? flags u` reply, DA reply as the terminator so a kitty-less terminal
  costs one round trip. Unlike claude we still probe rather than enable blind:
  bubbletea v1 has no table for `CSI <code> u`, so enabling on a terminal that
  encodes keys we cannot decode makes every key a dead key.
- **Tests** (`kitty_test.go`) run against a real pty, because every one of these
  bugs was invisible without a terminal on the other end: supported, DA-only
  (the old false positive), silent-terminal (the old hang), termios restored.

## Render cost

### ✅ Streaming wrote 17x more than it had to (scroll repaint + colour churn)

dun felt sluggish next to claude on a phone over ssh while CPU said it was fine
(refresh 13–74µs, View 66µs, a token round-trip 116µs, keystroke→first byte
5.7ms vs claude's 4.6ms). The cost was never CPU — it was bytes.

**Measured with `--replay` on a synthetic 600-token stream, 100×40, pane full:**

| | bytes for a ~15s stream |
|---|---|
| before | 1,814,270 |
| + colour packing | 287,015 |
| + scroll region | 106,590 (**17x**) |

- **Scroll repaint.** A full-height pane pinned to the bottom means every new
  wrapped line moves every other line, and Bubble Tea's line diff then rewrites
  the whole screen: measured 37 of 40 rows dirty per scroll frame. `scrollregion.go`
  hands those rows to the terminal instead (DECSTBM via SyncScrollArea/
  ScrollUp/ScrollDown), so a new line costs one inserted line. The region stops
  ONE ROW SHORT of the pane bottom — that row is where streamed text grows a
  character at a time, and pulling it in would repaint the region per token
  (measured 7x WORSE before that was fixed). Off with `DUN_FAST_SCROLL=0`.
- **Ordering is the subtle part.** Bubble Tea runs every Cmd in its own
  goroutine, so two INCREMENTAL region commands can land out of order: three ↑
  presses inside 20ms rendered rows as `38, 36, 37` (reproduced in tmux, not
  just the vt10x harness). `minRegionGap` issues at most one command per 25ms
  and schedules a retry for anything arriving inside the gap.
- **Colour churn.** The renderers above dun style every WORD separately —
  `ESC[38;5;252m` before it and `ESC[0m` after, around trailing pad spaces
  included — so a 35-row region repaint was 28KB for ~2KB of text. `sgrpack.go`
  drops escapes that change nothing; it is a peephole over runs of adjacent
  escapes and touches nothing else (cursor moves, margins, erases pass byte for
  byte). Cached on the entry next to `wrapped`, because packing every block on
  every refresh cost 100µs → 143µs at 200 blocks. Off with `DUN_PACK=0`.
- **Verified**, not assumed: screens captured through tmux (a real emulator)
  with region+packing on vs both off are byte-identical once the stream
  finishes, and mid-stream differ only by the spinner frame and how far the
  in-flight line has grown.
- **Rejected:** `tea.WithFPS(renderHz)` — 7% fewer bytes for double the
  keystroke latency (the renderer would tick at 30Hz instead of 60).

### Startup, and a stall that tmux hides

- `ready` takes 3.46s in a real project vs 0.29s in an empty dir (MCP server
  spawn), against claude's 0.85s to a usable prompt. Not addressed.
- **bubbletea's package init** (tea_init.go) calls `lipgloss.HasDarkBackground()`
  before main runs; termenv's OSC 11 query has a 5s timeout, so first paint is
  5.18s under `TERM=xterm-256color` when nothing answers — caught with a SIGQUIT
  stack dump mid-stall. termenv skips the query for `screen`/`tmux` TERMs, which
  is why dun feels fine under tmux and slow in a bare terminal. dun's OWN query
  is already gated (render.go); this one is the dependency's, and no v1.3.x
  release drops it.

### ✅ The custom-answer row takes typing directly

Highlighting "✎ custom answer / chat…" and typing did nothing until you pressed
enter first — every keystroke fell through `updateAsking` and was swallowed. A
no-options ask already dropped straight into text entry, so the row offering the
same thing behaved differently from the same thing. Now any typed character (or
space) on that row starts the answer AND is the first character of it; enter
still opens it empty, arrows still move off the row without getting stuck, and
`n` types an "n" there while still starting a detail on an option row. The row
says "(just type)" while it is highlighted and not yet capturing.

## ✅ Thinking and tool runs in the TUI (USER, 2026-10-22)

- **Engine:** agentkit streams `StreamChunk.Reasoning` but has no callback for
  it. `reasoning.go` tees the Session's runner (chunks forwarded unchanged) into
  `Config.OnReasoning`; `-p` emits `reasoning` {text} deltas. Only the turn's
  rounds: suggest/rephrase/commit/fork call `untee` first, and children get
  `OnReasoning = nil` like `OnToken`. History emits a `reasoning` item just
  before the entry agentkit stamped it on.
- **TUI thinking** (`thinking.go`): one block per round. Since 2026-10-22
  (USER: "expand to some reasonable size, and subscroll there") it is a
  bounded box (`box.go`): grows to min(10, pane/3) rows, then scrolls inside,
  tail-following while live; done, it keeps its height (no fold). Selected,
  ↑/↓ pgup/pgdn scroll inside it and leave it at its edges.
- **Tool runs** (`toolrun.go`), since 2026-10-22 (USER: "total height should
  never decrease"): a run of calls and the thinking between them is ONE
  grow-only box — a step list (finished thought = its last sentence, call =
  short form + preview, failures keep 2 error lines, same-tool calls group),
  only the streaming thought open. The old fold-and-replace aggregate is gone.
  Every block is held at the most rows it has drawn (`maxH`); property test
  `noshrink_test.go`. Enter → tree; "i" on a call → inspector.
- **mcpshell eval renderer** (`evalrender.go`): code as the call line, hosted
  calls as `◇ ns.tool k=v`, records as a table, values laid out, errors with
  their line marked; fixtures captured from mcpshell in
  `testdata/mcpshell/`.
- **Pager** (`pager.go`): enter on thinking or an aggregate gives the block the
  convo pane (head, input, status stay). Plain text scrolls; an aggregate is a
  tree, a node per call/thought that opens to args + result. Follows a live
  block by identity, not index. esc/q back.
- **Selection band** (`band.go`): the selected block (convo focus) and the
  pager tree's selected node are drawn through a background filter — each
  segment's own background (or the theme base) lightened/darkened 6–14%,
  re-applied after resets, padded to the pane width. Truecolor pulses the
  strength (10 steps, 1.2s); 256 is palette-matched and static; 16 is one
  static bg; DUN_NO_ANIM=1 is static. The clock runs only while a band is
  shown; a step rewrites only the selected rows; the scroll region is off
  while it shows. Measured: BenchmarkPulseStep ~0.5ms/step at 200 blocks; live
  1.1–1.3% of a core in convo focus with or without the pulse, same as input
  focus (the floor is the existing blink/spinner).
- **Proven:** unit tests (`reasoning_test.go`, `thinking_test.go`, `band_test.go`,
  `toolrun_test.go`, `TestReasoningEventReachesTheTUI`) and ttydrive against a
  fake OpenAI-compatible server streaming `reasoning_content` + 5 exec calls,
  with a scratch DUN_HOME dund, including a dund restart → `--continue` history
  replay.
- **Not done:** the web client renders neither (see icebox). Per-call duration
  is not on the wire (tool_call and tool_result are emitted together after the
  call), so the aggregate shows no total time.

## ✅ Turn interrupt (USER, 2026-10-22)

- **Engine:** `Harness.Interrupt` (`interrupt.go`) cancels the turn's context
  with `ErrInterrupted`; runTurn does not retry it. `finishInterrupt` saves the
  cut round's streamed reply (captured in the reasoning tee — agentkit only
  persists a round whose stream completed), answers an orphan call with a
  user-interrupt result, and appends a `[the user interrupted this turn]`
  notification (tag `interrupt`) with the reasoning's duration, length and last
  sentence — never the reasoning itself. All silent appends. `withInterrupt`
  (outermost dispatcher) skips calls reached after the interrupt and relabels
  the one it cut; `liftQueued` lifts nothing while interrupting, so typed
  messages stay pending and the loop starts the next turn at once.
- **Protocol:** command `interrupt`; `done.reason = "interrupted"`; idle →
  `control{id: interrupt}` "nothing to interrupt". `/interrupt` is an engine
  command (web gets it).
- **Fix on the way:** the reasoning tee closed a cancelled stream cleanly, which
  agentkit read as a finished reply (partial persisted, turn "succeeded"). It
  now sends an error chunk, and refuses to start a round on a cancelled ctx.
- **TUI:** ctrl+k while busy interrupts (idle: kill-to-end-of-line); esc
  interrupts after every existing esc consumer; ctrl+c unchanged; busy hint
  `ctrl+k interrupt · ctrl+c quit (the turn keeps running)`; `interrupting…`
  status; thought folds `interrupted after Ns`; open run marked; dim
  `⏹ interrupted — …` row (no-shrink test covers it).
- **Web:** Stop button while busy → `/interrupt`; `done{interrupted}` keeps the
  streamed partial and adds the note.
- **Proven:** `interrupt_test.go` (harness), `cmd/dun/interrupt_test.go`
  (engine over proto, queued → next turn), `interrupt_tui_test.go`, jest; live
  in `tmux -L dunint` against a fake slow-thinking server with a scratch
  DUN_HOME dund — the next request carried partial → marker → message.
- **Not done:** an interrupt during `/rephrase`'s pre-turn call is answered
  "nothing to interrupt" (the turn has not started). A tool that ignores ctx
  holds the interrupt until it returns. Heredoc tool format: the saved partial
  can hold half a heredoc block (prose only was checked).

## ✅ Kill background jobs (USER, 2026-10-22)

- **Harness** (`bgkill.go`): every job runs under its own context; `kill`
  cancels it and records who asked. `KillJob(id, by)` waits (bounded) for the
  job to end; finished → "already finished"; unknown id → error.
  `KillAllJobs` also reaches commands still in their grace (`bgLive`): the
  exec call waiting on one returns `[exit: killed by the user after Ns]`
  inline. `ExecResult.Killed` is the verdict: log line `[dun: killed by …]`,
  pane state `killed`, completion notice "was killed by the user after Ns (it
  did not fail on its own)" — still sent unless muted.
- **Backends:** HostShell — cancellation used to return and leave the command
  running in a shell the next call reused; now `termGroup` sends SIGTERM to the
  shell's group (setsid leader), SIGKILL after `killTermGrace` (2s), and the
  read waits for it. HostExec — `killGroup` is SIGTERM then SIGKILL (was
  SIGKILL at once). Docker — unchanged path: `docker stop --time 2` on the
  named container, then the client's group.
- **Model:** `exec_monitor {job, kill: true}`; tool description and system
  prompt say "ends the job and everything it started".
- **User:** engine command `/jobs` (bare lists running: id, command, age, last
  line), `/jobs kill <n>`, `/jobs kill all`. TUI asks y/n on the status line
  for `kill all` and for `x` on a running job in the activity table (hint
  updated). Web confirms `kill all` with its confirm helper.
- **Interrupt:** the marker lists the cut turn's running jobs (a job still in
  its grace is promoted so it has a number) and says they were not stopped;
  `done.detail` carries "still running: #3 … (2m) — /jobs kill 3 …", shown
  under the TUI's and web's interrupted row.
- **Proven:** `bgkill_test.go` (child + grandchild gone, both host backends;
  SIGTERM-ignoring tree → SIGKILL; grace kill; already finished; exec_monitor
  kill; interrupt listing; docker opt-in `DUN_DOCKER_TEST=1`, passed locally),
  `cmd/dun/jobkill_test.go`, `commands_test.go`, jest. Live: `dun -p` with a
  fake model and scratch DUN_HOME — `ps` showed `sh`, `sleep 300`, `sleep 301`
  before, none after `/jobs kill 1` and after `exec_monitor kill`.
- **Not done:** a command that calls setsid itself leaves the group and is not
  reached. Pid reuse between the group emptying and the SIGKILL is not guarded.

## ✅ RAG ingest skips junk, large dirs, large and binary files (USER, 2026-10-22)

- **Rules** (`rag_ingest.go`, `planIngest`): one file list — `git ls-files -z
  --cached --others --exclude-standard`, or outside git a walk that honors
  nested `.gitignore` (port of agentkit's unexported matcher, `rag_ignore.go`)
  and never enters junk dirs. Skipped: junk names anywhere (node_modules,
  .venv/venv, env with pyvenv.cfg, target beside Cargo.toml/pom.xml, dist,
  build, out, caches, .dun/worktrees, .claude/worktrees, ...); a dir over
  5,000 files / 200 MB, at the highest such dir, root never; files over
  1,024 KB; binary (extension or NUL in 8 KB; PDF exempt). vendor/ is judged
  by size only.
- **Config:** `"rag": {include, exclude, max_file_kb, max_dir_files,
  max_dir_mb}` (`LoadRag`), patterns gitignore syntax and accumulated. include
  beats every automatic skip, exclude beats include; inside a junk dir only an
  include naming it or a path in it reaches.
- **Surfaced:** `ServerState.Ingest` → `/rag on|auto|restart` answer, bare
  `/rag`, the ready hint, and `log.Printf` in the engine log.
- **Live:** scratch tree (3,000-file node_modules, 6,020-file data/, 5 MB
  JSON, sources), git and non-git: 3 files indexed, summary names all skips;
  with an include, 25. This repo: 362 files. Real raglit ingest NOT run
  (isolating raglit's daemon from the user's was not trivial).
- **Not done:** the highest-offending-dir rule skips a large `src/` whole in
  a big monorepo; raise `max_dir_files` or include it.

## ✅ Palette argument completion; /skill for many skills (USER, 2026-10-22)

- **Palette** (`cmd/dun/argcomplete.go`): "/<cmd> " + a partial argument lists
  that command's choices when it has a completer (`completerFor`): ↑/↓, tab
  completes "/<cmd> <value> ", enter runs it. Count line ("12 of 180 skills").
  Window of `paletteWindow` (10) rows around the selection — the command list
  is capped the same way now (it was not capped before).
- **Matching:** case-folded; name prefix > substring > in-order letters > a
  description substring; matched letters bright.
- **Completers:** /skill (names, source, description); every engine command
  with declared `choices` (/rag, /lsp, /mcp, /docker, /worktree, /ship …);
  /mcp <subcommand> then a server id (ids from ready/server events). Not done:
  /resume ids (a disk read + dund call per keystroke), /model names (only
  known after the picker fetches them).
- **/skill:** bare with > 15 skills counts them by source; `/skill <part>`
  that is not a name: one name match runs it, else lists the matches (20).
- **Live:** tmux, scratch HOME + DUN_HOME, 200 generated skills, fake URL:
  `/skill rev` ↓↓ tab + text + enter, `/skill tst` (fuzzy) ↓ enter,
  `/skill zzzq`, `/mcp tools `, `/skill alpha x` (list) — as designed.
- **Known:** in the palette, enter on "/skill rev" runs the highlighted
  choice, so the match list is reached with text after the query
  (`/skill rev fix it`) or outside the palette.

## ✅ A surfaced skill is a load cue, not a fragment (USER, 2026-10-22)

- With /rag on, a hit on a skill's SKILL.md (the `skills` index, d9eb08e)
  ranks in its own group and is written as "A skill matches … call skill
  name=X and follow it before you act", never inlined. The UI row reads
  "skill X".
- **Live:** scratch HOME + DUN_HOME, real model, 101 skills, the target
  sorted last (past the 80-skill list cap). "sign and verify the release
  tarball with minisign": 1 surfaced (~60 tok); the model's first call was
  `skill name=zz-tarball-signing`.
- **Known:** the first index of many skills is slow (~77/min on the shared
  raglit daemon); a skill not yet indexed cannot surface. Test projects go
  into whatever raglit daemon is running, the user's included.

## ✅ Project rules: @imports, CLAUDE.md, /rules modes; skill reference files indexed (USER, 2026-10-22)

- **Rules block** (`rules.go`, replaces `rootAgentsMDBlock`): root AGENTS.md
  (or agents.md) and root CLAUDE.md, then their `@path` imports in full, one
  `### <path> (imported by X)` section each. Claude Code semantics: outside
  fenced blocks and code spans, relative to the importing file, `~` = home,
  depth 5, cycle-safe (a file already in the block is skipped). Missing →
  one line `(x.md, imported by AGENTS.md: not found)`. Caps 64 KiB per file,
  256 KiB total, cut with a note. Re-read on every system rebuild.
- **Decisions / assumptions (reversible):** CLAUDE.md is read too (USER
  2026-10-22 assumption); same file (symlink) or identical bytes → once,
  heading "(also CLAUDE.md)"; a CLAUDE.md that is only `@AGENTS.md` adds
  nothing. An `@token` counts as an import only if it contains `/` or `.`
  (so "ask @carl" is not a missing file). Indented code blocks are not
  treated as code.
- **/rules** (engine command, session group; no clash with TUI slashes):
  bare → files, sizes, estimated tokens, mode (and why a saved mode is not
  in force). `full` (default) · `summary` — one side call (kind "rules")
  condenses the IMPORTS, root files stay verbatim; cached in
  `$DUN_HOME/projects/<id>/rules.json` keyed by a hash of the imports; a
  summary not smaller than the files is refused; stale → full + re-offer ·
  `index` — imports into raglit index `rules`, the block lists the paths
  and says they are searchable; needs /rag (refused with "/rag on first";
  with /rag off later, the block falls back to full). What is indexed is
  saved in rules.json, so leaving index mode forgets it, from a later
  process too. Hits from it are their own surfacing group, framed as
  imported rules.
- **Offer:** full block > 4,000 tokens (meter estimate,
  `rulesOfferTokens`) and there are imports → one control line naming
  /rules summary and /rules index; once per hash of the full block.
- **/context:** "project rules N (/rules <mode>)" under system prompt
  (marginal count when the endpoint counts, else estimate).
- **Skills:** `syncSkillIndex` also indexes each local skill's .md/.markdown/
  .txt/.rst files (≤ 64 KiB, ≤ 50 per skill; not scripts/, hidden dirs,
  node_modules). A hit on one gives the same cue plus "The match is in its
  file X (skill name=S file=X)". One candidate per skill in a pass (a
  skill's files no longer count as several candidate skills — seen live).
  `syncIndex` is shared by the skills and rules indexes.
- **Live** (tmux, scratch HOME + DUN_HOME, real model local-Qwen3.8-27B,
  scratch raglit project `dunscratch-rules` on the shared daemon): AGENTS.md
  importing docs/conventions.md (24 KB, a codeword on line 151). Offer shown
  at start (~6,257 tok). Full: answered PLUM-HARBOR-7731 from the prompt.
  Summary: ~264 tok vs ~8,000; after /clear the model answered the codeword
  and "m13 → t6" from the summary's wording; /context showed "project rules
  206 (/rules summary)". Index without /rag: refused. With /rag: block ~148
  tok, rules.json lists the file, the hit surfaced framed as an imported
  rules file; the model answered via grep. `/rules full` forgot the file
  (raglit search on index rules: no matches). Skill: "zanthic glaze
  crawling" (only in kiln-firing/reference/glaze.md) → cue with
  file=reference/glaze.md; the model's first call was `skill
  name=kiln-firing file=reference/glaze.md`.
- **Known:** the first index-mode question, asked right after /rules index,
  got `search` → no hits (the ingest had not finished); a minute later it
  surfaced. A pointer hit shows raglit's generated caption, not the
  matching line, so the model searched again. A group with ONE candidate
  always surfaces (existing rule): seen live, the kiln skill and
  conventions.md surfaced on unrelated questions. Editing only a skill's
  reference file does not trigger a re-index until the next tool rebuild
  (skillsSig watches SKILL.md). Summary faithfulness is the model's; the
  summary is shown only via /rules token counts, not its text.

## ✅ MCP switches at global, project and session scope (USER, 2026-10-22)

- **What:** every server has an on/off switch, a mode (standalone = dun
  connects to it; scripted = hosted in mcpshell, called through eval) and
  per-tool switches, each at three scopes. Resolution: session if set, else
  project, else global (`mcpscope.go`: `loadCatalog` keeps the global layer
  — built-ins + `$DUN_HOME/mcp.json` — apart from the project layers — repo
  dun.json files, `projects/<id>/`, overlay — and `resolve` adds the
  session's). Global writes go to mcp.json (a switch for a built-in or a
  repo-defined server is a partial entry there, skipped where nothing
  defines the server); project writes to `projects/<id>/dun.local.json`
  (`autostart`, `via`, new `tools: {name: bool}`); session writes to
  `<session>.mcp.json` beside the `.jsonl` (`LoadSessionServers` at Start).
  `reloadServers` applies a change (stop / start / restart the shell / rebuild
  tools); `followServers` runs it at the top of every turn, starting what
  turned on in the background. A tool resolves session map > project map >
  project `disable` list (still wholesale) > global `disable`.
- **Commands:** `/mcp on|off <id> [--global|--project|--session]` (bare:
  session), `/mcp unset <id> --project|--session`, `/mcp mode <id>
  standalone|scripted|unset [--scope]` (`via`, `move`, `shell`/`direct`
  accepted), `/mcp toggle <id> <tool> on|off|unset [--scope]`. `/rag on|off`
  are session switches; `/rag auto` = project on, `/rag manual` = project off
  (kept, not deprecated; manual now also stops it, since off is off). Palette
  completes ids, presets, modes, switches and scope flags.
- **TUI:** bare `/mcp` opens the table in the pager (`server proto global
  project session mode → state`); `g` global on/off, `p`/`s` unset→on→off,
  `m` mode at the mode scope (session; `M` then g/p/s picks it), hint names
  it. Hotkey letters bold+underlined (`stHotKey`, no color, so the Ascii
  profile renders the plain word — tested). `/mcp tools <id>` rows show
  g/p/s marks; g/p/s cycle them, space toggles at session. The TUI no longer
  re-sends `/rag on` after an engine restart (the engine keeps the switches).
- **/mcp add:** presets (`mcppresets.go`; Figma's URL marked unverified),
  `/mcp add <preset>` or typing a preset id prefills; the server row says
  "remote (HTTP)" or "local command (stdio)"; a command is split with shell
  quoting (unbalanced quote = form error) and hides bearer/oauth; a masked
  env row `NAME=value NAME2=$VAR` (an env value that is wholly `$NAME` is
  read at start, like bearer). No working-directory row: ServerSpec has none.
- **Also fixed:** /context counted an added server's tools as built-in
  (`groupTools` keyed by the raw name, not `id__name`).
- **Live** (tmux, scratch HOME + DUN_HOME, real model local-Qwen3.8-27B,
  workspace `dunscratch-mcp`, plan/m3/fakemcp.py as `corp`): added corp in
  the form (bearer masked, "remote (HTTP)") → `on (global)`. `p` twice →
  `○` project, `off (project)`, stopped, `/mcp tools` listed only shell.
  `/mcp on corp` → `on (session)`; the model named `corp__lookup_ticket`.
  `/mcp mode corp scripted` → `scripted (session)`, shell restarted; the
  model ran `help("corp")` → `corp.lookup_ticket` and said it had no
  `corp__lookup_ticket`. Killed every scratch process (TUI, scratch dund,
  launcher, engine) and `dun --continue`: table showed session `●`, mode
  `scripted (session)`. `m` → `standalone (session)`; /context listed
  `mcp: corp 46` (after the groupTools fix). `/mcp unset corp --session` →
  `off (project)`; then the project file was rewritten by hand (another
  writer): the table still said off until a turn; after one turn `on
  (global)`, running, and the next turn's model named
  `corp__lookup_ticket`. `/mcp toggle corp lookup_ticket off --project` →
  tools view `g● p○ s–`, 0 of 1 on, file holds `"tools":
  {"lookup_ticket": false}`.
- **Known / not verified:** a server turned on by another writer joins the
  tool set one turn after the boundary that started it (it starts in the
  background). The presets were not started live (npx); Figma's URL is
  unchecked. Global mode keys and `M` scope picking verified by tests only.
  A `$` value in env that was meant literally (`KEY=$X`) is now read from
  the environment. Removing a server leaves its project/session switch
  entries behind (harmless: skipped). Web and mobile get the text table only.
- **Assumptions:** the catalog stays where definitions already were —
  added servers in mcp.json, built-ins in code, a repo's in its dun.json —
  nothing moved; "migration" is that existing files already mean global
  (mcp.json) and project (the rest), tested by comparing against the old
  one-merge resolution. Stored values stay `direct`/`shell`: an older dun
  rejects any other `via` and would fail to load mcp.json. A shell-placed
  server with no autostart anywhere counts as on (the shell always hosted
  it). `Config.Servers` is now the base the files layer over, not a
  replacement; a child still takes its parent's list as given.

## /mcp cleanup, remove, and the poly-lsp / raglit rename (2026-10-23)

### ✅ Dead code, /mcp remove, /poly-lsp and /raglit
- **What:**
  - Removed what the scopes and rules work orphaned: `SetUserServerVia`,
    `SetLocalDisable`, `SetUserServerDisable`, the package-level and
    `Harness` `SetAutostart`, `toolRun.calls`, `session.busy`. Tests write
    the project switch through `updateLocalServer` (helper `setAutostart`).
    `SetToolEnabled` and `rootAgentsMDBlock` were already gone (rules.go
    holds the rules comment). `MoveServer` is `SetServerVia` plus the
    shell-restart report; `/mcp mode` calls it for every value; `move` and
    `via` still work as synonyms.
  - `/mcp remove <id>` also deletes the id's entries (including `tools`) in
    the current project's `dun.local.json` (and the linked-worktree overlay's)
    and in this session's `.mcp.json`. Other projects' files are not read;
    the message says their entries are skipped when loaded.
  - `/raglit` and `/poly-lsp` replace `/rag` and `/lsp`, no aliases. Server
    ids: `code` → `poly-lsp`, `docs` → `raglit` (`ServerCode`, `ServerDocs`).
    `canonicalServerID` reads the old ids in `readServersFile` (dun.json,
    dun.local.json, mcp.json) and in the session sidecar (a current key beats
    an old one); `builtinServer` accepts both, so `/mcp add` still refuses
    them. New writes use the new ids. Hints, help, palette, README,
    dun.example.json, docs/event-protocol.md, schema.graphql follow.
  - Unchanged on purpose: model-facing tool names (unprefixed), the system
    prompt's family labels, raglit index names (workspace, `skills`,
    `memory`, `rules`, `mcp-<id>`), the `"rag"` config key, the notification
    kind `docs`, the `--rag` / `--lsp` flags.
- **Live** (tmux, scratch HOME + DUN_HOME, workspace `dunscratch-clean`, fake
  model URL): startup lines `poly-lsp off — /poly-lsp on to start it,
  /poly-lsp auto …` and the same for raglit. `/raglit on` → `raglit: session
  on … running · 4 tools`; `/poly-lsp on` → both `●` session in `/mcp`
  (rows poly-lsp, shell, raglit); `/context` listed `mcp: poly-lsp`, `mcp:
  raglit`, `mcp: shell`; bare `/raglit` printed the `/raglit off …` hint.
  Second run with an old-format `dun.json` (`{"id":"docs","autostart":true}`)
  and `mcp.json` (`{"id":"code","autostart":false}`): raglit started at
  launch, table showed `raglit … on (project)` and `poly-lsp … off (global)`.
- **Known / not verified:** `/mcp remove` was tested, not run live. The
  ingest line still starts with `docs:` (it names the index, pinned by
  tests). A web or mobile client that sends a `server` event with id `rag`
  or `lsp` gets "no server"; the shipped mobile client has no hard-coded ids
  (its test fixtures were renamed). `/mcp on docs` (old id typed) is not
  mapped: only files are. `TestScrollRegionTopRowIsNotBlanked` failed once
  under load, passes alone and on rerun.

## ✅ 21. Extensions: raglit and poly-lsp as bundled manifests (USER, 2026-10-23, branch `ext-manifests`)

- **What:** an extension = an MCP server + `extension.json` (`extension.go`
  documents the format). Fields: `id` (= server id = directory name),
  `description`, `server` (the /mcp add ServerSpec, under the global, project
  and session switches; mode standalone/scripted), `slash` {name,
  description, commands: {cmd: {tool, arg}}}, `roles` {search {tool, get},
  index {tool, forget, cli}}, `hooks` [turn.before, workspace.open,
  memory.save, skills.changed, rules.changed, mcp.docs], `host_hooks`
  {ENV: agents_md_guard}, `env`, `skills`. Placeholders `{{workspace}}`,
  `{{project}}`, `{{index}}`, and `{{dir}}` (the extension's own directory,
  user/project only). Unknown fields and bad values are refused with the
  field named (`hooks[1]: unknown event "x.y"`).
  - Bundled: `examples/extensions/{raglit,poly-lsp}/extension.json`, embedded
    (one copy, no drift). DefaultServers builds their servers from them.
  - Loaded in order bundled < `$DUN_HOME/extensions/<id>/` <
    `<repo>/.dun/extensions/<id>/`; the same id replaces. A slash name taken
    by an earlier extension, or one of dun's own commands (engine or TUI),
    is dropped from the later one; the extension still loads.
  - Special cases now lookups: the proactive search = the turn.before
    extensions' search tool (several are merged; each extension after the
    first is its own surfacing group); memory = memory.save + a turn.before
    extension; skills/rules/MCP-docs/workspace indexes = the extension taking
    that hook, through `indexSink`; the AGENTS.md guard = poly-lsp's
    `host_hooks.POLY_LSP_HOOK`; `/raglit`, `/poly-lsp` = their `slash`.
  - **Index seam (`indexSink`):** MCP `{index, paths}` to roles.index.tool
    and .forget when the tool's schema has `paths` and the forget tool
    exists; else the `cli` fallback (only `raglit`: the server's own binary,
    chosen by its base name, with the serve args' store flags). raglit's
    `ingest` takes one `url` and it has no `forget` (serve.go, 2026-10-23),
    so raglit is on the CLI. When raglit adds both, dun switches with no
    code or manifest change.
  - `/ext`: each extension's source, server state, slash, roles, hooks,
    host hooks, path; then what did not load. A command of its own, not a
    column in /mcp: /mcp is servers and switches, /ext is manifests. An
    extension's server is in /mcp like any other.
  - `/<slash> <command> <text>` → control `ext` → `RunExtCommand` → the
    tool with `{arg: text}`; anything else is the server switch.
- **Tests:** characterization first (`builtin_servers_test.go`,
  `cmd/dun/builtin_cmds_test.go`, fake raglit/poly-lsp = the test binary
  re-executed, `fakebin_test.go`), unchanged through the conversion.
  `extension_test.go`: bundled manifests, field-named errors, sources and
  overrides, the MCP/CLI index choice, and the test extension
  (`testdata/extensions/hello`, python3 stdio server: `/hello greet`, and a
  `find` search whose hit surfaces as an aside). `TestTUIDoesNotShadowEngineCommands`
  now also covers extension slashes. `make test` passes.
- **Live** (tmux, scratch HOME + DUN_HOME, workspace `dunscratch-ext`,
  `--rag --lsp`, real model local-Qwen3.8-27B, shared raglit daemon):
  - `/ext` listed raglit and poly-lsp (bundled, running) and hello (user,
    off, its manifest path).
  - `/hello on` → running · 2 tools; `/hello greet Carl` → `hello, Carl`.
  - "What flag do zebra deploys need?" → 1 surfaced (hello's `find` hit);
    the model answered "--frostbite … hangs at 40%" without a tool call.
  - AGENTS.md guard: `node_read sub/thing.go` (sub/AGENTS.md present) →
    `[hook] AGENTS.md guard: read sub/AGENTS.md before read-ing thing.go …`.
  - `/rules index` → 1 file into index "rules"; asked when the deploy
    password rotates → the aside held "From a file the project's rules
    import …" for docs/zebra-rules.md; the answer was Thursday 07:15 UTC,
    team-kestrel (the imported file's fact).
  - Skill cue: 101 skills, `zz-tarball-signing` sorted last; "sign and
    verify the release tarball with minisign" → aside "A skill matches …
    zz-tarball-signing"; the model's first call was
    `skill name=zz-tarball-signing`.
- **Known / not verified:**
  - The MCP index path is tested against a fake only; no real server has
    a `paths` ingest yet.
  - `systemFor` still keys raglit's and poly-lsp's prompt text on their ids
    (`ServerDocs`, `ServerCode`); `--rag`/`--lsp`, `dun doctor` and the
    docker image's binary list are still raglit/poly-lsp by name.
    Bundled ids keep bare tool names (`builtinServer`); a user extension's
    tools are `<id>__<tool>`.
  - Behaviour change, edge case: the skills and rules indexes now follow
    "raglit is running" rather than "raglit's `search` tool is visible";
    with raglit up and `search` hidden, they still sync (memory and
    surfacing still need `search`).
  - The workspace ingest now goes through the same sink as the other
    indexes, so it adds the serve args' `--home`/`--daemon`/`--embedded` when
    a config sets them (it ignored them before, ingesting into a store the
    server did not read). Same argv for the default args.
  - The web/mobile clients get extension commands from the commands event;
    they were not run. Argument completion for an extension's server
    switch is keyed on its choices having auto and manual.
  - Leftover in the shared raglit daemon: project `dunscratch-ext`
    (workspace, skills, rules, memory indexes) from the live check.
- **Assumptions:** hooks are a list of event names, not a map (the index
  events need nothing more than roles.index); a hook to an extension that
  is not running is not sent; `memory.save` needs a turn.before extension
  too (a note nobody recalls is not worth writing); shell-command hooks
  are not in v1; `dun ext init` not built — the examples dir is the
  template.

## ✅ 20. Shared raglit indexes: watch a directory, attach it to a session (USER, 2026-10-22; done 2026-10-23)

USER's goal: index a directory once and let many sessions use it. raglit's
daemon is shared, raglit's watcher keeps the index fresh, dun only reads it.
Commits `a02a6c1`, `639542a`, `1feb785`. Needs raglit ≥ 87aaf12 (qualified
`proj:idx` reads) and 9af06d1 (MCP ingest `paths` + `forget`); checked
against c410052.

- **What:**
  - `/raglit watch <dir> [--index N]` → `raglit watch <abs dir> <serve args'
    --home/--daemon/--project/--embedded> --index N`, run as the index role's
    CLI (`roles.index.cli`), detached. Relative dir = the workspace's; `~/`
    expanded; N defaults to the dir's name; N is spelled as raglit stores it
    (lowercase, [a-z0-9_-]). The answer is raglit's output plus the attach
    command.
  - `/raglit attach <index>|<project>:<index> [--project|--global]`,
    `/raglit detach …` (default scope: session), bare `/raglit attach` or
    `--list` lists each attachment and its scopes. Stored as `attach` on
    ServerSpec (mcp.json, dun.local.json) and SessionServer (`.mcp.json`
    sidecar); the scopes are a union (`Server.Attach`, per scope in
    `ServerScopes.Attach*`). A bare name is stored as `<own>:name`, own =
    the serve args' `--project`, else `{{project}}`.
  - With another project's index attached, the model's `search` and
    `get_document` calls that name no index, the proactive search
    (ragnotify ExtraArgs) and the short-hit reader (docsBodyFetcher) send
    `<own>:*,<proj>:<idx>,…`. Attachments of the own project send nothing
    (already read by the default). The search tool's description lists the
    attachments. Code: `attach.go`; routing `cmd/dun/extensions.go`
    (extIndexVerb, parseWatch, parseAttach, extIndexCmd).
  - Generic by role, not by id: watch needs `roles.index.cli`, attach/detach
    need `roles.search` (selector only when the search tool takes `index`).
    An extension's own slash command of the same name wins.
  - routeSlash sends `watch|attach|detach` to control `ext` with the args as
    typed (it lowercases server args).
  - raglit's new `forget` MCP tool is in the manifest's `disable` (dun calls
    it through the index role, like `ingest`).
- **Tests:** `attach_test.go` (qualify, union, selector, scope storage and
  sidecar round trip, an attach-only entry is switches-only; through a fake
  MCP server: search/get selector, a named index left alone, description,
  proactive search, fetcher, project + session scopes, detach; WatchDir argv
  with a fake `raglit`); `cmd/dun/commands_test.go` (routing keeps case,
  poly-lsp has no attach; parseWatch/parseAttach). `make test` passes.
- **Live** (tmux `-L dunlive20`, scratch HOME + DUN_HOME + RAGLIT_ROOT, own
  raglit daemon on 127.0.0.1:7499 built from `git archive c410052`, real
  model local-Qwen3.8-27B):
  - Session A (workspace proja): `/raglit watch ../handbook --index Handbook`
    → raglit added the root to `raglitroot/projects/proja/config.json`,
    queued 1 file into `proja__handbook`, watch on.
  - Session B (projb): `/raglit attach proja:handbook` → listed at session
    scope, sidecar `"attach": ["proja:handbook"]`. "How do I restart the
    quillfeather relay…?" → the model called `search(index=projb:*,
    proja:handbook, …)`, got the runbook, answered `--amber-tide`,
    team-osprey.
  - After /clear, "Answer without calling any tools: at what percent …" →
    1 surfaced: kestrel-runbook.md with its text inlined (so the get call
    with the selector worked); the model answered 63% with no tool call.
    Control: `/raglit detach proja:handbook`, /clear, same question → no
    handbook hit; the model said it had no such information.
  - Watcher: a new file in the handbook dir was searchable through
    `proja:handbook` from projb ~20 s later.
  - Index paths: a 0.2 s `ps` poll for raglit CLI processes saw only the two
    workspace ingests (`raglit ingest --project proj? --index proj?-main
    --now`) and the watch. Skills (zz-vellum-binding), rules (`/rules index`,
    docs/deploy-rules.md) and memory (`/remember …`) went through MCP ingest:
    no CLI process, and `raglit search --index skills|rules|memory` found
    each once the queue drained.
- **Known / not verified:**
  - Risk: with another project's index attached, the selector replaces
    raglit's default, so the `shared` namespaces in raglit's project config
    are not read. raglit has no token for "the default set" inside a list
    ("all" in a list becomes `<own>__all`). Decision for USER: ask raglit
    for such a token (icebox).
  - The workspace ingest still runs the CLI (`raglit ingest --now`): its
    sink is chosen before the server starts (icebox).
  - A model call that names an index gets no attachments (by design).
    `/raglit attach --project` writes dun.local.json; an attachment in a
    committed dun.json cannot be removed by a local detach (dun.local.json
    replaces the list only when it has one). `--project`/`--global` were
    tested, not run live. The web/mobile clients were not run.
  - The 0.2 s ps poll could miss a CLI process shorter than that; the
    workspace ingests it did catch take longer.
  - raglit's `manage_index` tool (status, pause, purge …) is visible to the
    model; not changed here.
- **Assumptions:** an attached index is read-only from dun's side; the
  scopes add up (no scope hides another's attachment); `attach` names an
  index, not a namespace (`proj:*` is refused: raglit reads only a named
  index of an undeclared project); watch does not auto-attach.

## ✅ 22. Extension slash commands as a proxy; raglit include/exclude (USER, 2026-10-23)

USER's design: a manifest gives its slash command more commands, each an MCP
call to the extension's own server or a run of its CLI; raglit's
include/exclude are the first. Commits `8688a63` (proxy), `ceed0be`
(raglit). Checked against raglit c410052 (`manage_index` ignore, unignore,
purge with confirm, status; MCP `ingest` paths; `watch DIR --index`).

- **Schema** (`extcmd.go`, `ExtSlashCmd`/`ExtStep` in `extension.go`):
  `slash.commands.<name>` = `{help, tool, args}` | `{help, cli}` |
  `{help, steps: [step…]}`; a step is `tool`+`args` or `cli`, plus
  optional `if: file|dir` and `unless: --flag`. Placeholders `{{arg}}`
  (text, quotes removed, case kept), `{{paths}}` (whole value: JSON array /
  argv elements), `{{path}}` (step runs per path), `{{index}}`/`{{project}}`
  (serve args' `--index`/`--project`, else the workspace's), `{{workspace}}`.
  Unknown placeholders, `{{path}}`+`{{paths}}` in one step, `if` without a
  path placeholder, a cli step without `roles.index.cli` are manifest
  errors that name the field.
- **Run:** text split with shell quoting (`SplitShell`, moved from
  cmd/dun); declared `unless` flags taken out; paths absolute against the
  workspace (`~/` = home); stat decides `if`, every path must be taken by a
  step; a tool step needs the server running (a tool in `disable` works),
  a cli step does not; steps run in order, an error stops the rest; the
  answer is each reply (JSON indented), `› <step>` before each when more
  than one ran. `WatchDir` now uses the same CLI runner (`runIndexCLI`:
  argv + serve args' `--home/--daemon/--project/--embedded`, detached).
- **Routing:** unchanged path — routeSlash → control `ext` `call <id>
  <cmd> <text>` (text as typed) → `RunExtCommand`. Bare `/raglit` lists
  `commands:` with help; the completion choices carry the help.
- **Manifest changes:** `arg` replaced by `args` + `{{arg}}`; per-command
  `description` renamed `help` (the test extension migrated).
- **Tests:** `extcmd_test.go` (field-named errors, input/placeholder
  expansion, a user extension over the fake raglit: dir → watch, file →
  ingest, disabled tool called, ignore→purge order, `--keep`, an error
  stops the rest, no-text and missing-path errors; the raglit manifest's
  five commands against the serve args' index); `cmd/dun`: routing keeps
  case/quotes, completion help, bare /raglit lists commands. `make test`
  passes.
- **Live** (tmux `-L dunlive22`, scratch HOME + DUN_HOME + RAGLIT_ROOT,
  `RAGLIT_DAEMON=http://127.0.0.1:7498`, own daemon built from `git
  archive c410052`, real model local-Qwen3.8-27B, workspace `proj22` on
  `main`, so `{{index}}` = `proj22-main`):
  - Bare `/raglit` listed the 5 commands + watch/attach/detach with help.
  - `/raglit include ../handbook22 ../loose22/loose-note.md` → `raglit
    watch …/handbook22 --index proj22-main` (root added to
    `raglitroot/projects/proj22/config.json`, 2 queued into
    `proj22__proj22-main`, watch on) and MCP `ingest` of the file. CLI
    search found both. A new `gannet.md` in handbook22 was searchable ~13 s
    later; the model answered "cachectl drain --violet-moss" from it.
  - `/raglit add-document ../once22` → ingest; `once.md` searchable; a
    later file in once22 was never indexed (not watched).
  - `/raglit exclude '**/*.log'` → ignore, then purge: `documents: 1,
    sample: [.../build-errors.log]`. CLI search: no matches; the model's two
    searches found nothing. Appending to that .log and a new .log while
    excluded: not enqueued (jobs count unchanged).
  - `/raglit excludes` → ignore `["**/*.log"]`, allow, builtin.
  - `/raglit unexclude '**/*.log'` → ignore `[]`; after one more change in
    the watched dir both .log files were indexed again.
  - `/raglit exclude fresh.log --keep` → fresh.log still searchable;
    `/raglit exclude fresh.log` (no `/`: file name) → purged 1.
- **Known / not verified:**
  - Risk: `{{index}}` is per branch (`<dir>-<branch>`), so an include
    belongs to this worktree's branch index; `dropIndexFor` on worktree
    removal drops it while raglit's config keeps the watch root.
  - `exclude` acts on this workspace's index only (not memory, skills,
    rules, or attached indexes).
  - `unexclude`: whether removed files come back without any change in the
    watched dir was not isolated (one change triggered both back).
  - A typo'd flag (`--kep`) is taken as text (the GLOB). Runs of spaces in the text
    collapse to one, also inside quotes (the TUI and routeSlash split and
    rejoin words before SplitShell sees them).
  - The web/mobile clients were not run.
- **Assumptions:** `arg`/`description` in a user's manifest written in the
  one day since item 21 are rare enough to break (DisallowUnknownFields
  names the field); `help` holds the usage (`FILE|DIR... — …`).

## ✅ 23. ask_user UI: no bounce, draft kept, edit an answer, multi used (USER, 2026-10-23)

USER: "when asked a question and it goes away, we have bounce back", "text is
not restored", "tab or right … should be allowed to edit that answer", and
models never set `multi`. Commits `63db6f8` (bounce), `9071995` (draft),
`a61c8e2` (edit), `2e54e38` (multi).

- **Bounce — cause:** the picker is several rows, the composer one. On close
  the convo pane grew by the difference; pinned to the bottom, every row moved
  down (older rows appeared at the top); the `answered` row and the reply then
  moved them back up. Seen in tmux: top row `line 11` → `line 8` → `line 11`.
  **Fix** (`askpark.go` holdConvo): keep YOffset, set the pane to the new
  height, and pad it with blank rows up to `yoff + height` (refresh,
  `convoHold`); the next rows fill them. The hold ends when the content
  reaches it, at a new width, on reset/session switch. Unpinned readers keep
  their top row too (the Update funnel sees no height change).
- **Draft:** `keepDraft` copies the whole composer (text, cursor, paste
  tokens) when a question opens (or a parked one reopens); `dropAsk` gives
  it back on every close: option, detail, custom, multi done, park,
  interrupt, any turn end, engine restarting/stopped/gave up/gone, /model,
  session switch. `saveUI` saves the kept draft, not a half-typed answer. A
  turn that ends for any reason now closes and settles its question (was:
  interrupt only).
- **Edit:** tab / → on an option open the custom field holding what enter
  would send (option + detail; multi: the checked options with the
  highlighted one), cursor at the end; enter sends, esc back to the options.
  Neither key had a binding on option rows (in the field they still edit).
  Status line: `tab/→ edit`; while typing, `enter send · esc back`.
- **Multi:** the description names "pick several" (include, fix, enable,
  install, remove, test) beside "pick one". `recommended` is string |
  string[] (anyOf); several only with multi; for multi a comma string whose
  parts are all options is split. `AskFunc` takes `[]string`; event `ask`
  adds `recommended_all`, keeps `recommended` = first (mobile reads it). The
  TUI pre-checks every recommended option of a multi question.
- **Tests:** `askpark_test.go` (no move on answer/park/scrolled back; draft
  back for 8 endings + park/reopen; tab/→ edit, esc, detail, multi, custom
  row; multi pre-check), `ask_test.go` (array/string cases, comma split,
  description). `make test` passes.
- **Live** (tmux `-L dunlive23`, scratch HOME + DUN_HOME, repo `proj23`, real
  model local-Qwen3.8-27B): top convo row unchanged over 36 frames after
  answering (full pane); "my draft text" with the caret 4 from the end came
  back after a multi submit; → opened "green" in the field, esc returned, tab
  + " but darker" sent "green but darker". Two prompts that list
  non-exclusive items ("which of them to fix", "these CI checks … ask me")
  got `multi=true` with 3 and 2 recommended, unprompted; "which color" stayed
  single with one recommendation.
- **Not done / known:** space does not toggle in multi (enter toggles, the
  done row submits — as before). The mobile/web client shows only the first
  recommendation and has no multi UI. Blank rows held under a collapsed
  thinking block (holdHeight) were seen; they predate this.

## ✅ 24. Every surfaced RAG item shows where it came from (USER, 2026-10-23)

USER: "who is poisoning or pointlessing my agent" — the TUI showed `skill
docs (0.00)` with no origin; it was a claude.ai-synced skill
(`~/.claude/skills/synced/<bucket>/docs/SKILL.md`). Commits `018c157`
(feature), `206b3af` + `8caaff6` (collapsed row). Checked against raglit
08c1551.

- **What:**
  - `DocHitInfo` / proto `DocHit` (additive, omitempty): `kind` (skill,
    memory, rule, mcp-doc, doc, attached doc), `path` (relative inside the
    workspace or config dir, else `~/`), `index` (raglit's hit field),
    `root` (skills: `user ~/.claude/skills`, `claude.ai sync
    (synced/<bucket>)`, `dun user skills`, `project .claude/skills|.dun/skills`,
    `extension <id>`; `Skill.Root`, `skillRootLabel`), `candidates`,
    `runner_up`, and the display strings `source` and `why`.
  - Why: `only candidate · score 1.4e-05` or `3.0× the runner-up (of 4) ·
    score 3e-05` (%.2g; %.2f printed 0.00).
  - Model text: the why replaces "(N candidates; this one clearly ranked
    first)" and `score=%.2f`; a `Source: …` line follows each item.
  - TUI: one dim source line under each doc title (always, not only when
    open); the why in place of `(%.2f)`; the collapsed row adds the titles
    by kind when ≤ 60 runes, else the kinds and a count of lone candidates
    (`· skill, memory, doc · 3 only candidate`). An older event: `(score
    %.2g)`, no source line, collapsed row unchanged. `-p` mode prints the
    why and source per doc. Web (`internal/dundapi/web`) and mobile notes
    list each doc with why and source.
  - The index: agentkit's `agent.DocHit` has no index field, so the
    proactive finder is dun's own (`indexedFinder`, extension.go: the same
    call as ragnotify.MCPFinder, plus `hitIndexes.note` on the raw JSON;
    a document's first, best hit names its index). "attached doc" = the
    hit's index is a named part of the attach selector.
  - The lone-candidate rule is unchanged; notify.go points at plan item
    "surfacing floor".
- **Tests:** `surfacing_test.go` (Why, source labels for skill/memory/
  attached doc in the UI info and the model's text, skillRootLabel,
  displayPath, hitIndexes + attached); `cmd/dun/tui_test.go` (source line,
  why, collapsed row and its fallback; an older event); mobile
  `events.test.ts` (new and older event; jest run with the main checkout's
  node_modules). `make test` passes.
- **Live** (tmux `-L dunlive23`, scratch HOME + DUN_HOME + RAGLIT_ROOT, own
  daemon on 127.0.0.1:7497 from `git archive` raglit 08c1551, real model
  local-Qwen3.8-27B, workspace proj23; a skill in
  `$HOME/.claude/skills/synced/bkt7f3/quillpress`):
  - Skill turn: `skill quillpress (only candidate · score 1.4e-05)` /
    `skill · ~/.claude/skills/synced/bkt7f3/quillpress/SKILL.md · index
    skills · claude.ai sync (synced/bkt7f3)`; README.md alongside as `doc ·
    README.md · index proj23-main` (only candidate, 2e-06). The session's
    docs entry carried the same `Source:` lines; the model loaded the skill.
  - Memory turn (`/remember`, `/clear`, a rollback question): `memory ·
    <DUN_HOME>/memory/proj23/….md · index memory` (only candidate, 1e-05);
    the path is absolute because the scratch DUN_HOME is not under the
    scratch HOME. The model answered from the note.
  - A third question surfaced 3 lone candidates, two of them wrong (see
    "surfacing floor"); the collapsed row read `· skill, memory, doc · 3
    only candidate`.
- **Known / not verified:**
  - Web and mobile were not run (mobile: jest + tsc on events.ts; the tsc
    run has one unrelated error, `@/gql` not generated in a worktree).
  - rule, mcp-doc and attached-doc kinds and the extension/project skill
    roots were tested, not seen live. Whether raglit tags another
    project's hit as `proj:idx` (what "attached doc" matches) was not seen
    live.
  - The tmux server of the live run exited on its own twice (the dund
    session survived; the TUI re-attached). A reattached TUI showed
    "working…" with no turn running. Not investigated.
  - A replayed session shows the docs entry as a 🔔 notification with the
    model's text, not as a docs block (as before).
- **Assumptions:** `path` relative to the workspace first, then the config
  dir; the doc id is the file path (raglit), so `path` is derived from it;
  `index` is per document, from its best hit.

## mobile/ on TypeScript 7 + oxlint type-aware (2026-10-23, X-LINT-2)

### ✅ mobile/: TS 7.0.2, `oxlint --type-aware`, prettier; eslint removed
- **Gates passed:** (1) `expo config` / `--type public` / with
  `DUN_VERSION_*` identical on TS 6.0.3 and 7.0.2 (only difference: a Node
  `stripTypeScriptTypes` ExperimentalWarning on stderr, so require-utils falls
  back to Node's type stripping on 7.0); tsc, `export:web`, jest pass.
  (2) eslint-plugin-expo (3 rules) and eslint-plugin-react `no-deprecated`
  load through oxlint `jsPlugins` (the react one under alias `react-js`);
  each fires on a bad sample, silent on `mobile/`. (3) eslint-config-expo
  (72 active rules) gave 0 findings on mobile/; oxlint gave 0 after the
  fixes below.
- **Config:** `mobile/.oxlintrc.json` = X-LINT-2 floor + eslint-config-expo
  rules mapped (react-hooks compiler rules as `react/*`). `import/no-unresolved`
  not carried (tsc covers it). `react.version` pinned to 19.2.3 (keep in step).
- **Fixes the type-aware floor forced:** `process.env` is `any` under RN
  typings (`app.config.ts` now pins it); `String(unknown)` in `api/live.ts`
  became `scalar()`; promise-returning callbacks wrapped in `void`; one
  needless `as Item`; `/^closed/.test` -> `startsWith`.
- **Service worker:** `mobile/public/tsconfig.json` (allowJs, checkJs, webworker
  lib) is the nearest tsconfig for `public/sw.js`; tsgolint uses it, so no
  lint override. `pnpm typecheck` also runs `tsc -p public/tsconfig.json`.
  Side effect: expo copies that tsconfig.json into `dist/`.
- **Prettier:** none before; `.prettierrc.json` (single quote, width 120)
  and one `prettier --write` commit.
- **Times (this box):** lint eslint 2.8-4.0 s -> oxlint 1.2 s; tsc 1.7-2.0 s
  -> 0.5 s (TS 6 -> 7).
- **No Makefile/CI target ran mobile lint** (no workflows in the repo);
  `make web` only runs `export:web`. Nothing to update there.

## Shell overflow dir (2026-10-23)

- dun sets `MCPSHELL_TMP_OVERFLOW_DIR=<sessions>/<root>/overflow/<session-id>`
  on an mcpshell shell (`Harness.shellEnv`, `overflowDir`); no session file →
  `$TMPDIR/dun-overflow/<pid>`. A config that sets the var wins.
- mcpshell (≥ `aaff09b`) saves eval output over `--max-output` there and names
  the file in the truncated response; its read()/grep() accept that path.
- Same id across a shell restart and a resume → same dir. `ForgetSession`
  removes it with the session (children's `<id>.subN` dirs too).
- `startServer` now clones the spec's env before appending (it aliased
  `s.Env`).

## ✅ 25. Soft turn budget: tell the model, then cut (USER, 2026-10-23)

USER: "When we have this error [the turn ran past its 30m0s budget],
shouldn't we note this to the agent? instead of killing the flow."
Commit `751c5fd`.

- **Clock** (`cmd/dun/turnclock.go`): two phases on one pausable timer.
  Budget spent → mark `spent` (no cancel), grace starts; grace spent → mark
  `cut`, then cancel with cause `dun.BudgetCut`. ask_user/compaction pause
  either phase. A stale fire after a pause is ignored (generation counter).
  `--timeout-grace`: 0 = 20% of the budget, 2m–10m; negative = none.
- **Notice** (`budget.go`): `NoteBudget` queues it; the Session preparer
  (`withBudgetNotice`, wraps the docs preparer, so never nil) appends it as
  a `budget`-tagged notification at the top of the next round: after a
  running tool's result, never between a call and its result. Dropped at
  the next turn's start (per turn, not per retry attempt). Text: "[dun] This
  turn has used its 30m time budget. dun cuts the turn off in 6m. Finish or
  checkpoint the current step now: commit (a WIP commit if it is not done)
  and note where you are. Then summarize what is done and what is left, and
  end the turn."
- **Cut:** `NoteBudgetCut` records what is running (unanswered tool calls,
  else the streaming model step) before the cancel; `interrupted()` treats
  `BudgetCut` as an interrupt, so the interrupt path records partial reply,
  heals the cut call with the budget as the reason (`withInterrupt` no
  longer says "the user" for a non-user cause) and writes one note: "[dun]
  Your previous turn was cut off after its 30m budget plus 6m grace, during
  the exec call. … Check the state before you continue." `turnErr` maps the
  resulting ErrInterrupted back to the budget error, so the TUI shows the
  error and `kindTurnError` is recorded (still out of context).
- **TUI:** event `notification` kind `budget` → one dim `⏱` line.
- **Sub-agents:** do not use the clock (`subagent.go` runs `h.Ask` on
  `context.Background()`), so no change and no "wrapping up" report.
  Found: `dun.CompactionPause` is global, so a CHILD's compaction pauses
  the parent's turn clock. Not changed.
- **Tests:** `budget_test.go` (notice on the next call once, after the
  tool result, dropped next turn; stale notice dropped; cut during a tool →
  result names the budget, next-turn note once over two turns, turn error
  hidden; cut during a model step), `cmd/dun/turnclock_test.go` (soft then
  cut, paused budget and paused grace do not expire, stop before budget,
  graceFor, turnErr, TUI line). `make test` passes.
- **Live** (scratch HOME + DUN_HOME with a copy of the config, `dun -p` in
  tmux `-L dunlive25`, real model local-Qwen3.8-27B, repo `proj25`):
  - A, `--timeout 60s --timeout-grace 90s`, six `sleep 25` steps: budget
    ran out in step 2; the notice landed after step 2's result; the model
    made a WIP commit ("WIP: steps 1-2 of 6"), wrote done/left and ended
    the turn 66 s into the grace. No cut.
  - B, `--timeout 30s --timeout-grace 10s`, one `sleep 100` exec: cut at
    40 s during the exec; error event names budget + grace; the next turn's
    session log has the note once before the user message; the model
    described the cut correctly.
- **Not verified live:** the TUI line in a real terminal (unit test only).
- **Seen, not changed:** exec cancelled by the cut hands the command off to
  a background job with "STILL RUNNING after 2m30s" (its configured wait,
  not the 12 s it ran).

## ✅ 26. A message typed during an exec wait frees the model (USER, 2026-10-23)

USER: "I'm sitting behind a giant analysis exec, and I want the llm to
investigate its status and also maybe do something else, like validate that
early data indicates it is correct."

- **Mechanism** (`harness.go`): `Say` appends the user message and closes
  `userWaitCh` (under `noteMu`). `userWaiting()` returns that channel, or an
  already-closed one when a user message is queued — so a message typed
  while the model was still writing the call releases the wait too, and no
  message falls between check and wait. No goroutines.
- **exec** (`exec.go`): the grace `select` also waits on `userWaiting()`.
  On wake the job is promoted, the handoff says "Handed over early because
  the user wrote — their message follows.", and `withLiftedQueue` appends
  the message to the same result. The handoff now names the time it
  actually waited (was the configured grace; item 25's "after 2m30s" for
  12 s).
- **Other waits:** `agent wait:true` and `agent_monitor wait:true` stop the
  same way (the child keeps running). Not changed: `exec_monitor` (no
  wait), `ask_user` (excluded), `ask_parent` (child side; a child gets no
  Say), MCP tool calls (no job to hand over).
- **Description:** exec says the user can cut the wait short and that a
  long job should be checked early with exec_monitor.
- **TUI** (`cmd/dun/main.go`): the queued `user` echo is now emitted BEFORE
  `Say`, so the `tool_result` the wake produces always follows it and clears
  the "(queued)" mark. Known gap: the `queued` count event can still land
  after that result (window = exec's onCall → liftQueued); the next
  tool_result or the turn end clears it.
- **Interrupt today** (confirmed): the turn ctx cancels → the grace select
  returns → `settle` promotes; the job's ctx is `context.Background`
  (`newJob`), so it keeps running; the message starts the next turn.
- **Tests:** `exec_wake_test.go` — Say during the wait (handoff < 3 s,
  message in the result, job still running); message queued before the
  call; Say with no waiter queues only; finished command answers inline;
  two concurrent waits both released, message delivered once;
  agent_monitor wait. `make test` passes.
- **Live** (scratch HOME + DUN_HOME with a copy of the config, `dun -p`,
  real model local-Qwen3.8-27B, plain dir `proj26`; run as a background
  shell, not tmux — the sandbox refused tmux): exec of the 120-row loop
  with timeout 600, message typed 10 s in → handoff "STILL RUNNING after
  11s … Handed over early" at the same instant; the model called
  exec_monitor(job:1), read rows 1–19, said ~15% done and the rows are in
  order; job completion notification at 2m0s started a turn that reported
  row 120. The session file holds "[user] how far along…" in the exec
  result.
- **Seen, not changed:** in `-p`, `tool_call` is emitted when the call
  RETURNS (both events share a timestamp), so a client cannot see a long
  exec start; and the `tool_result` event carries the result before the
  lift, without the message.

## ✅ 27. Surfacing floor: similarity floor + delta; coverage without embeddings (USER, 2026-10-23)

USER: a long query with one weak match surfaced a claude.ai skill as "1
surfaced". Item 24 live: one question surfaced 3 lone candidates, 2 wrong.
Two causes: a lone candidate passed with no test, and since raglit's search
went hybrid by default (raglit e66a3df) the score is RRF (≤ ~0.033), so the
1.5× ratio almost never passed with two candidates. Design approved by USER
2026-10-23. Commit `5319618`.

- **What:**
  - Query (`surfaceQuery`, `surfaceQueryWords` = 50): the newest user
    message since the last pass, else (a later pass in the turn) the newest
    entry with text; its first 50 words. Before: all new text joined (the
    reply since the last pass + the message).
  - `mode` omitted (raglit: hybrid on an index with embeddings, bm25 on one
    without; an explicit hybrid/vec skips indexes without).
  - Index kind: the search role takes `list` (manifest
    `"list": "list_indexes"`); `indexMeaning` reads `{indexes:[{name,
    meaning}]}` once per surfacer (rebuilt by applyTools: server restart,
    attach change) and once more per index the list does not name. An
    index still unknown: similarity rule if any of its hits this pass has a
    similarity, else coverage.
  - `hitIndexes.note` keeps per document the best `similarity`, the most
    `matched_terms`, and `query_terms`. Candidates sort by similarity, then
    score (so a skill's best-similarity file represents it).
  - Rule per group × index kind (notify.go relevance note):
    - with embeddings: top sim ≥ `surfaceFloor` 0.68; vs a runner-up with a
      similarity, a lead ≥ `surfaceDelta` 0.01; a lone candidate needs only
      the floor; no similarity → never surfaces; a runner-up with none sets
      no bar.
    - without: top (by score) holds ≥ `coverageFloor` 0.5 of the query's
      words, and ≥ 1.5× the runner-up's score (kept: see the bm25 table).
    - raglit before e66a3df (no `query_terms`): the old ratio rule.
  - Why: `sim 0.71 · 3/4 terms · +0.07 over runner-up (of 4)`, `only
    candidate · sim 0.66 · 2/3 terms`, `runner-up has no sim (of 2)`;
    without embeddings `2/3 terms · 2.0× the runner-up (of 3)`; an older
    raglit or an older event: the item-24 text. proto `DocHit` adds
    `similarity`, `matched_terms`, `query_terms`, `runner_up_sim`
    (omitempty). The TUI, `-p`, web and mobile show the `why` string as
    sent; the source line from item 24 is unchanged.
- **Calibration** (2026-10-23, read-only `GET /search` and `/indexes`, no
  mode, n=8, one index per query; scripts in the session scratchpad
  `floor/`):
  - Indexes: code = `dun__dun-main` and skills = `dun__skills` on the real
    7420 daemon (raglit e66a3df). The 7420 daemon has no dun memory or rules
    index, so memory and rules ran on our own daemon (127.0.0.1:7488, built
    from `git archive e66a3df`, `--embed`, the same config/embedder) with
    copies of real files: memory = the 23 zdx-go Claude Code memory notes
    (`~/.claude/projects/-home-nthalk-local-src-iodesystems-zdx-go/memory`),
    rules = the 14 files of `~/doc/patterns/*.md`. Identity captions had
    finished before the run.
  - Found: `dun__dun-main` holds 2582 documents, 2174 of them under
    `.claude/worktrees/` (Claude Code worktrees, with `mobile/node_modules`).
    The duplicates give identical similarities (delta 0) and the
    node_modules READMEs/CHANGELOGs are most off-topic false positives.
    Not changed here (see icebox).
  - Per document: the best similarity of its fragments; skills one
    candidate per skill. R = right, rel = the project's own plan docs
    describing the feature (counted apart), W = wrong; `wt:` a worktree
    copy. The last column is the chosen rule's outcome.

    | idx | kind | question | top-3: sim terms label | 0.68/0.01 |
    |---|---|---|---|---|
    | code | on | the model keeps calling the same tool with the same arguments in a loop, what stops it | 0.746 8/8 R loopguard.go; 0.696 6/8 rel wt:plan/plan.md; 0.696 6/8 rel wt:plan/plan.md | right |
    | code | on | how does dun stub out an older copy of a file it read again | 0.707 8/8 R rehead.go; 0.703 7/8 rel wt:plan/plan.md; 0.703 7/8 rel wt:plan/plan.md | none (miss) |
    | code | on | what happens when the endpoint refuses the prompt because it is too long | 0.674 6/7 R rescue.go; 0.672 6/7 rel wt:plan/plan.md; 0.672 4/7 rel wt:plan/plan.md | none (miss) |
    | code | on | measure chars per token instead of dividing bytes by four | 0.751 5/8 rel wt:plan/m0/notes/meter-first-round; 0.717 8/8 R tokencal.go; 0.701 5/8 rel wt:plan/plan.md | related |
    | code | on | OAuth for a remote MCP server that answers 401 | 0.822 5/6 rel plan/plan.md; 0.765 6/6 rel wt:plan/plan.md; 0.765 6/6 rel wt:plan/plan.md | related |
    | code | on | how do I kill a runaway background job | 0.716 3/4 rel wt:plan/plan.md; 0.716 3/4 rel wt:plan/plan.md; 0.716 3/4 rel wt:plan/plan.md | none (miss) |
    | code | on | git push in a child process hangs asking for credentials on the terminal | 0.680 5/8 rel plan/plan.md; 0.670 1/8 rel plan/m0/notes/untracked-files-lost; 0.662 4/8 W ship.go | none (miss) |
    | code | on | who writes the commit message for worktree commit | 0.719 4/4 R commit.go; 0.704 4/4 W cmd/dun/servers.go; 0.681 4/4 W cmd/dun/commands.go | right |
    | code | on | how much has this session spent and how close are the rate limits | 0.686 0/7 rel wt:plan/done.md; 0.663 4/7 W proto/events.go; 0.663 3/7 rel plan/plan.md | related |
    | code | on | save a memory note automatically when the user says to remember something | 0.798 6/8 rel wt:plan/plan.md; 0.792 6/8 rel plan/plan.md; 0.702 4/8 rel plan/icebox.md | none (miss) |
    | code | ambig | the answer got cut off because the window had no room left for output | 0.740 8/11 rel wt:plan/plan.md; 0.706 8/11 rel wt:plan/plan.md; 0.706 8/11 rel wt:plan/plan.md | related |
    | code | ambig | make a vague request concrete before the agent acts | 0.673 7/7 R rephrase.go; 0.661 2/7 W node_modules:corsPrivateNetworkPer; 0.654 2/7 W node_modules:corsPreflightAllowPri | none (miss) |
    | code | ambig | accounting for the llm calls that are not the turn itself | 0.735 6/6 R sidecall.go; 0.695 5/6 W harness.go; 0.675 5/6 W wt:README.md | right |
    | code | off | how do I bake sourdough bread with a stiff starter | 0.592 0/5 - tools/ttydrive/README.md; 0.586 0/5 - node_modules:README.md; 0.581 0/5 - wt:tools/ttydrive/README.md | none |
    | code | off | configure nginx as a reverse proxy with letsencrypt certificates | 0.635 1/6 - node_modules:README.md; 0.632 0/6 - node_modules:corsInsecurePrivateNe; 0.628 2/6 - node_modules:README.md | none |
    | code | off | write a SQL migration that adds an index on the users email column | 0.663 1/8 - node_modules:SECURITY.md; 0.650 2/8 - node_modules:SameSiteWarnStrictLax; 0.648 4/8 - plan/plan.md | none |
    | code | off | difference between a mutex and a semaphore in rust | 0.649 0/5 - plan/m0/notes/model-invents-ids.md; 0.649 0/5 - wt:plan/m0/notes/model-invents-ids; 0.649 0/5 - wt:plan/m0/notes/model-invents-ids | none |
    | code | off | plan a three day trip to Lisbon in March | 0.547 1/6 - stories/the-lanterns-of-vell.md; 0.537 1/6 - node_modules:readme.md; 0.536 0/6 - node_modules:SECURITY.md | none |
    | code | off | why does my react component render twice in strict mode | 0.690 4/7 - node_modules:README.md; 0.647 3/7 - node_modules:README.md; 0.642 3/7 - node_modules:README.md | SURFACED |
    | code | off | kubernetes pod stuck in CrashLoopBackOff after a deploy | 0.699 1/6 - wt:plan/plan.md; 0.699 1/6 - plan/plan.md; 0.667 0/6 - plan/m0/notes/mcp-stderr-flake.md | none |
    | code | off | monthly payment on a thirty year mortgage at six percent | 0.555 1/7 - wt:plan/plan.md; 0.555 1/7 - wt:plan/plan.md; 0.555 1/7 - wt:plan/plan.md | none |
    | code | off | tune postgres autovacuum for a write heavy table | 0.625 0/6 - wt:plan/plan.md; 0.625 1/6 - plan/plan.md; 0.611 1/6 - tools/ttydrive/README.md | none |
    | code | off | draft the quarterly sales report for the northeast region | 0.578 0/6 - node_modules:README.md; 0.570 1/6 - wt:plan/done.md; 0.570 1/6 - wt:plan/done.md | none |
    | code | off-ambig | how should I name the variables in this python script | 0.671 1/5 - examples/extensions/poly-lsp/exten; 0.665 1/5 - testdata/extensions/hello/extensio; 0.658 2/5 - node_modules:Readme.md | none |
    | code | off-ambig | set up a github actions workflow that runs the tests | 0.731 5/7 - node_modules:CHANGELOG.md; 0.729 5/7 - node_modules:CHANGELOG.md; 0.726 5/7 - node_modules:CHANGELOG.md | none |
    | skills | on | make a powerpoint deck for the quarterly review | 0.674 3/5 R pptx; 0.672 4/5 W google-workspace | none (miss) |
    | skills | on | fill in the fields of this PDF form | 0.752 4/4 R pdf; 0.650 3/4 W google-workspace | right |
    | skills | on | edit this Word document and keep tracked changes | 0.715 6/6 W google-workspace; 0.671 6/6 W docs; 0.643 5/6 R docx | WRONG |
    | skills | on | build a spreadsheet with formulas from this csv | 0.653 4/4 W google-workspace; 0.619 4/4 R xlsx | none (miss) |
    | skills | on | research the main vector databases and write a report comparing them | 0.681 6/8 R deep-research; 0.665 6/8 W skill-creator; 0.608 5/8 W google-workspace | right |
    | skills | on | set a breakpoint in IntelliJ and step through the code | 0.668 6/6 R ij-debugger; 0.596 4/6 W google-workspace; 0.563 2/6 W pptx | none (miss) |
    | skills | on | write a new skill and test whether it triggers | 0.694 6/6 R skill-creator; 0.632 5/6 W google-workspace; 0.626 5/6 W ij-debugger | right |
    | skills | on | import my memories from ChatGPT | 0.693 4/4 R import-memory; 0.531 2/4 W skill-creator; 0.521 1/4 W computer-use | right |
    | skills | on | add a chart to my google sheet | 0.699 4/5 R google-workspace | right |
    | skills | on | click through the login page in my chrome browser | 0.697 4/7 W built-in-browser; 0.686 6/7 R chrome-browser; 0.667 4/7 W computer-use | WRONG |
    | skills | ambig | give me my brief for today | 0.627 4/5 R morning; 0.583 2/5 W google-workspace; 0.569 3/5 W skill-creator | none (miss) |
    | skills | ambig | open the settings app on my mac and change the wallpaper | 0.625 3/7 R computer-use; 0.612 5/7 W built-in-browser; 0.589 4/7 W google-workspace | none (miss) |
    | skills | off | why does the retry loop back off twice after a 429 | 0.561 3/7 - google-workspace; 0.549 3/7 - ij-debugger; 0.542 3/7 - built-in-browser | none |
    | skills | off | rename this Go function across the package | 0.575 3/5 - google-workspace; 0.560 2/5 - skill-creator; 0.519 3/5 - pptx | none |
    | skills | off | the session store test is flaky on CI | 0.603 1/5 - built-in-browser; 0.567 2/5 - computer-use; 0.566 1/5 - chrome-browser | none |
    | skills | off | how do I bake sourdough bread with a stiff starter | 0.550 0/5 - skill-creator; 0.535 0/5 - google-workspace; 0.493 0/5 - morning | none |
    | skills | off | what does the soft turn budget do when it runs out | 0.577 4/5 - google-workspace; 0.556 2/5 - import-memory; 0.542 2/5 - computer-use | none |
    | skills | off | fix the nil pointer in the harness when the server stops | 0.594 2/6 - google-workspace; 0.579 1/6 - built-in-browser; 0.573 2/6 - ij-debugger | none |
    | skills | off | add a column to the users table and backfill it | 0.676 4/5 - google-workspace | none |
    | skills | off | explain how BM25 scoring works | 0.566 2/4 - computer-use; 0.564 1/4 - google-workspace; 0.561 2/4 - skill-creator | none |
    | skills | off | kubernetes pod stuck in CrashLoopBackOff after a deploy | 0.554 1/6 - morning; 0.550 1/6 - ij-debugger; 0.546 1/6 - google-workspace | none |
    | skills | off | commit this with a conventional commit message | 0.544 2/3 - google-workspace; 0.528 1/3 - ij-debugger; 0.517 1/3 - computer-use | none |
    | skills | off-ambig | summarize this file for me | 0.624 3/3 - import-memory; 0.599 2/3 - skill-creator; 0.581 1/3 - google-workspace | none |
    | skills | off-ambig | write the release notes for this version | 0.643 3/4 - skill-creator; 0.623 2/4 - morning; 0.621 3/4 - google-workspace | none |
    | memory | on | the task I claimed depends on other tasks that are not finished, what should I do | 0.746 7/8 R feedback_blocked_task_handling.md; 0.679 4/8 W reference_decompose_todo_close.md; 0.660 1/8 W project_test_failure_policy.md | right |
    | memory | on | how do I connect to the production database | 0.646 2/3 R reference_prod_db.md; 0.629 1/3 W reference_db_integration_tests.md; 0.595 1/3 W reference_decompose_todo_close.md | none (miss) |
    | memory | on | where do the database integration tests go and how do they get postgres | 0.821 6/7 R reference_db_integration_tests.md; 0.623 2/7 W project_test_failure_policy.md; 0.612 3/7 W feedback_trace_dont_guess.md | right |
    | memory | on | what does bin/ship check before it deploys | 0.697 3/4 R project_ship_flow.md; 0.691 2/4 W project_ship_framework.md; 0.657 1/4 W project_env_agent_direction.md | none (miss) |
    | memory | on | a test was already failing before my change, should I skip it | 0.714 5/8 R project_test_failure_policy.md; 0.638 3/8 W feedback_hil_is_work_avoidance.md; 0.631 2/8 W feedback_blocked_task_handling.md | right |
    | memory | on | dx todo incomplete says the key is wrong | 0.836 4/6 R reference_dx_todo_incomplete_key.m; 0.746 4/6 W feedback_blocked_task_handling.md; 0.686 6/6 W reference_decompose_todo_close.md | right |
    | memory | on | should I mark this as human in the loop only | 0.677 5/5 R feedback_hil_is_work_avoidance.md; 0.608 3/5 W feedback_trace_dont_guess.md; 0.603 1/5 W project_maturity_vines.md | none (miss) |
    | memory | on | how should I close a decompose todo that has no dev done path | 0.827 7/9 R reference_decompose_todo_close.md; 0.741 4/9 W reference_dx_todo_incomplete_key.m; 0.706 4/9 W project_worker_close_codegen.md | right |
    | memory | on | the worker task touched the schema, do I commit the generated code | 0.779 7/7 R project_worker_close_codegen.md; 0.620 3/7 W project_queue_resident_orchestrati; 0.613 3/7 W feedback_blocked_task_handling.md | right |
    | memory | on | why does X happen, should I read a few snippets and guess | 0.744 6/7 R feedback_trace_dont_guess.md; 0.641 0/7 W feedback_blocked_task_handling.md; 0.622 0/7 W project_test_failure_policy.md | right |
    | memory | ambig | who runs deploys and schema dumps for each environment | 0.695 3/6 W project_ship_framework.md; 0.653 3/6 W project_queue_resident_orchestrati; 0.652 6/6 R project_env_agent_direction.md | WRONG |
    | memory | ambig | should orchestration run in a cron job or a watcher | 0.681 5/6 R project_queue_resident_orchestrati; 0.559 2/6 W feedback_hil_is_work_avoidance.md; 0.538 4/6 W project_flow_observability.md | right |
    | memory | off | how do I bake sourdough bread with a stiff starter | 0.514 0/5 - project_self_service_todos.md; 0.500 0/5 - project_maturity_vines.md; 0.498 0/5 - project_test_failure_policy.md | none |
    | memory | off | rename this Go function across the package | 0.571 2/5 - project_env_agent_direction.md; 0.566 1/5 - reference_decompose_todo_close.md; 0.565 1/5 - project_worker_close_codegen.md | none |
    | memory | off | explain how BM25 scoring works | 0.571 2/4 - project_model.md; 0.559 1/4 - project_llm_quality_umbrella.md; 0.545 1/4 - feedback_visibility_first_llm_pipe | none |
    | memory | off | make a powerpoint deck for the quarterly review | 0.612 2/5 - project_llm_quality_umbrella.md; 0.599 2/5 - feedback_visibility_first_llm_pipe; 0.577 1/5 - feedback_hil_is_work_avoidance.md | none |
    | memory | off | why does my react component render twice in strict mode | 0.598 0/7 - feedback_blocked_task_handling.md; 0.575 1/7 - project_test_failure_policy.md; 0.565 0/7 - feedback_trace_dont_guess.md | none |
    | memory | off | plan a three day trip to Lisbon in March | 0.473 0/6 - feedback_hil_is_work_avoidance.md; 0.464 1/6 - project_model.md; 0.462 1/6 - project_flow_observability.md | none |
    | memory | off | configure nginx as a reverse proxy with letsencrypt certificates | 0.579 2/6 - reference_prod_db.md; 0.572 0/6 - project_env_agent_direction.md; 0.570 0/6 - project_llm_quality_umbrella.md | none |
    | memory | off | what does the soft turn budget do when it runs out | 0.560 0/5 - feedback_hil_is_work_avoidance.md; 0.553 0/5 - feedback_blocked_task_handling.md; 0.549 1/5 - feedback_trace_dont_guess.md | none |
    | memory | off | the OAuth callback for the MCP server fails | 0.666 1/5 - project_test_failure_policy.md; 0.634 0/5 - feedback_blocked_task_handling.md; 0.630 1/5 - project_audit_tool_investigation.m | none |
    | memory | off | difference between a mutex and a semaphore in rust | 0.628 0/5 - feedback_hil_is_work_avoidance.md; 0.620 1/5 - project_maturity_vines.md; 0.599 0/5 - feedback_trace_dont_guess.md | none |
    | memory | off-ambig | add a column to the users table and backfill it | 0.619 3/5 - feedback_hil_is_work_avoidance.md; 0.615 1/5 - project_model.md; 0.605 3/5 - project_event_stream.md | none |
    | memory | off-ambig | the deploy failed, what now | 0.656 1/3 - project_ship_framework.md; 0.637 1/3 - project_test_failure_policy.md; 0.633 1/3 - project_env_agent_direction.md | none |
    | rules | on | how should a service layer its config and secrets | 0.694 5/6 R config-secrets.md; 0.622 5/6 W README.md; 0.622 5/6 W standards.md | right |
    | rules | on | which CLI library do we use and what goes in doctor | 0.619 2/6 W observability.md; 0.614 2/6 W README.md; 0.603 3/6 W caveats.md | none (miss) |
    | rules | on | how do I store passkey credentials | 0.623 3/3 R webauthn.md; 0.560 1/3 W config-secrets.md; 0.541 3/3 W caveats.md | none (miss) |
    | rules | on | where should the metrics dashboard live | 0.740 3/4 R observability.md; 0.632 3/4 W standards.md; 0.591 2/4 W uploads.md | right |
    | rules | on | resize uploaded images and serve derivatives | 0.649 5/5 R uploads.md; 0.599 5/5 W standards.md; 0.586 2/5 W observability.md | none (miss) |
    | rules | on | design tokens a page may use | 0.688 4/5 W caveats.md; 0.681 5/5 R design.md; 0.666 4/5 W uploads.md | none (miss) |
    | rules | off | how do I bake sourdough bread with a stiff starter | 0.506 0/5 - caveats.md; 0.472 0/5 - config-secrets.md; 0.469 1/5 - standards.md | none |
    | rules | off | why does the retry loop back off twice after a 429 | 0.593 3/7 - uploads.md; 0.577 3/7 - webauthn.md; 0.575 2/7 - lenny.md | none |
    | rules | off | plan a three day trip to Lisbon in March | 0.489 2/6 - standards.md; 0.477 2/6 - uploads.md; 0.459 1/6 - caveats.md | none |
    | rules | off | explain how BM25 scoring works | 0.570 3/4 - george.md; 0.569 3/4 - standards.md; 0.551 3/4 - lenny.md | none |
    | rules | off | rename this Go function across the package | 0.575 4/5 - uploads.md; 0.554 3/5 - caveats.md; 0.547 2/5 - standards.md | none |
    | rules | off | kubernetes pod stuck in CrashLoopBackOff after a deploy | 0.612 1/6 - uploads.md; 0.596 1/6 - standards.md; 0.585 1/6 - observability.md | none |

  - Sweep (no duplicate collapse, as built; right / related / wrong / miss
    of 43 on-topic, surfaced of 42 off-topic):

    | floor / delta | right | rel | wrong | miss | off-topic surfaced |
    |---|---|---|---|---|---|
    | 0.66 / 0.01 | 21 | 5 | 3 | 14 | 4 |
    | 0.68 / 0.00 | 20 | 6 | 4 | 13 | 3 |
    | **0.68 / 0.01** | **18** | **4** | **3** | **18** | **1** |
    | 0.68 / 0.02 | 16 | 4 | 2 | 21 | 1 |
    | 0.70 / 0.01 | 12 | 3 | 1 | 27 | 0 |

    Per kind at 0.68 / 0.01: code 3 R + 4 rel, 0 W, 1 off-topic (react
    question → a node_modules README, 0.690, lone after duplicates); skills
    5 R, 2 W (Word → google-workspace 0.715; chrome → built-in-browser
    0.697), 0 off; memory 8 R, 1 W (ambiguous), 0 off; rules 2 R, 0 off.
    One floor: the per-kind best floors (memory 0.67, skills 0.68, code
    0.70 on this polluted index) differ by one or two questions each, which
    12 questions per kind do not resolve.
  - A coverage check added to the similarity rule (sim ≥ F AND coverage ≥
    0.4/0.5) changed nothing at delta 0.01. Not added.
  - Without embeddings (the same 85 questions, `mode=bm25`, which is what
    an index without embeddings answers): coverage ≥ 0.5 alone: 36 R, 7 W,
    22 of 42 off-topic surfaced; + the 1.5× ratio: 14 R, 1 W, 6 off-topic
    (code: 0 R — a `_test.go` twin scores within 1.5×); coverage ≥ 0.67
    alone: 33 R, 6 W, 6 off-topic. Built: 0.5 + ratio, as approved (the
    run supports keeping the ratio over dropping it). 0.67 alone is the
    open decision below.
- **Query cap** (8 long messages, 55–92 words, written in the USER's
  style; code + skills on 7420, memory on 7488; the right document's rank
  among the candidates of a clean view, n=20): right ranked first for
  old query (a ~110-word reply + the message) 1/8 (out of the top 20 for
  4/8), whole message 4/8, first 50 words 4/8, first 30 2/8, first 20
  0/8, last 30 3/8, first 30 content words 2/8. Mean similarity of the
  right document: old 0.676, first 50 0.709. Chosen: the first 50 words.
- **Latency per pass** (all 5 `dun__*` indexes of the real daemon, 24
  queries each): old query median 4.88 s (p90 5.55 s; over the
  preparer's 3 s timeout, so those passes failed open), capped query
  median 1.21 s (p90 1.50 s). `list_indexes` 0.11 s, once per surfacer.
  The live session's passes (own daemon, 2 small indexes) were not
  timed.
- **Tests:** `surfacing_test.go`: Why (new and old texts), note's measures
  (best similarity, terms, an older raglit), floor, delta (order by
  similarity, not score), no similarity (lone, and as runner-up), coverage
  for meaning=false with the ratio, an unlisted index both ways, the query
  cap and the newest-entry pass. `extension_test.go`: through a fake MCP
  server, the list is read once per server start and again after a
  restart; coverage surfaces the meaning=false hit and the floor drops the
  meaning=true one. `cmd/dun/tui_test.go`: the new why renders, the
  collapsed row still marks a lone candidate; the item-24 event tests
  (older why, no why) pass unchanged. `make test` passes.
- **Live** (tmux `-L dunlive26`, scratch HOME + DUN_HOME, RAGLIT_ROOT and
  RAGLIT_DAEMON on our own daemon 127.0.0.1:7488 from e66a3df, real model
  local-Qwen3.8-27B; workspace `proj26` = copies of 16 dun source files;
  `~/.claude/skills` = copies of pdf, pptx, docx, xlsx, ij-debugger):
  - "The model keeps calling the same tool … What stops it? Answer in two
    sentences, without calling any tools." → nothing surfaced: loopguard.go
    0.722 vs memtrigger.go 0.723 (the instruction words blur the query);
    the model answered "nothing automatic" (wrong).
  - "the model keeps calling the same tool with the same arguments in a
    loop, what stops it" → `🔎 3 relevant doc(s) · 1 surfaced · doc
    loopguard.go`, opened: `loopguard.go (sim 0.69 · 8/8 terms · +0.03 over
    runner-up (of 2))` / `doc · loopguard.go · index proj26-main`. The
    session's docs entry carried the same why and Source line; the model
    read the file and answered correctly (3 repeats, then ask_user).
  - "What is a good recipe for sourdough bread …" → no docs block.
  - "Fill in the fields of this PDF form for me. Which skill applies? …" →
    `skill pdf (only candidate · sim 0.71 · 8/12 terms)` / `skill ·
    ~/.claude/skills/pdf/FORMS.md · index skills · user ~/.claude/skills`;
    the model named pdf.
  - Captures: `floor/live/capture-ontopic.txt`, `capture-offtopic.txt`,
    `capture-skill.txt` in the session scratchpad.
- **Known / not verified:**
  - The coverage rule (meaning=false) ran only in tests and in the bm25
    sweep: every live index has embeddings.
  - Seen live, not changed: on a small code-only index the similarity of
    a right code file is lower than in the calibration (bgkill.go 0.593
    with 4/4 terms for "How do I kill a runaway background job?"), so code
    questions surface less there.
  - In dun the search spans all the project's indexes merged with
    limit 8, so a group often has 1–2 candidates; the calibration searched
    one index at a time.
  - Web and mobile were not run (they show `why` as sent).
- **Assumptions:** one embedder for all indexes (similarity comparable
  across them; the rule still runs per group); "later passes in a turn"
  = no new user message since the last pass; a document's similarity is
  its best fragment's.
- **Open decision (USER):** the coverage floor for an index without
  embeddings: 0.5 + ratio (built) vs 0.67 alone (more right, 6 vs 1 wrong,
  same off-topic count). One constant and one branch.

## ✅ 28. /repaint and /repaint report (USER, 2026-10-23)

USER: "can we have a /repaint tool, it should capture the screen and the
state for an optional bugreport ... propose to send it through a llm for
anonymization and redaction, with an explanation of the issue. I have seen
some situations where the screen is just mangled." Design approved by USER
2026-10-23. Commits `e0d69ca` (capture + repaint), `57ab77c` (report),
`924b5f6`, `3f5e9ad`, `9c7be69`, `5823629` (fixes from the live run).

- **/repaint** (`cmd/dun/repaint.go`): waits 50 ms (`repaintSettle`, so the
  last View is flushed), captures, then in one `tea.Sequence`: `termReset`
  written to the tty (1049h, SGR 0, `ESC ( B`, DECSTBM reset, origin and
  insert mode off, autowrap on), ClearScrollArea, ClearScreen, WindowSize
  (a missed resize), the /mouse mode, bracketed paste, hidden cursor, then
  `repaintDoneMsg`. Before that: the scroll region is reset and set to
  `wait` (plan returns nil until done, then syncs from scratch), holdConvo
  dropped, mouse highlight cleared, wrap and viewport caches invalidated,
  refresh. Not reset: `maxH` (no-shrink rule) and kitty flags (a stack).
  Ends with "repainted · capture saved to <dir>/".
- **Capture** `$DUN_HOME/bugreports/<YYYYMMDD-HHMMSS>/` (0700, files 0600):
  `expected.{txt,ansi}` = the last View (pointer `shown`), the last h rows,
  each cut to w, the region's `sr.last` laid over rows top..bot;
  `actual.{txt,ansi}` = `tmux capture-pane -p -e -t $TMUX_PANE` (else "not
  available: not in tmux"); `diff.txt` (rows that differ, trailing blanks
  ignored, SO/SI and other C0 bytes dropped from text rows); `state.json`
  (sizes from dun, the tty and `tmux display`, env, kitty, mouse, layout
  heights, YOffset, region, focus, what is open, hold, version, vcs
  commit, session); `events.jsonl` (ring of 200 in the Update funnel, one
  record per run of the same kind with no summary, engine events looked
  through procMsg; text ≤ 80 runes, typed keys named not spelled);
  `README.txt`.
- **/repaint report** (`cmd/dun/repaintreport.go`, `repaint.go`): the
  deterministic pass ($HOME → `~___`, user and host names ≥ 3 chars at word
  boundaries, e-mail, key prefixes/JWT/bearer, any 32+ run mixing upper,
  lower and digits; every placeholder padded with `_` or cut to the width
  it replaces) writes `prered/` and `prompt.txt` (ESC shown as `⎋`). Not
  local (loopback, private, link-local, `localhost`, `.local`/`.lan`/
  `.home.arpa`/`.internal`, dotless name; dun had no such notion) → y/n
  via confirmState. Engine control `repaint` (dir checked to be
  `$DUN_HOME/bugreports/<one level>`) runs a side call kind `repaint` and
  writes `model-output.txt`; the event carries the dir, never content. The
  TUI splits `=== FILE … ===` blocks into `redacted/` (falls back to prered
  per missing file and says so), derives `.txt` and `diff.txt` from the
  redacted `.ansi`, writes `explanation.md`, re-runs the scan (leaks listed
  with file:row) and lists rows whose width changed. Prints `gh issue
  create --web --repo iodesystems/dun --title … --body-file …`; never runs
  gh.
- **Tests:** `cmd/dun/repaint_test.go` (all files with a fake tmux, diff
  rows, no-tmux note, region rows in expected, reset + wait + done line,
  ring bound, tick runs, procMsg, SO/SI), `repaintreport_test.go`
  (redaction of path/user/host/email/token at width, commit id kept,
  placeholder fit, localEndpoint, prompt has no leaks, post-check flags a
  leak and a width change, fences stripped, confirm only for a remote
  model), `repaint_test.go` (dir check, one round trip, side call
  counted). `make test` passes.
- **Live** (tmux `-L dunlive28`, scratch HOME + DUN_HOME with a copy of
  the config, own dund, real model local-Qwen3.8-27B at
  llm.iodesystems.com, plain dir `proj28`): window resized 100x30 → 88x27
  while streaming; junk printf'd to the pane tty at rows 6, 14, 20 →
  `/repaint report` → screen restored (no junk in capture-pane after);
  `diff.txt` "27 and 27 rows, 3 differ" naming exactly rows 6, 14, 20;
  state sizes 88x27 three ways; y/n asked (endpoint is not local), y →
  after the provider queue (several "at capacity" retries) `redacted/` +
  `explanation.md` titled "External tty writes corrupt rendered rows after
  scroll-region clear", naming rows 6, 14, 20 and the three strings as
  outside writes, missed resize ruled out. Post-check: no leaks; 20 width
  warnings in actual.ansi (the model shortened `[text]` runs). `n` at the
  question: prered/ and prompt.txt written, nothing sent.
- **Found live and fixed:** SO/SI from tmux made 5 clean rows differ;
  ticks filled the ring in ~10 s; engine events were all `main.procMsg`;
  a burst "/repaint⏎" captured a View the renderer had not flushed.
- **Known / open:**
  - Junk written BEFORE /repaint is typed is usually gone by capture time:
    typing "/" opens the palette, the pane height changes, the region
    syncs, and SyncScrollArea makes the renderer repaint every row. Seen
    twice live ("0 differ"). The capture shows junk written after the
    palette opened. A key with no layout change (ctrl+l) would capture
    first — USER decision.
  - The model's explanation got the mechanism partly wrong (says the
    scroll region protects rows from outside writes; it does not).
  - The model redacted all conversation text, numbers included, and did
    not keep row widths; the width check reports it.
  - Seen, not changed: the side call's provider retries set the TUI's
    retry banner ("provider at capacity … attempt 5"), and it stayed after
    the report finished, until the next turn.
  - Layout numbers in state.json are taken after the composer was cleared
    (README.txt says so); expected is the frame before.

## ✅ 29. dun↔dun messaging: dun_list and dun_msg (USER, 2026-10-23)

USER design (approved 2026-10-23): one dun session's model sees and messages
the other live dun sessions out of band. Naming and worktree rules added by
USER mid-work. Commits `6680641` (proto), `bd6d3fe` (dund), `35e6d13`
(tools + queue), `39bc48b` (worktree naming), `a61d893` (engine wiring, TUI).

- **Transport** (`proto/peer.go`): the engine's stdio, the one channel it
  has to dund. Engine → dund `peer_request{rid, op list|msg, to, text,
  wake}` (an event dund does NOT log: plumbing); dund → engine
  `peer_reply{rid, error, sessions | id, name, busy, started}`; dund →
  target `peer{from, from_name, text, wake}`. No second transport.
- **dund** (`internal/dund/peer.go`): answered on its own goroutine, never
  the event reader's (delivery writes to another engine's stdin; two
  readers waiting on each other's engines would deadlock). Owner's sessions
  only (`Get` = no such session for others), loaded only (`sendLoaded`
  never starts an engine; a registered-but-stopped target is "not loaded"),
  self refused, `peerRateMax` 6 per `peerRateWindow` 1 min per (sender,
  target), sliding. Busy = `inTurn || asking` from dund's own event
  tracking. Task = state sidecar status, else goal, else last user echo.
- **Names** (`nameSessions`): `<folder>:<hhmmss>` — folder = basename of
  the workspace dund recorded (the source checkout, also for a `--worktree`
  session, so never `dun-worktree-XXXX`); same basename, different paths →
  each takes the fewest parent components that tell it from the others
  (`src/dun` vs `old/dun`, `x/src/dun` vs `b/src/dun`); a name still
  shared → the id. hhmmss = id past its date (`-2` suffix kept). `to:`
  takes name or id; unknown/ambiguous lists the candidates.
- **Worktree column**: dund tracks `proto.Workspace`; a path other than the
  workspace = a worktree (`wt <branch>` in the listing); the in-place one
  `--ship` reports is not. `/worktree new` now emits `Workspace` too (it
  emitted only a control line). In-place branch = `.git/HEAD` read
  directly (worktree `.git` file followed).
- **Engine** (`peers.go`, `cmd/dun/peers.go`): `Config.Peers` (a
  `PeerLink`) set only when `DUN_DUND` is in the env — dund's ExecSpawner
  sets it, the engine unsets it at start so a dun run in exec does not
  inherit it. Standalone: tools absent (matches ask/exec/mount: optional
  tools are left out, not erroring). Children: `childConfig` drops it.
  Reply wait 15 s (`peerTimeout`).
- **Delivery** (`Harness.PeerMessage`): queued kinds `queuedPeer`
  (causesTurn) and `queuedPeerQuiet` (wake:false; rides the next turn via
  flushSilent). Text = `PeerLabel` ("[dun msg from <name> (session <id>)]
  … not from your user … cannot answer a question you asked your user …
  Reply with dun_msg(to:"<id>")"), prefix `[peer] ` when lifted. wake frees
  `userWaitCh` (exec grace, agent/agent_monitor waits) and pokes `h.wake`
  so an idle driver runs `continueTurn`. The wait-release texts now say "a
  message arrived (the user wrote, or another dun session did)".
  `Pending`/`Queued` count by `causesTurn` (was: not aside).
- **ask_user**: a peer frame goes to `peerFn`, never `in.answers`; the
  question stays open and the message lifts into the result after it.
- **TUI**: `notification{kind:"peer"}` → dim `✉ dun msg from <name> (<id>):
  <text>`; `dun_msg` renderer (to + text, refusal in red); `dun_list`
  generic.
- **Tests**: `internal/dund/peer_test.go` (owner/loaded filter, you marker,
  busy row, naming incl. collisions and the worktree case, git HEAD, busy vs
  idle delivery, refusals, rate limit, the stdio round trip, nothing
  logged), `peers_test.go` (tools only for a root with a link, child has
  none, rendering, wake default, busy lift + wait freed, idle wake,
  wake:false, ask not answered), `cmd/dun/peers_test.go` (env unset, frame
  routing, link round trip, timeout, renderer). `make test` passes.
- **Assumptions:** reply's busy/started are dund's view at delivery (the
  engine decides from its own `turnActive`; a turn ending in between makes
  "busy" stale — the message still arrives); wake:false does not free a
  wait; the mobile client shows a peer notification with its default
  notification style.
- **Not verified:** never run live between two real sessions under a real
  dund.
- Out of scope → icebox: Claude Code sessions (`/integration enable
  claude`).

## ✅ 31. Scrolling moved the offset but not the text (USER, 2026-10-23)

USER report: "sometimes when I'm scrolling, I can scroll below the window,
and the text doesn't move" — the scroll state changed (overlay, status),
the conversation rows did not.

- **Cause 1 — a clear that overtook its sync** (`scrollregion.go`
  `disable`). Every region command runs in its own goroutine, and
  minRegionGap serialised plan()'s commands — but not `disable`, called
  straight from `planScrollRegion` when a mode wants the rows back (the
  band on a selection, a mouse highlight, the pager…). A sync and a
  disable inside 25ms could land clear-first: the renderer kept ignoring
  the region's rows while the model had let go of them. View drew the
  pane, nothing painted it, and every scroll moved the offset and the rows
  outside the pane but not the text — until the band went away and a
  re-sync repainted. The reverse (disable, then a sync inside the gap)
  could land sync-first and leave the renderer painting the blanked rows.
  Fix: `disable(now)` waits its turn — the model lets go at once (View
  draws the real rows; the renderer still skips them, harmless), the clear
  is OWED and sent by one retry when the gap is up; a sync meanwhile
  supersedes it. `disable` stamps `lastAt`, so the next sync waits too.
- **Cause 2 — region planned before the height sync** (`tui.go` Update).
  `planScrollRegion` ran before the block that anchors the bottom across a
  height change (composer grows/shrinks while scrolled up). The region
  showed rows from the old offset while View blanked rows for the new one
  — one row off per composer line, until the next message replanned. Fix:
  plan last, after the height sync, syncPager, and the pulse.
- **Tests:** `TestScrollRegionDisableWaitsForTheGap`,
  `TestScrollRegionSyncWaitsForADisable` (a `regionRace` harness delivers
  commands issued inside the gap later-first; both fail on the old code),
  `TestScrollRegionFollowsAHeightChange` (region rows == View's rows after
  each step of pgup / ctrl+j / backspace / wheel; fails on the old order).
  `make test` passes.
- **Assumption:** the race window is a goroutine handoff, well inside
  minRegionGap (the existing serialisation rests on the same assumption).
  A band/highlight that starts inside the gap now reaches the screen when
  the owed clear lands, ≤25ms later.
- **Not verified live:** the race was not caught on a real terminal; it is
  reproduced by the harness's worst-case ordering. If it recurs: `/repaint
  report` while it is frozen — expected vs actual rows, plus the region
  state in the capture (`live`, `held`, top/bot), say which side diverged:
  rows frozen with `live:false` = the renderer still ignoring them.

## ✅ 30. Images in the composer; vision probe; vision→text fallback (USER, 2026-10-23)

USER scope (approved 2026-10-23): images into the TUI composer, sent as
content parts, kept out of the session log, and a per-model vision probe
with a vision→text fallback.

- **Composer** (`cmd/dun/imageinput.go`, `multiline_input.go`): an image is
  a token like a paste token (`images` side table beside `pastes`, same
  private-use runes), drawn and valued as its label `[image N: name W×H]`.
  In: ctrl+v (was unbound; a `tea.Cmd` runs `wl-paste --type image/png`,
  then `xclip … -t image/png -o`; PNG magic checked; no image → nothing, as
  before) — the bytes go to `$DUN_HOME/images/<sha256>.png`; or a paste
  (bracketed, or a multi-rune KeyRunes) whose whole text is an existing
  png/jpg/jpeg/gif/webp (quotes, `file://`, `~/`, `\ ` escapes) that
  decodes — else it is text. Labels are numbered per TUI process and
  remembered (`imageRegistry`, a pointer shared by the model's copies), so
  `SetValue` turns a known label back into its token: history recall, the
  ask draft (`keepDraft` copies the struct), a refused draft. ctrl+o leaves
  an image alone.
- **Wire** (`proto`): `User.Images []Image{path, label}`; event
  `image_refused{message, content, images}` then `done`. dund passes it
  through (it re-encodes `proto.User`).
- **Engine** (`cmd/dun/main.go`, `images.go`): `users` carries `userMsg`
  (text + images). A message with images during a turn is HELD
  (`inputStream.hold`, a notification says so) and starts the next turn —
  never lifted into a tool result. Not rephrased (a rewrite moves labels).
  `turn` refuses up front when the record says no vision and no
  `vision_model` is set.
- **Harness** (`images.go`): `AskImages` → `imageParts`: text up to and
  including each label, then the image (`llm.ImageData`); `prepareImage`
  scales to ≤1568 px long side (x/image CatmullRom, never up), PNG/JPEG
  that need no scaling are sent as their own bytes, others re-encode PNG,
  a PNG > 1 MiB after that → JPEG q85 on white. Probe: the first image to
  a model (key `VisionKey(url, model)`) records `vision=true` on a turn that
  went through, `vision=false` on a refusal (`refusesImages`: 4xx not 429
  mentioning image/vision/multimodal/content type; 5xx with llama.cpp's
  "image input is not supported"/"mmproj"), in `$DUN_HOME/vision.json`;
  later sends use the record. No vision → each image described by the
  `vision_model` side call (kind "vision"; prompt: describe for an
  assistant who cannot see it, user text as context), label becomes
  `[image N: name W×H — description: …]` and the entry is that text (so a
  resume does not describe again). Refused after the probe → the entry is
  dropped and `*ImageRefusedError` says how to set `vision_model`.
  `runTurn` does not turn-retry a refusal (a 5xx one would be).
- **Log** (`store.go`): `flushLocked` writes each data-URI image part to
  the session folder's `blobs/<sha256>.<ext>` and keeps `\x00dun-image:` +
  name; load rebuilds the data URI; a missing file → a text part
  "[dun: image missing from the session — …]".
- **Config** (`cmd/dun/config.go`): `vision_model` (a model id, on the
  active provider) and optional `vision_provider` (a logged-in provider's
  name) in `$DUN_HOME/config.json`.
- **Tests:** `images_test.go` (downscale + no upscale + JPEG for a noisy
  PNG, part order, no base64 in the log + resume rebuilds + missing blob,
  `refusesImages` table, probe recorded true, refusal → described text sent
  and recorded false and the next image not re-probed, no describer →
  refused and not in the session), `cmd/dun/imageinput_test.go` (path
  paste forms → token, non-images stay text, token vs paste behaviour and
  recall, clipboard through an injected `clipboardRun`, ctrl+v → cmd,
  stdin images in + `image_refused`/`done` out strictly, TUI refusal keeps
  the draft with its token + notice), `proto_compat_test` (send with
  images). `make test` passes.
- **Assumptions:** 1568 px and the 1 MiB PNG→JPEG cut (picked, not
  measured); `vision_model` is on the active provider unless
  `vision_provider` names one; the record key is provider URL + model id
  (no provider name); clipboard images stay in `$DUN_HOME/images/` (never
  cleaned); a mid-turn image message waits for the turn instead of being
  lifted; an image in a sub-agent tell is refused; a 4xx that mentions an
  image for another reason marks the model no-vision (delete its
  `vision.json` entry to probe again); agentkit's client still retries a
  llama.cpp 5xx refusal itself before dun sees it; the probe-refusal path
  has already echoed the message to clients (the up-front refusal has not).
- **Risks:** images are not in agentkit's context estimate
  (`estimateTokens` counts `Content`), so the prompt meter puts their
  tokens in overhead; every later request re-sends an image while its
  entry is in the window.
- **Not verified:** a real clipboard (Wayland/X11), a real vision provider,
  a real refusal body from a provider.
- Out of scope → icebox: `img_read` tool; mobile image upload.

## ✅ 32. Focus by tab only; ctrl+tab types a tab; the job view (USER, 2026-10-23)

USER: "the movement between panes is incredibly obnoxious … arrow key back
shouldn't go between the focus groups. it should be tab, and ctrl+tab should
be the insert raw tab … job display should have the similar to multistep
tool+think". Commits `1967864` (focus), `38fc768` (ctrl+tab), `cadb19e`
(job view).

- **Arrows never change the zone.** Removed: ← at the composer's front →
  convo; → on a convo block with nothing to descend → composer; ← in convo
  with nothing to ascend → next zone; ← in the activity table → fold, then
  composer. At every edge the arrow is a no-op. Kept: → on an empty
  composer accepts the suggestion; ← leaves an agent's scope (from the
  composer's front, the convo, and now the strip); ↑/↓ at the composer's
  first/last row still scroll the conversation (not a zone move). esc
  unchanged: from convo or activity it still goes home to the composer.
- **Activity zone:** focusing it opens the table with ➤ on a row (was: the
  collapsed line, first → only unfolded); leaving folds it. Skipped by tab
  when `actRows()` is empty; finished/failed/killed jobs stay listed, so
  they keep it reachable.
- **ctrl+tab** (`keyfilter.go`): CSI 9;5u (kitty) and CSI 27;5;9~
  (modifyOtherKeys) → `ctrlTabRune` U+FDD0 → `cleanInput` makes it `\t`.
  bubbletea v1 swallowed both. Also CSI 9;2u / CSI 27;2;9~ → CSI Z: under
  the kitty protocol shift+tab was swallowed too. Legacy terminals send TAB
  for ctrl+tab — indistinguishable; dun only gets the CSI forms when the
  kitty probe succeeds (`kittyAppMark` is sent only then).
- **Pasted tabs** already worked: bracketed paste keeps `\t`, `cleanInput`
  keeps it, the layout draws it to 4-col stops. Now pinned by tests (incl.
  through bubbletea's real parser).
- **Job view** (`jobview.go`, replaces the inspector for jobs): convo-pane
  view like the pager; command item on top (open by default; long ones
  open to "command" + wrapped body, capped at a third of the pane), output
  tail panel (follows while running; ↑ at its top selects the command;
  pgup/pgdn g/G wheel; / ? n N search, less's start rule; esc clears a
  search, then leaves). Log read incrementally from `jobRow.log` every
  500ms (gen-tagged timer), last 256 KB, ANSI stripped, last `\r` wins,
  tabs to 8. `jobDetail` removed.
- **Tests changed** (intended behaviour change): `TestTUI_ArrowNav`,
  `TestTUI_RightDescendsAFoldedBlock`, `TestTUI_RightStopsWhereTheBlockEnds`,
  `TestYank`, `TestTUI_XKillsSelectedJobAfterConfirm`,
  `enterAgentScopeByKeys`/`TestActivity_LeavingAgentScope`; replaced:
  `TestMultilineInput_LeftAtFrontHopsToConvo` → `…StaysInInput`,
  `TestActivity_RightDescendsLeftAscends` → `…TabOpensTheTableWithAHighlight`,
  `TestActivity_LeftWalksBackThroughTheZones` →
  `TestFocus_ArrowsStopAtEveryEdge`, `TestActivity_LeftWrapsHomeWithNoActivity`
  → `TestActivity_OnlyDeadJobsIsStillReachable`,
  `TestLeftFromInputSelectsTheNewest` → `TestTabFromInputSelectsTheNewest`.
- **Not verified:** not run live in a terminal; ctrl+tab not tried on a
  real kitty/ghostty/tmux; the job view not watched on a real running job.

## ✅ 33. Bubble Tea v2; the hand-built scroll region deleted (USER, 2026-10-23)

USER approved ("rip and tear until it is done"). v1's line renderer is why
dun grew its own scroll region (item 31's bug class: two owners of what the
screen shows, Cmds racing). v2's renderer is a cell diff with hard-scroll
optimisation (ultraviolet `TerminalRenderer`, on in fullscreen), so the
region goes instead of being ported.

- **Deps:** charm.land/bubbletea/v2 v2.1.0, bubbles/v2 v2.2.1, lipgloss/v2
  v2.0.6, glamour/v2 v2.0.1; termenv dropped (band: `colorprofile.Detect`;
  render: `lipgloss.HasDarkBackground`).
- **Deleted** (lines): `scrollregion.go` 253 + test 251 (DECSTBM region,
  `minRegionGap`, held/owed rows, `blankRegion`), `planScrollRegion` and the
  region fields in /repaint's state.json (`scroll_region`, `kitty_keyboard`);
  `keyfilter.go` 172 + test 92 (byte rewrites ahead of v1's parser, our own
  raw mode); `kitty.go` 151 + test 132 (probe/enable/disable: v2 pushes
  kitty flags=1 and modifyOtherKeys=2 itself and pops at exit);
  `sgrpack.go` 194 + test 64 and the `packed` cache (measured: per-word
  styled rows 2807 B in / packed 527 B in, 529 B out of the renderer
  either way); termios test helpers 12; the v1 burst split in Update (v2
  decodes one grapheme per key). Net for the branch (code + tests, no docs):
  72 files, +1272 / −2201; non-test code +420 / −1235.
- **Kept, smaller:** `normalizeKey` (tui.go) folds shift+enter to ctrl+j —
  v2 decodes CSI 13;2u and CSI 27;2;13~ as shift+enter, but SS3 M as keypad
  enter, which v2 names "enter" and would SEND. ctrl+tab is v2's own
  "ctrl+tab" key; the composer inserts `\t`.
- **Paste:** `tea.PasteMsg` → `paste()` routes it to the field that has the
  keys (composer, ask custom answer, search, model/provider/MCP forms,
  inspector and job-view search); `multilineInput.HandlePaste` keeps image
  paths and paste tokens. A paste is never a command (v1 got this from
  `[...]`-wrapped String()).
- **/repaint:** `tea.Raw(termReset + modeReset)`, `tea.ClearScreen`,
  `tea.RequestWindowSize`. Mode re-assert is written with the reset: the
  renderer only sends a mode when View changes it.
- **View fields:** alt screen, mouse mode from `/mouse` (`mouseModeOf`).
- **Tests:** 470 → 450. Deleted with their code: 8 `TestScrollRegion*` /
  `TestBlankRegion*`, 5 `TestKeyFilter_*`, 5 `TestProbeKitty*`, 3
  `TestPackSGR*`, `TestKeyFilter_RewritesCtrlTabAndKittyShiftTab`,
  `TestRepaintExpectedHasRegionRows`. Rewritten: `TestCtrlTab_ThroughTheRealParser`
  → `TestKeys_ThroughTheRealDecoder` (raw bytes through v2's decoder: both
  ctrl+tab spellings, kitty + legacy shift+tab, a paste with a tab, the
  three shift+enter spellings); `TestBurstOfRunes_KeyNamedWordIsTyped`
  (through the real decoder); `TestRepaintResetsAndReports` (no region);
  `TestServerHeader_HotKeys` (no Ascii-profile half: lipgloss v2 is pure);
  `TestMultilineInput_PastedKeyNameIsText` (HandlePaste); 4 tests strip ANSI
  where v1's Ascii test profile had stripped it. New:
  `TestRepaintExpectedIsTheFrame`, `TestRepaintModeReset`,
  `TestScreen_ScrollingMatchesView` (screen_test.go: the real program and
  renderer into a buffer, replayed through a small emulator that fails on
  any unknown CSI; checks ↑ one row, a stream, a taller composer; a
  one-row scroll is 113 B against 808 B for the 80×15 frame — DECSTBM + RI).
- **Smoke:** `dun --replay` of a synthetic trace in tmux (100×30, temp
  DUN_HOME, no engine): frames, ↑ scroll and the scroll overlay correct;
  with `extended-keys always` + `csi-u`, S-Enter is a newline and C-Tab a
  tab; ctrl+c twice exits to the shell screen; stderr empty.
- **Risks / not verified:** no real kitty/ghostty/foot/Termux, no
  `dun -serve` (xterm.js answering v2's kitty/modifyOtherKeys requests), no
  dund-attached run, mouse not exercised live; an unbracketed paste of an
  image path is now typed keys, not an image (v2 always turns bracketed
  paste on); under NO_COLOR (Ascii profile) bold/underline now show (v1
  dropped them); OSC 52 copy still writes to stdout from a Cmd goroutine,
  as in v1 (`tea.SetClipboard`/`tea.Raw` would order it with frames).

## ✅ 35. Onboarding fixes from run 1 of the walk (USER, 2026-10-23 — "get them all happy")

- **Built** (commits 95f40c5, 6480a1c, d31d453, 7252b9a): first screen says
  "not logged in — /login to connect to an AI service" (accent) and no tool
  dump; off-servers hint grey, one line, once per TUI; a question asked
  before login is held (not sent) and put back in the composer after the
  model pick; setup errors plain (setupMissing unwraps "agent: chat:");
  picker: enter = use + save default, alt+enter = this session, no
  checkbox, confirmation line after the restart, no "resumed" marker, model
  in the header, no provider column with one service; one form layout
  (formRow), form keys on the status line; /mcp add: no mode row, enter on
  server adds, optional rows behind tab; panels end at a word with "…",
  status lines drop whole hints (most-used first); palette by group
  (setup → … → maintenance), descriptions in one column, args on the row
  below; "running" lines green, orange only for attention; /mcp add result
  replaces "adding …"; "1 tool". Tests: onboarding_test.go (new), and the
  intended old-string updates in mcpform_test, persist_test, model_test,
  servers_test, builtin_cmds_test, tui_test.
- **Walk:** 10 steps now (the held question is step 07; last step asks
  about the notes; stub answers it). Run dir:
  `tmp/ux-run-onboarding-20261023-204551`.
- **next:** parent runs Lenny/George/Ives on the new run dir; then the
  onboarding demo recording.
- **risks:** a message is held only once the engine's settings event said
  it has no URL (before that, it is sent and the engine's plain error
  shows); held images are not held (sent, engine errors); /mcp add for an
  OAuth URL no longer walks into the client row — enter adds without
  OAuth, tab reaches it; `/model <name>` (typed) still switches for this
  session only, unlike the picker's enter; commands event order changed
  (dun's own before extensions') — the web client sees that order;
  checked at 80 and 100 columns only.
- **blocking decisions (USER):** (a) the half-width pink segment on the
  divider is the focus cue (tui.go divider()); Ives reads it as
  decoration — keep, make full-width, or change? Left as is. (b) should a
  typed `/model <name>` also save the default, like the picker?
- **assumptions:** "AI service" replaces "provider" on first-run surfaces
  only (usage strings and /logout still say provider); the off-servers
  hint stays (grey) because its recorded reason is "an agent that silently
  lacks code navigation".
- **optional extensions:** status line "not logged in" could carry
  "/login"; the header's workspace path could shorten to the folder name.

## ✅ 36. Onboarding fixes from run 2 of the walk (USER, 2026-10-23)

- **Owner decisions implemented:** (1) splash at the top of a fresh
  session (session event `resumed: 0` and no history): `dun <build>`, then
  label/value rows — workspace (~), model on its service, tools, servers
  (running green, failed red), off extras with their `/x on` commands
  (cmd/dun/splash.go, `statusSections`, so /status can grow sections). The
  engine's ready hint no longer has the "off:" line; `proto.ServerState`
  gained `Slash`. A model pick (engine restart) puts the splash back at the
  top with the new model; /new shows one again. (2) `/status` (owner
  renamed it from /session): the splash appended client-side — every TUI
  `m.append` entry is client-only (the engine owns the session log), so
  nothing new was needed beyond a `splash` flag on convoEntry; session
  group. (3) header `dun · <folder> [⎇ branch] · <model>`. (4) divider:
  focused half `━` in 212, other `─` dim; activity focus lights the upper
  half (it lit the input half before); no pane dimming. (5) typed
  `/model [provider] <name>` and `/model use` save the default;
  `--session` = this session only (README).
- **Findings fixed:** held question → "kept your question — it comes back
  after /login" in place of the not-logged-in line; stub LLM calls
  `notes__project_notes` then answers from the result; model list chat
  first, non-chat by name heuristic (`modelKind`: embed, rerank, vision,
  whisper/tts/transcribe, moderation, dall-e — /v1/models has no kinds)
  dim + labelled, cursor on running model → saved default → first chat
  model, no count when all fit, one indent (`listIndent`); palette and
  /help command names bold neutral (were green); not-logged-in and
  no-model lines orange (were pink); status notices one shape `✓ what ·
  detail` (`okNotice`: login, model pick, `notes added · running · 1
  tool`); /mcp and /ext descriptions shortened (fit at 100); both forms:
  fields, blank row, notes (presets moved below), status line via
  `formKeys` with shift+tab back.
- **Tests:** new cmd/dun/splash_test.go (splash fresh only, kept through a
  pick, /status client-side, header, divider, typed /model, model list,
  palette, forms) and tools/ux/stub_test.go. Old strings updated:
  onboarding_test (held line, `·` notes, `✓` notice), persist_test
  (`TestReadyNamesNoTools`: header has no tool count), servers_test
  (`TestServerHint_NamesWhatIsOffAndHow` →
  `TestServerHint_NamesWhatFailedNotWhatIsOff`), tui_test
  (`TestTUI_ScreenDump` looks for the header, not "1 tool").
- **Walk:** waits changed — step 02 on "kept your question", step 09 on
  "notes added" ("running" now matches the splash). Run dir:
  `tmp/ux-run-onboarding-20261023-212154` (worktree agent-ae92fafa0ca5f6ec1). Every PNG looked at.
- **Ives brief:** "Owner decisions" — the half-line focus cue is the
  owner's; never recommend dimming a pane for focus.
- **Open / for the owner:** the notes call renders as `⚙ 1 step —
  notes__project_notes×1` / `✓ ⚙ notes__project_notes()  → …`: the
  internal tool name, a ×1 and two marks — not redesigned here. Green is
  still the colour of tool calls (⚙ lines, toolrun.go) as well as
  success. The form titles and the model picker title stay pink (bold),
  read as "the active surface". The splash says what was true at start;
  the servers row does not update after /mcp add (/status does). The
  "1-10 of 36 commands · /login [name]" palette footer is unchanged. The
  heavy rule is 2 px against 1 px in the PNG renderer at 10 px cells.
- **Not verified:** a real terminal, light theme, dund-attached reuse of an
  idle session (should replay history → no splash).

## ✅ 37. Onboarding fixes from run 3 of the walk (USER, 2026-10-23)

- **Owner's standing rule — no bounce** (and the clarification): on a
  change that adds rows the view keeps its top row (pinned follows the
  bottom); on a change that takes rows away the view keeps its top row and
  blank rows fill in below, only to the bottom of the view as it was, so
  the furthest scroll is where the view is (`holdShrink`, refresh). A pane
  that grows because a panel/form/palette closed keeps its top row
  (`holdTop`, generalized from askpark's holdConvo); a terminal resize (the
  phone keyboard) still anchors the bottom. Holds end when content reaches
  them, at a new width, and at reset/restart/new session/repaint
  (`dropHold`, which also skips the next refresh's shrink check).
- **Splash (1):** folder, model, servers, extras — always 4 rows, each cut
  to the width, drawn from live state at every refresh (`liveSplash`); a
  server event refreshes it. No build, wordmark or tool count (those are in
  `/status`: build, folder, model, tools, servers, extras — a snapshot).
- **Extras (2):** "code navigation (poly-lsp) and project search (raglit)
  are off · /help poly-lsp for more" (on ones say "is on"; "for more"
  drops first when narrow). `/help <command>`: description, the command's
  long help (new `proto.CommandSpec.Help` / manifest `slash.help`), usage
  and choices. raglit / poly-lsp manifests: plain description + help with
  on / auto / off / manual.
- **Tool calls (3):** `toolLabel` (server__tool → "server · tool words",
  `_` → space) in callText, callFull, the group line and the summary. A
  one-call run is one line (`✓ notes · project notes  → …`), no summary,
  no `()`, no `×1`; 2+ steps keep the ⚙ header, `×N` only for N>1. The
  call stays in the turn, expandable (enter → pager) as before. Model-
  facing names, logs, inspector and copy text keep the real name.
- **/login (4):** address first, then name (filled in from the host as the
  address is typed — api.example.com → example, IP/localhost → local —
  until the name is edited), then key. Re-login steps over the fixed name.
- **Smaller (5–10):** `appendNotice` — blank row above and below every ✓
  notice (and the /mcp add "adding…" line it replaces); /mcp add form has
  no presets line, presets in `/help mcp`; both forms end
  "the same name again replaces it" (`sameNameNote`); not logged in → the
  status line is "enter send · / commands · ctrl+c quit"; logged in →
  "ready · enter send · alt+enter newline · tab scroll · ctrl+↑/↓ history ·
  ctrl+c quit"; the restored question: status line "your earlier question
  · enter asks it · or change it first" while unchanged (`restoredMsg`);
  orange only for attention — `replyLine` (engine replies, /mcp tools,
  server messages) is plain unless it says failed/error/usage…; kept-
  question line, /mouse, /trace dim; /mcp, /docker and the other "bare"
  descriptions in plain words.
- **Owner decisions folded in:** process visibility — calls stay in the
  turn, one readable line each (ives.md: never recommend hiding or
  demoting how an answer was produced). Question band: text in the
  answer's foreground (`answerFG` = glamour dark document colour 252,
  8.56:1 on 236), fill capped at 100 columns (`bandCap`); khaki 144 gone.
- **Tests:** new cmd/dun/bounce_test.go (live splash ×3 scroll positions,
  block close at bottom + scrolled up + scroll-back bound + refill, one
  tool call ×2, notice while scrolled up, form close), onboarding4_test.go
  (nameFromURL, login prefill, form words, /help, plain descriptions,
  not-logged-in once, restored question, notice spacing, replyLine, band
  colour + fill at 48/100/180). Both mechanisms were checked to fail with
  their fix disabled. Old strings updated: splash_test (rows, /status),
  persist_test (tools in /status), blankrow_test (one call one line),
  evalrender_test / tui_test (generic call form, "node query"),
  toolrun_test (summary, group line), model_test / onboarding_test (field
  order).
- **Walk:** step 05 types the address, waits for the filled-in name
  ("local"), enter, key. Run dir:
  `tmp/ux-run-onboarding-20261023-215018` (worktree
  agent-a130b1c68680f2037). Every PNG looked at; consecutive-frame row diff
  (scratchpad r4/shift.py): no row moved; rows that changed changed in
  place, except 05→06 where the model pick restarts the engine and the
  conversation is replaced (pre-existing).
- **Open / for the owner:** the walk's service is now named "local" (the
  prefill) — keep, or have the walk type "work"? A model pick still
  replaces the conversation (the question leaves the log for the
  composer). A one-call run that gets a second step grows from 1 row to 3
  (the header appears above the first call: growth, the call line moves
  down one row). The tool-result preview is dim 240 (renderers.go, not
  changed). Renderer previews (background job, queued, recap) still use
  orange. The header still repeats the model.
- **Not verified:** a real terminal, light theme, personas on this run.

## ✅ 38. Readable tool results, neutral previews, the walk at three sizes (USER, 2026-10-23)

- **Contrast (1):** new token `stSecondary` (tui.go) = 246 on dark
  (5.86:1 on the renderer's #16181c, 6.92:1 on black, 4.35:1 on the 236
  band), 240 on a light terminal (7.11:1 on white; set in
  initMarkdownStyle). stDim 240 (2.5:1) stays for chrome only: the "→"
  arrow, rules, hints, labels, box edge, thinking header. Moved to
  stSecondary: every renderer's preview text and expanded body
  (renderers.go, filerender.go, evalrender.go shape/where/"+N more
  columns"), box body rows (thinking text), a finished thought's line,
  a group's targets. Code comments in highlighted code stay 240.
- **Orange (2):** neutral now: background job receipt, job/agent counts,
  "Started agent", agent_monitor acts (Answered/Told/…), tell_parent ✉,
  dun_msg ✉, ingest "queued", recap. Kept orange: ship "⊘ refused"
  (verified but NOT pushed — work left local), an agent report that says
  "BLOCKED, waiting on you". Running/idle children green, failed red.
  agentReportHead now reads subAgent.report's "agent #N is <state> after …
  (N tokens)" (it showed "#N is" in orange). Activity strip's idle "✓" in
  orange (activity.go) not changed — not a tool preview.
- **Walk realism (3):** /login step erases the filled-in "local" with five
  backspaces and types "work". Found a product bug: the filled-in name kept
  its cursor after the first character (textinput SetValue), so the result
  was "workocal" — fixed (`CursorEnd`, model.go).
- **Sizes (4):** walk JSON `"sizes": ["48x24","100x30","180x50"]`
  (cols/rows still work; neither = 100x30); `bin/ux --size WxH`
  (repeatable) replaces the list. One run dir per size,
  `tmp/ux-run-<walk>-<stamp>-<cols>x<rows>/`, each its own arrival (fresh
  HOME + stub); BRIEF says that size and lists only that dir's files
  (`ux sizes`, `ux assemble -size`). Waits match wrapped text (rows
  trimmed + joined with a space, whitespace normalized; or joined with
  nothing for a cut word). Coordinator add: `ux/product.md` copied into
  every run dir; BRIEF sends every persona to it first and lists it.
  Tests: tools/ux/sizes_test.go, ttyrec TestScreenHas_Wrapped,
  TestRecSizeOverride.
- **Fixed from the 48/180 run (5):** at 48×24 the /mcp add form left the
  conversation pane blank and the walk stopped (palette narrowing grew the
  pane with held blank rows; the form's shrink then anchored the bottom of
  the blanks) — a shrink while holding keeps the top row and cuts the hold
  (tui.go). Call rows cut mid-word at the edge → fitText (word + …). At
  180 an answer ran 170 columns → prose wraps at bandCap (100). ttyrec
  listed "→ Notes: …" as a key hint → hints are ≤4 words, no ":"/".".
  Tests: bounce_test TestNoBounce_PhoneFormAfterPaletteKeepsTheText,
  sizes_test.go (cmd/dun), onboarding4 TestLoginForm_FilledInName…,
  contrast_test.go; each checked to fail without its fix.
- **Run dirs:** `tmp/ux-run-onboarding-20261023-221704-48x24`,
  `-100x30`, `-180x50` (worktree agent-aa57da11355044b8e). Every PNG looked
  at. Consecutive-frame row diff: only the divider (panel heights), the
  model pick replacing the conversation (05→07, known), and at 48 the
  palette opening (07→08: conversation moves up 6 rows, the view follows
  the bottom when the pane shrinks while pinned).
- **Open / for the owner:** (a) at 48 the palette opening scrolls the
  conversation up (pinned view follows the bottom) and the splash leaves
  the view — keep, or hold the top row (then the newest rows hide under
  the palette)? (b) at 48 the tool result preview shows three words
  ("→ Notes: keep the…"); wrap it to a second row on narrow screens?
  (c) at 48 palette descriptions and form notes are cut to ~20 columns;
  status lines drop "esc cancel". (d) a notice that wraps at 48 starts its
  second row with "· key set". (e) prose cap at 100 also caps code blocks
  and tables in answers. (f) personas not run; light theme not seen.

## ✅ 39. A blank screen while poly-lsp started (USER, 2026-10-23)

Live: plain `dun` in the dun repo, phone (49×21), blank pane ~45 s until
killed. Engine journal: poly-lsp's `started` line (logged after MCP
initialize) came 54 s after raglit's. Two causes, both fixed:

- **TUI (cmd/dun/tui.go runTUI):** `startDunProc` (attachSession: dial
  dund, checkDundBuild, Create, Subscribe) ran BEFORE `tea.NewProgram.Run`;
  dund acks a Create only when the engine emits `session`, which is after
  `dun.Start` returns. Now runTUI starts with proc nil and Init's existing
  nil-proc path (`restartEngine` → `attachFn`) attaches behind the first
  frame. Status: "starting session… · Ns" (no proc), "starting the
  engine…" (proc, no ready), "starting session… (attempt N): err" on a
  first-attach failure. Enter before ready keeps the message
  (startqueue.go: dim "› text (sent when the session is ready)", "N
  message(s) kept" on the status line), sent at the `settings` after
  ready, or held for /login (holdUnsent) when there is no login.
  waitDundStopped no longer prints to stderr (it runs under the alt
  screen now).
- **Engine (harness.go Start):** built-ins were `builtins.Wait()`ed.
  Now every autostart server starts in the background; Start waits for
  the built-ins up to `serverStartGrace` (1.5 s), for an added server not
  at all (as before). `Harness.starting` (id → since) feeds
  `ServerState.Starting/StartingSeconds` (proto too); StartServer and
  reloadServers skip a server mid-start (no second process). A late
  server's OnServerStarted sends `ServerEvent{Background: true}`. The TUI
  shows it in the splash (servers row "X starting…", extras row
  "poly-lsp is starting…" — id only, fits 49) and the status line
  ("ready · poly-lsp starting 34s"); on up, the row changes in place and
  no notice is added (a failure still gets one). `/raglit`, `/mcp` detail
  and runningSummary say "starting".
- **Safety:** the Session is published under turnMu (a late server's
  rebuild runs on its own goroutine); applyTools loops while a rebuild is
  pending, so a server that comes up while idle joins at once, mid-turn
  at the turn's end (unchanged). ToolNames reads a snapshot stored at
  rebuild (it read Session.Tools unlocked). One-shot runs (`dun -p
  'task'`, `dun 'task'`) call `WaitServers` before the task, so the task
  keeps every tool it was started with.
- **Recorded decision?** None by the USER. Commit 7b8552d (agent,
  2026-10-22) chose "the session waits for its slowest built-in"; this
  replaces it.
- **Tests:** slowstart_test.go (fake poly-lsp sleeping 3 s in initialize,
  `DUN_FAKE_SLOW_MS`: Start < 2 s, Starting reported, a turn runs, tools
  join, one process; WaitServers), cmd/dun/startframe_test.go (real
  program at 49×21 with an attach that never answers: alt screen +
  "starting session…" drawn < 1 s, ctrl+c quits; status wording; kept
  message sent at ready / held without login; late server: no notice, ≤2
  rows change, same height). `make test` passes.
- **Walk:** `bin/ux --size 49x21` →
  `tmp/ux-run-onboarding-20261023-224616-49x21`: cast shows the alt
  screen and "starting session" in the first output at 0.15 s, ready at
  0.47 s. The walk has no slow server (no slow-server variant written).
- **Open:** (a) not run live against the real dund/poly-lsp (dund not
  restarted, by rule). (b) a create in flight when ctrl+c quits still
  makes a session nobody subscribes to (claimed, idle). (c) `/poly-lsp
  off` during its start does nothing; the start then completes. (d) the
  status cannot say "indexing": MCP gives no progress before initialize
  answers. (e) systemctl start output (dundcli) could still print under
  the alt screen on failure.

## ✅ 40. Run-4 fixes: splash with context and spend, a spend ledger, wide blocks scroll (USER, 2026-10-23)

- **Splash (owner 1):** nine fixed rows, each fitted at a word: folder ·
  model · servers (poly-lsp is built in: here, "starting Ns" counted on the
  TUI's clock and redrawn on the spinner tick only when the words change) ·
  extensions (`raglit off · /help raglit`; the word "extras" is gone) ·
  skills (`N · /skill` — /skill is the real command) · context files (the
  rule files dun loads, rules.go: AGENTS.md/CLAUDE.md and their @imports) ·
  context (`~N of W before you type (P%)`, "~" while estimated, the exact
  count replaces it in place; second row `system · tools (mcp) · skills ·
  context files`, from the label column at 49) · spend. Context is orange
  from **60%** of the window (`contextHeavy`). Engine: the skill tool is
  its own part (`SkillsPart`), the rules' files travel as
  `SystemFiles` (each estimated). `/status`: the same rows plus servers
  (state, tools, their share), skills (each ~tokens of its list line),
  context files (each ~tokens), spend (today/week/month/year, this month
  by model, this session, the key's budget).
- **Spend ledger (owner 3):** `$DUN_HOME/spend/spend-YYYY-MM.jsonl` (0600,
  dir 0700), one line per response that reported a cost: time, provider
  (saved name, else host), key fingerprint (first 8 bytes of SHA-256 in
  hex — never the key), model, cost, currency. Written by
  `SpendMeter.NoteAs` on every client (main, the per-model cache for
  sub-agents, vision). Cross-process safe without a lock: O_APPEND, one
  write, line < 1 KiB (< PIPE_BUF); a reader leaves a partial last line for
  the next read. Size: one file per month (~150 B a line, 10k responses a
  day ≈ 45 MB a month); reads are incremental per file; dun never deletes
  old months. Splash: smallest significant period (**$0.01 / 1 credit**),
  `· $X left on <provider>` from the key budget, orange when what is left
  ≤ the key's spend over the last 24 hours (or ≤ 0); a provider with no
  cost says `not reported by <host>`, never $0.
- **Wide blocks (owner 2):** cmd/dun/wide.go. A fenced code block or table
  wider than the prose column renders at its own width (code via a glamour
  renderer rounded to 20 columns, padding trimmed; a table at its natural
  width), never wrapped; wider than the pane, each row cut with
  `… +N cols →` and a dim `…` on the left when scrolled. → on the selected
  answer descends; ←/→ (h/l) a third of the pane, home/end, ← at the left
  edge or esc leave. All wide blocks of one answer scroll together. The
  streaming reply renders the same way, so the finish moves no row.
- **Run-4 findings (4–9):** `/mcp add` its own row (third, "connect a server
  you were given — its command or address"); palette heading "commands";
  descriptions cut at a clause (`fitClause`: `: ; — (` before `,`).
  A panel opening below (palette, form, picker) keeps the view's top row
  (`panelHeld`): it takes the blank rows, then covers the newest rows; the
  hold lasts until the pane has grown back, so a form closing with an
  "adding …" line moves the view only as far as that line needs. Form
  fields sized to the pane (`formFieldW`), placeholders that fit 49, notes
  one per row, "saved privately to ~/.dun/config.json" (saveConfig now
  chmods 0600 — WriteFile's mode only applied to a new file). One-call
  tool row: only ✓ green; at ≤ 60 columns the preview takes up to two
  rows of its own. Status lines ≤ 4 hints; `fitHints` never drops `esc …`
  / `ctrl+c quit` while another hint can go; forms put esc cancel second.
  `⚠` (in DejaVu Sans Mono, and dun's other warnings) in column 0 of the
  not-logged-in line, now "not logged in — type /login to connect" (fits
  49; errNoLogin's words unchanged, the engine matches them). Kept line:
  "kept your question — type /login, then it comes back", column 2,
  stSecondary. Wrapped warning, kept and notice lines hang at column 2.
  ↑ on an empty composer (or on a recalled entry) walks history, ↓ back to
  empty; ctrl+↑/↓ unchanged. Divider split at min(w,100)/2 (50 at 180).
  Walk: the /login step keeps the filled-in name.
- **Tests:** ledger_test.go (4 processes + 4 goroutines × 300 lines, all
  whole, 0600; periods; incremental and partial-line read; NoteAs never
  writes the key), context_tokens_test/rules_test (skills row, files),
  splash5_test.go (9 rows at 49/100/180 through counting → estimate →
  exact → spend → servers → skills; context row, 60% orange; period
  selection, credits floor, budget orange, "not reported"; /status
  detail), wide_test.go (split, cut not wrapped at 49/100, fits at 180,
  →/←/esc keys with no row moving, stream = finish), findings5_test.go,
  bounce_test `TestNoBounce_PhoneFormKeepsTheRowsWhereTheyWere` (49×21:
  every typed key, the form, esc, add — checked to fail without each fix).
  Changed for the new rules: the ↑ scroll tests type a draft first (↑ on
  an empty composer is history now), palette height +1, six-first list.
- **Walk:** `tmp/ux-run-onboarding-20261023-233726-{49x21,100x30,180x50}`
  (worktree agent-a767ea5ef1f930ca6); every PNG of the run before it
  (`…-233154-*`, same words) looked at. Row diff: at 49 the palette covers
  the lower rows (nothing above moves); 04→05 the login notice and the
  picker scroll 4 rows (new content while a panel opens); 08→09 two rows
  (the new notice); 05→07 the model pick replaces the conversation (known).
- **Open / for the owner:** (a) ↑ on an empty composer now recalls history:
  on Termux, where a finger-scroll arrives as plain ↑/↓ with mouse tracking
  off, a swipe on an empty composer walks history instead of scrolling.
  (b) On a phone a panel covers the newest rows (the answer under /mcp
  add's form) rather than moving the splash off. (c) At 49 the context
  parts row starts at column 2 (the value column is too narrow).
  (d) Skills count includes /skill-only ones. (e) The splash's spend row
  refreshes on spend/usage events only (not at midnight). (f) The login
  notice at 49 wraps "key set" under the picker. (g) Personas not run;
  light theme unseen; no live dund or real provider with a cost.
