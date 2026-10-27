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
- [x] **What dun is.** The `harness` tool, `dun session [<id>] info
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
- [ ] The splash says where each row is set: a row's command (`tools` →
  `/tools`, a hint that it is one).
- [ ] The spend row: requests, tokens processed and generated, cache rate.
- [~] ctrl+p pastes the clipboard's text (wl-paste, xclip, pbpaste,
  termux-clipboard-get).
- [ ] Messages between agents in boxes like thinking: from whom, to whom,
  what was said.
- [ ] Multi-choice questions: a clear way to select, unselect and edit; tab
  edits a choice, as in single choice.
- [ ] Collapse only under scroll pressure: a block folds as the scroll pushes
  it, never ahead of it, so no blank rows are left (owner: not yielding
  matters most).

## Sessions and agents

- [ ] Persist carries the sub-agents: a persisted root resumes its children;
  toggling persist toggles theirs.
- [ ] Descend into a sub-agent from the activity strip and come back out.
- [ ] `/worklog`: start a task, fork a subtask, end one with why, view;
  the model reads a closed worklog (input, events, recaps) and writes it up
  as a clean account, with the model, effort, harness and version.

## The CLI

- [ ] `dun --usage`: total use, what is left, per-agent use, tokens
  processed and generated, cache rate, cost; without opening a session.
- [ ] **decide** `dun -p "thing"` runs in text and exits with the answer;
  `dun -p` alone is the JSON engine (`--no-tui`). Today `-p` is the engine
  and `dun "thing"` is the one-shot, so this changes what `-p` means.
- [ ] `--no-tools`, `--no-rag`, `--no-lsp`.
- [ ] `--media <file>` with a prompt.
- [ ] `dun --no-dir`: a session attached to no directory, to talk with dun,
  its skills and its MCP tools; servers that need a working directory
  (poly-lsp, raglit's project index, the file tools, exec's workspace) are
  left out. (Relayed by corrallm-f2, 2026-10-27.)

## Workspace and tools

- [ ] `/docker` becomes `/isolate` (worktree and Docker together).
- [ ] **decide** Remove `/ship` and its prompt lines: repos state their own
  shipping rules, and dun's read as the blessed path.
- [~] `/file add|remove <path>`, `/file`: a file or directory outside the
  workspace becomes readable and editable by dun's file tools for the
  session; refused one, the model is told to ask for `/file add <path>`.
  poly-lsp does not see added paths yet.

## Other projects (sent to their sessions)

- [x] corrallm: hand-declared models can be deleted; dashboard keys are live
  (2bd1e96); a caller with no key gets 403 on every paid account; a 429
  names the budget that refused, its spend and when it has room again.
- [ ] poly-lsp-mcp: find language servers installed with pnpm (aw4
  i_1m5ze4k2581r4sbxy4h9g5; committed there, waiting on the owner). Swift
  is pushed (6e5ef86, untagged).
