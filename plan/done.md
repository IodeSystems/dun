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
