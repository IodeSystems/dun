# dun: bugs and requests (2026-10-27)

From the owner's doc "Iode Bugs and Feature Requests in public tooling"
(Google Docs, 1sbAjGqj…), restated. `[x]` done and released (version), `[~]`
done on main, not released, `[ ]` open. A line marked **decide** needs the
owner's call before work starts.

## Critical (released fast)

- [x] **Context window.** dun learns it from an OpenAI-compatible model list
  (OpenRouter, vLLM, LiteLLM) when there is no `/props`; `/context config`
  (a picker, or `--max --compact-buffer --tool-lod --recap-nudge`) sets the
  window and the compaction knobs per model. v0.1.4.
- [x] **What dun is.** The `session` tool (was `harness`), `dun session [<id>] info
  [--context]`, `DUN_SESSION_ID` in every command dun runs, and "You are dun
  (👷), session …" in the prompt; for a repo's PR rules, which dun does not
  read. v0.1.4.

## Since the doc

- [x] `/effort` (thinking level per request, per dialect) and named reasoning
  dialects for a LiteLLM proxy (`"reasoning": "openai"`). v0.1.4.
- [x] `/retry` (presets and knobs for a flaky provider). v0.1.4.
- [~] A call missing a required parameter gets the tool's signature; recap's
  error shows both of its shapes.
- [ ] **decide** Split recap into `recap {after, rewrite, keep_results?}` and
  `read_saved {ref, …}`, so a half call cannot be made.

## The TUI

- [~] Daemon notices (`🔔 [shell] {"servers":…}`) off by default: a notice
  that is JSON goes to the server's activity log (`/mcp`), not the
  conversation or the model.
- [~] Up with queued messages: take them out of the queue, merged, into the
  composer to fix and send again.
- [~] `/splash`: clear the screen and show the splash again.
- [~] `/rephrase prompt [text|default]`: show, set or reset the rephrasing
  prompt ($DUN_HOME/rephrase.txt).
- [~] `/close` exits; discarding a session for good is now `/discard`.
- [~] The splash says where each row is set: a row's command (`tools` →
  `/tools`, a hint that it is one).
- [~] The spend row: requests, tokens processed and generated, cache rate.
- [~] ctrl+p pastes the clipboard's text (wl-paste, xclip, pbpaste,
  termux-clipboard-get).
- [~] Messages between agents in boxes like thinking: from whom, to whom,
  what was said.
- [~] Multi-choice questions: a clear way to select, unselect and edit; tab
  edits a choice, as in single choice.
