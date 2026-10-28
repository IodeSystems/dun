# What each agent sends before you type (2026-10-27)

Each agent was pointed at a stand-in model server (`tools/promptsize/capture.py`)
and sent "hi" from an empty directory with a clean home: as it ships, with no
rules files, skills, extensions or MCP servers of the person's own. Its first
real request (the largest one that carries its tools) was counted by one
model, Qwen3.8 27B on llama.cpp (`tools/promptsize/measure.py`): the request
is sent with `max_tokens=1` and the server's `prompt_tokens` is read, so the
count includes how that model's chat template lays out tool definitions.
"Tools" is the total less the same request without its tool definitions.

| agent | version | total tokens | tools | tool tokens | the rest (system prompt, message) |
|---|---|---:|---:|---:|---:|
| pi | 0.82.1 | 1,613 | 4 | 917 | 696 |
| dun | main (after v0.1.4) | 5,204 | 14 | 4,026 | 1,178 |
| Claude Code | 2.1.311 | 11,366 | 12 | 7,602 | 3,764 |
| opencode | 1.15.3 | 13,704 | 10 | 7,806 | 5,898 |

The tools each one sent:

- pi: read, bash, edit, write.
- dun: eval, exec, exec_monitor, file_read, file_edit, file_write, file_grep,
  file_list, ask_user, agent, agent_monitor, recap, session_state, harness.
- Claude Code: Agent, Bash, Edit, ListAgents, Read, ReportFindings,
  ScheduleWakeup, Skill, ToolSearch, Workflow, Write, and a placeholder for
  the tools it loads on demand (ToolSearch).
- opencode: bash, edit, glob, grep, read, skill, task, todowrite, webfetch,
  write.

## How to read it

- This is the first turn only. What an agent does to the context as a session
  goes on (dun cuts command output above 20,000 characters and keeps the
  whole on disk; the recap tool replaces churn) is not measured here.
- One model's tokenizer and chat template. Another model counts differently,
  but the same for every agent.
- Claude Code speaks Anthropic's API: its request was converted to the OpenAI
  form for counting (system text first, each tool's JSON schema unchanged).
  Qwen's template takes a system message only first, so every agent's system
  text is merged into one leading message (same text).
- pi was run with `--no-skills --no-context-files --no-extensions` and its own
  clean agent directory (`PI_CODING_AGENT_DIR`): pi exits with code 2 and no
  message when HOME is changed. dun was run with a clean HOME and DUN_HOME (it
  also reads ~/.claude/skills).
- Not measured: hermes (not installed), Codex, Gemini CLI, aider.

## Reproduce

    tools/promptsize/capture.py OUT/<agent> <port>     # one per agent
    # point the agent at http://127.0.0.1:<port>, send "hi"
    KEY=… tools/promptsize/measure.py <url> <model> OUT/*

## With the same two MCP servers (2026-10-27)

Playwright MCP 0.0.83 (`node cli.js --headless`, from the npx cache) and
poly-lsp-mcp (`mcp --root <dir>`) added to each agent, the same way:

| agent | total tokens | tools sent | tool tokens |
|---|---:|---:|---:|
| opencode 1.15.3 | 19,959 | 38 | 14,059 |
| dun, both servers direct | 12,220 | 42 | 10,802 |
| Claude Code 2.1.311 | 11,643 | 12 | 7,602 |
| dun, Playwright scripted in mcpshell | 7,203 | 17 | 5,929 |

- Scripted, Playwright's tool schemas leave the request: the model gets
  `eval` and one line in the system prompt naming the servers hosted in the
  shell and `help("<ns>")`. 12,220 → 7,203 is 41% less on every request.
- dun's built-in poly-lsp cannot be scripted (its file guard needs it
  direct); it stays direct in both dun runs.
- Claude Code sends MCP tools as names behind its ToolSearch tool and loads
  a schema when the model asks: its count barely moves (11,366 → 11,643).
- The capability, checked live: dun with Playwright scripted and the local
  Qwen 27B was asked to open https://dun.iodesystems.com/ and report its
  title and first heading. It called `playwright.browser_navigate` and
  `playwright.browser_evaluate` inside `eval` and answered correctly, first
  try, without calling help().
- `~/.local/bin/node` here is a shim that finds node under `$HOME/.nvm`, so
  a clean HOME breaks it (and pi, which runs on it): the servers were given
  node's absolute path.

## pi 1.4.0 (2026-10-27): built-in MCP and codemode

pi 1.4.0 has built-in MCP (`~/.pi/agent/mcp.json`) and a `codemode` tool: a
QuickJS script calls pi's tools; MCP tools default to `codemode` exposure,
so their schemas are not sent and scripts find them with `searchTools()`.
The same two servers, same method (its MCP is an extension: run without
`--no-extensions`):

| pi 1.4.0 | total tokens | tools sent | tool tokens |
|---|---:|---:|---:|
| no MCP servers | 1,783 | 4 | 916 |
| both servers, default exposure (codemode) | 2,448 | 5 | 1,479 |
| both servers, `"exposure": "direct"` | 8,158 | 32 | 7,291 |

So pi, like dun's scripted mode, keeps MCP schemas out of the request by
default, and with a smaller base: 2,448 against dun's 7,203 (where dun keeps
its built-in poly-lsp direct, ~1.8k). The 0.82.1 figures above predate
this.

## dun after the prompt cuts (2026-10-27, 6fdf11c)

`session` replaced harness and session_state, `jobs` replaced exec_monitor
and agent_monitor, and the system prompt stopped repeating the tools'
descriptions. Same method:

| dun | before | after | tools |
|---|---:|---:|---:|
| no MCP servers | 5,204 | 3,925 | 12 |
| Playwright + poly-lsp, direct | 12,220 | 10,806 | 40 |
| Playwright scripted, poly-lsp direct | 7,203 | 5,923 | 15 |

Capability smoke test, local Qwen 27B, two runs each, the same task (a
command that becomes a background job, a file written meanwhile, then wait
for the job and report): both builds 2 of 2 correct. The new build called
`jobs` (wait) once or twice, which the old could not do for a command, so
the task took more rounds and more tokens in all (17.5k and 22.5k against
11.5k and 11.5k) though each request was smaller.

## A whole task, four agents (2026-10-27)

tools/promptsize/session.py: each agent runs one task on a fresh fixture
(fixture.py: a Python repo whose CI failed; ci.log is 218 KB with the real
error buried; done = `python3 -m unittest` passes after), through a relay
(relay.py) to the same model (`chat` on llm.iodesystems.com, after corrallm
c5a3e9c applied the model's sampling profile and thinking budget on every
route: runs from before it are not comparable and were replaced) that
records every request's usage. Clean homes; 3 runs each. Claude Code goes
through the gateway's Anthropic endpoint (/v1/messages, the same lane);
its prompt is input + cache reads + cache writes, and corrallm cuts its
per-request billing line from its system prompt (for the cache).

| agent | done | requests | first request | last request | whole task |
|---|---|---:|---:|---:|---:|
| pi 1.4.0 | 3 of 3 | 5–7 | 1,827 | 3,089–3,393 | 12.8k–17.3k |
| dun (v0.1.5 + skill reader) | 3 of 3 | 6 | 3,986 | 7,443–10,573 | 38.6k–49.4k |
| Claude Code 2.1.311 | 3 of 3 | 5–6 | 12,499 | 13,896–14,415 | 66.5k–80.0k |
| opencode 1.15.3 | 3 of 3 | 6–7 | 595 (a title call) | 15,356–15,630 | 73.6k–89.4k |

A short task: no agent reached ten requests, so growth over a long session
is not measured by it.
