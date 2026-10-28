# Coding agent feature matrix (checked 2026-10-28)

Versions (npm registry / GitHub releases, 2026-10-28):
- Claude Code 2.1.312. Proprietary (npm license "SEE LICENSE IN README.md"). Install: `curl -fsSL https://claude.ai/install.sh | bash`; also brew cask `claude-code`, winget, apt/dnf/apk, npm `@anthropic-ai/claude-code`. https://code.claude.com/docs/en/overview
- opencode 1.19.5 (V1; a V2 line also exists, same `opencode` command). MIT. Install: `curl -fsSL https://opencode.ai/install | bash`, npm `opencode-ai`, brew `anomalyco/tap/opencode`. https://opencode.ai/docs/
- pi 1.4.0. MIT. Repo moved: github.com/badlogic/pi-mono -> github.com/earendil-works/pi; npm package now `@earendil-works/pi-coding-agent` (not @mariozechner). Install: `curl -fsSL https://pi.dev/install.sh | sh` or `npm i -g --ignore-scripts @earendil-works/pi-coding-agent`; Node >= 22.19. https://github.com/earendil-works/pi/blob/main/packages/coding-agent/README.md

Doc base URLs used below:
- CC = https://code.claude.com/docs/en/
- OC = https://opencode.ai/docs/ (source: github.com/anomalyco/opencode packages/web/src/content/docs/*.mdx)
- PI = https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/

| # | Row | Claude Code | opencode | pi |
|---|-----|-------------|----------|----|
| 1 | Any OpenAI-compatible endpoint | no. Gateway must speak Anthropic Messages/Bedrock/Vertex format; non-Claude models unsupported. CC llm-gateway-protocol.md#api-formats | yes. `@ai-sdk/openai-compatible` custom provider with baseURL. OC providers | yes. models.json `api: "openai-completions"`, baseUrl, apiKey. PI models.md |
| 2 | MCP built-in | yes. stdio/HTTP, `claude mcp`. CC mcp.md | yes. local and remote, OAuth. OC mcp-servers | yes. `pi mcp add`, ~/.pi/agent/mcp.json. PI mcp.md |
| 3 | MCP schemas kept out of requests | yes. ToolSearch on by default; only names listed. CC mcp.md#scale-with-mcp-tool-search | no. Docs warn MCP tools add to context; only enable/disable per tool. OC mcp-servers | yes. Per-server `exposure`: deferred (tool_search) or codemode (scripted). PI mcp.md |
| 4 | Sub-agents | yes. Agent tool, custom subagents. CC sub-agents.md | yes. `task` tool; General, Explore built-in. OC agents | via extension. examples/extensions/subagent; README: "skips sub-agents". PI README.md |
| 5 | Sub-agents parallel | yes. "spawn multiple subagents simultaneously". CC sub-agents.md | yes. General agent "run multiple units of work in parallel". OC agents | via extension. Example: parallel mode, max 8 tasks, 4 concurrent. PI examples/extensions/subagent/README.md |
| 6 | Sub-agent isolation worktree/container | yes (worktree). `isolation: worktree`; container only via manual sandbox env. CC sub-agents.md, worktrees.md | unknown. No doc found; Daytona sandbox plugin exists (via plugin). OC ecosystem | unknown. No worktree support found; docs cover running whole pi in a sandbox. PI containerization.md |
| 7 | Sessions survive terminal close (daemon) | yes. Background sessions run under a supervisor process (`claude agents`). CC agent-view.md | partial. `opencode serve`/`web` backend keeps sessions; TUI attaches. Not default. OC web, cli | unknown. No daemon doc; experimental pi-durable package is a library. PI (packages/durable/README.md) |
| 8 | Resume from browser/phone | yes. Remote Control, claude.ai/code, mobile app. CC remote-control.md | yes. `opencode web` over LAN with password, mDNS; mobile UIs via plugins. OC web | unknown. No browser/phone client documented. |
| 9 | Scheduled/recurring prompts built in | yes. `/loop`, cron tools, cloud `/schedule`. CC scheduled-tasks.md | no. Only GitHub Actions cron; scheduler is a plugin (opencode-scheduler). OC github, ecosystem | unknown. Nothing in docs; not found. |
| 10 | Loop detection | unknown. No doc found. | yes. `doom_loop` permission: same call 3x identical input, default ask. OC permissions | unknown. No doc/source hit. |
| 11 | Model removes/summarizes own context mid-task | unknown. Only user `/compact`, auto compact; subagents isolate context. CC context-window.md | via plugin. opencode-dynamic-context-pruning. OC ecosystem | partial. `context_edit` entries are extension API, not a model tool. PI extensions.md |
| 12 | Auto compaction | yes. CC context-window.md | yes. `compaction.auto` default true. OC config | yes. reserveTokens 16384 threshold. PI compaction.md |
| 13 | Long tool output truncated, full saved to file | yes. Bash >30k chars: file path plus 2k preview. CC tools-reference.md | yes. "[showing N of M; full output saved to file]" (source: packages/core/src/shell.ts, tool-output.ts); not in docs pages | partial. MCP text >20 KB middle-cut, full text in temp file; bash `full_output_path` in codemode. PI mcp.md, codemode.md |
| 14 | Skills (SKILL.md) | yes. CC skills.md | yes. OC skills | yes. Agent Skills spec. PI skills.md |
| 15 | Open source + license | no. Proprietary; repo hosts issues/plugins only. github.com/anthropics/claude-code | yes. MIT. github.com/anomalyco/opencode/blob/dev/LICENSE | yes. MIT. github.com/earendil-works/pi |
| 16 | Shows reasoning and every tool call | partial. Thinking collapsed by default, Ctrl+O verbose; MCP calls collapse to one line. CC model-config.md, interactive-mode.md | yes. `/thinking` toggles thinking blocks; tool calls shown. OC tui | yes. Tool calls shown; Ctrl+T thinking, Ctrl+O expand output. PI usage.md |

## Rows left "unknown"
- Claude Code: row 10 (loop detection), row 11 (model-driven context removal; only user /compact found).
- opencode: row 6 (worktree/container isolation).
- pi: rows 6, 7, 8, 9, 10.
Not checked: shittycodingagent.ai (pi.dev is the current site).

## pi custom OpenAI-compatible provider
File: `~/.pi/agent/models.json` (agent dir; override with env `PI_CODING_AGENT_DIR`). Reloaded when `/model` opens.
Source: PI models.md#configure-a-compatible-endpoint, PI models-configuration.md, PI configuration.md.

```json
{
  "$schema": "https://pi.dev/schemas/models.schema.json",
  "providers": {
    "ollama": {
      "baseUrl": "http://localhost:11434/v1",
      "api": "openai-completions",
      "apiKey": "ollama",
      "models": [
        { "id": "qwen2.5-coder:7b" }
      ]
    }
  }
}
```
Notes:
- Provider ID is any new key under `providers`; needs `baseUrl` and `api` (provider or model level). `api` values: openai-completions, openai-responses, anthropic-messages, google-generative-ai.
- `apiKey`: literal, `$NAME`/`${NAME}` env interpolation, or `!command` (run per request). Dummy key fine for local servers.
- Model fields: `id` (required), `name`, `reasoning`, `input`, `contextWindow` (default 128000), `maxTokens` (default 16384), `cost`, `samplingParams`, `headers`, `compat`.
- `compat` (openai-completions) flags such as supportsDeveloperRole, supportsReasoningEffort, maxTokensField; auto-detected from URL.
- Related pi docs: llama-cpp.md exists in the docs dir (not read).