- [~] A background job's notice (`🔔 background job #2 succeeded — cd /tmp
  && cat > x.mjs <<'EOF' …`) shows its command as highlighted shell, its
  lines and heredoc as written, not flattened into one line (owner,
  2026-10-27: part of the code highlighting).
- [~] The skill reader (owner, 2026-10-27): enter on a skill in `/skill`
  opens a pane below the list with the skill rendered (glamour, the front
  matter a header). A path to a file in the skill's folder is a link (n/p or
  tab/shift+tab, enter opens, backspace/esc back; a missing file is struck
  through, ✗); an `@path` is inlined between dim rules, 3 deep, each file
  once; `/` searches the text and the links' targets. Under 80 columns the
  pane takes the screen. The engine reads the files (control `skillfile`,
  event `skill_file`), confined to the skill's folder. Display only.
- [~] tmux window mark (owner, 2026-10-27): in tmux the TUI sets the window
  options `@dun_icon` (⚙ working, ❓ question or login waiting, ✓ a turn
  ended unseen, unset when idle) and `@dun_state`; ✓ clears on the next key
  or a focus-in; both unset at exit. Calls are off the UI goroutine,
  coalesced, 300 ms apart at most, 1 s timeout. A question rings the bell
  once (`DUN_BELL=off`). `dun tmux` prints the tmux.conf lines and says
  whether this tmux shows the icon. Never renames a window. Experimental,
  iTerm2 only: `--exp-iterm-tabcontrol` titles and colours the tab by the
  same states (tested by the bytes; not seen in iTerm2).
- [~] Request stats on the top bar (owner, 2026-10-27), right side when
  there is room: `ctx 6.3k · cache 96% · ttft 0.9s · pp 477/s · tg 121/s`
  (`ctx 6.3k/131k` when the window is known). The turn's own rounds, not
  side calls or sub-agents (dun/reqstats.go): ttft client side; pp and tg
  from llama.cpp's timings, else client side; tg and ctx live at ~4 Hz from
  the streamed chars; cache only once the provider reports it; ctx drops at
  once on a recap or compaction. Event `request`. Fixed-width pieces; they
  go pp → ttft → cache → ctx → tg as the width shrinks, none under 80
  columns.
- [ ] Collapse only under scroll pressure: a block folds as the scroll pushes
  it, never ahead of it, so no blank rows are left (owner: not yielding
  matters most).

## Sessions and agents

- [~] Sub-agents in isolation: the agent tool's `isolation` is none,
  worktree (its own git worktree and branch; the parent merges), docker
  (the same, commands in a container) or nodir. Several run at once.
- [~] A sub-agent asks by ending its turn (owner, 2026-10-27): its question
  reaches the parent as the turn-end notification, the parent answers with
  `jobs(agent:N, tell:…)`. `ask_parent` is unlisted (a hidden alias that
  makes the question the report) and nothing blocks; the TUI has no live
  blocked state.

- [ ] Persist carries the sub-agents: a persisted root resumes its children;
  toggling persist toggles theirs.
- [ ] Descend into a sub-agent from the activity strip and come back out.
- [ ] `/worklog`: start a task, fork a subtask, end one with why, view;
  the model reads a closed worklog (input, events, recaps) and writes it up
  as a clean account, with the model, effort, harness and version.

## The CLI

- [~] `dun --usage [--json] [--no-net]`: total use, what is left, per-agent use, tokens
  processed and generated, cache rate, cost; without opening a session.
  Built (cmd/dun/usage.go, from the stats ledger). Open: the ledger cannot
  tell root from same-model sub-agents (both role main).
- [~] `dun -p "thing"`: the model is told to answer after a `# RESPONSE
  BELOW` line; stdout is the answer alone, the work goes to stderr.
  `dun -p` with no task is the JSON engine, as before.
- [x] `--no-tools`, `--no-rag`, `--no-lsp`.
- [x] `--media <file>` with a prompt (one-shot only).
- [~] `dun --no-dir`: a session attached to no directory, to talk with dun,
  its skills and its MCP tools; servers that need a working directory
  (poly-lsp, raglit's project index, the file tools, exec's workspace) are
  left out. (Relayed by corrallm-f2, 2026-10-27.)

## Workspace and tools

- [~] `/raglit workspace on|off [--scope]`: raglit for other indexes without
  reading the workspace; bare `/raglit attach` is every index of every
  project, searchable, attached per scope (needs raglit v0.3.0).
- [~] `/status`: a server scripted in mcpshell reads "scripted in the
  shell", not off; the effort is shown.

- [ ] `/docker` becomes `/isolate` (worktree and Docker together).
- [~] `/ship` removed (owner, 2026-10-27): repos state their own shipping
  rules. `--ship`, `--pr`, `--no-ship` are accepted and do nothing; a
  `ship` section in dun.json is ignored.
- [~] `/file add|remove <path>`, `/file`: a file or directory outside the
  workspace becomes readable and editable by dun's file tools for the
  session; refused one, the model is told to ask for `/file add <path>`.
  poly-lsp does not see added paths yet.

## The site (dun.iodesystems.com)

- [x] Product page, install, capabilities, `harness.json`, the signed
  registry; served by hz (public). Screenshots of the current build, the
  dun man as favicon.
- [x] Measured: first-turn prompt sizes of pi, dun, Claude Code, opencode,
  and with the same two MCP servers (docs/measured/first-turn.md); the
  MCP tool scripting section.
- [x] Make the savings visible (owner: hard to see what scripting and
  surfaced skills save).

## Other projects (sent to their sessions)

- [x] corrallm: hand-declared models can be deleted; dashboard keys are live
  (2bd1e96); a caller with no key gets 403 on every paid account; a 429
  names the budget that refused, its spend and when it has room again.
- [x] poly-lsp-mcp: language servers found outside PATH (pnpm, npm -g, volta,
  bun, go, cargo, homebrew), and Swift: v0.3.1, pinned in dun v0.1.7.