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
