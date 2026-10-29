# MCP servers, tools and project state

Why the tool-server layer is built the way it is. Short rules on startup,
rebuilds and config files: "Tool servers and extensions" in `decisions.md`.
Switch scopes (session, project, global): `mcpscope.go`.

`/rag` (USER 2026-10-28; it was `/raglit`) is the search indexes as a switch
table: every index raglit lists (`raglit index --list --json`), each on, off
or unset at the global, project and session scopes, the nearest scope that
says deciding (`attach.go`, `indexkind.go`). Unset everywhere, this project's
own indexes are on and every other one off. What is on is what the model's
`search`/`get_document` read when they name no index, and what proactive
surfacing reads. One of the project's own switched off turns `<project>:*`
into its indexes named one by one (raglit's list, cached). The raglit
server's own switch stays in `/mcp`; its verbs (`setup`, `watch`, `include`,
`workspace`, …) are `/rag <verb>`, and `/rag status` is the capability report.

`/file` (USER 2026-10-28) is the paths outside the workspace the file tools
may use, switched the same way (`fileadd.go`): each scope (`paths.json`
global, project, and the session's sidecar) may name a path, with on/off, a
mode (read, the default, or write) and, for a directory, index (on by
default). The nearest scope that sets a field decides it. A read-only path
refuses `file_edit` and `file_write` with the command that opens it for
writing (`withFiles`). An indexed directory is watched by raglit into an
index of its own (`file-<name>-<hash>` in this project's namespace) and
switched on in `/rag` at the same scope; index off switches it off there and
keeps it. A session's old `<session>.files.json` list (read and write) is
read into its `paths.json` once.

A global switch (on/off, mode, a tool, a server added or removed) is a write
of `~/.dun/mcp.json`, and every session follows it. A turn re-reads the
layers at its start (`followServers`), and under dund the write is also
announced: the engine sends a `servers` peer request, dund forwards
`ServersChanged` to the owner's other loaded sessions, and an idle one
re-reads at once and sends its clients the new states
(`FollowServersIfIdle`). A busy one waits for its next turn, because
reconciling can restart the shell and kill an `eval` in flight. A session or
project switch still wins where it is set (USER 2026-10-28).

## Servers: direct or in the shell

(USER 2026-10-22.) Default is direct. A user-added server may instead be
hosted inside mcpshell (`ServerSpec.Via`, `"direct"` / `"shell"`), so its tool
schemas leave every request and the model reaches it through `eval`. Built-in
servers cannot be moved. Code: `shellhost.go`, `servers_config.go`.

- A shell-placed server is never started by mcpmgr. The shell is launched with
  `--mcp <$DUN_HOME/projects/<id>/shell-mcp.json>` (0600, removed when nothing
  is hosted). A `$NAME` bearer becomes mcpshell's `auth_cmd` printing the
  variable, so no token is written; a literal bearer or header is written
  resolved. Adding, removing or moving a hosted server restarts a running shell,
  which kills an in-flight `eval` and its state.
- **Namespace rule**: the id with non-identifier characters turned into `_`, a
  leading digit gets a `_` prefix (`ShellNamespace`). mcpshell's `eval`
  description is fixed text, so the model learns namespaces from one line in
  the system prompt.
- Hosted status is read from mcpshell's stderr lines per upstream
  (`parseHosted`, pinned by `TestParseHosted`). A miss reads "unknown" and the
  server is still listed to the model. mcpshell dials upstreams before serving,
  so a hanging upstream blocks the shell's start.
- What the shell loses, and the add forms say so: docs indexing, prompts and
  skills, the deny-list, renderers. mcpshell has no env expansion in headers
  and no per-tool deny-list.
- Direct servers other than the built-ins get tool names `<id>__<tool>`
  (two servers may share a tool name). Built-in names stay unprefixed.

## Tool cost and toggles

`/mcp` and `/mcp tools` show per server `N tools - ~Xk tokens - M hidden`; each
tool's wire JSON is priced by the calibrated `promptMeter`. This is the meter
estimate, not the exact `/context` count (which covers enabled tools only). At
5k tokens or more a hint suggests hiding some or moving the server into the
shell. Engine control `mcptools` ("<id>" or "toggle <id> <tool> on|off")
answers with an `mcp_tools` event, so TUI and web share it.
Engine control `skillfile` (JSON `{"skill", "file"}`) answers with
a `skill_file` event: one file of a skill for the /skill reader, confined
to the skill's folder, with the folder's file list and its @includes' text.

- Disable entries may be globs (`path.Match`). Re-enabling a tool a glob hides
  is refused (edit the glob).
- Each scope stores its own list. **An emptied list is written as `[]`, not
  omitted**, so it still overrides a lower layer. A single tool switched at
  project or session scope overrides a project deny-list.

## Remote servers and OAuth

Transport: agentkit's mcpmgr speaks Streamable HTTP (POST per message, JSON or
SSE reply, `Mcp-Session-Id`, `MCP-Protocol-Version`, DELETE on close, headers
on every request). The older HTTP+SSE transport and the server-initiated GET
stream are not built. `url`, `bearer` (`$NAME` reads the environment at use,
never written resolved) and `headers` are on the spec.

OAuth (`ServerSpec.OAuth`; `mcpoauth.go`, `servers_oauth.go`). Targets that
shaped it: Snowflake's managed MCP (401 -> RFC 9728 -> RFC 8414, no dynamic
client registration, pre-registered client and **exact** redirect URI),
Superset (validates an external IdP's JWT; with Google that is the ID token),
and corporate tools behind a login gate.

- **Discovery**: the configured issuer, else the 401's `resource_metadata`
  (else the well-known URL) -> protected-resource metadata (its `resource`
  must cover the server URL, path prefix allowed; otherwise the login is
  refused, so one server cannot obtain a login meant for another) -> first
  authorization server -> its metadata.
- **Client metadata document**: an authorization server that says
  `client_id_metadata_document_supported` (ElevenLabs, with no registration
  endpoint) takes a URL as the client id and fetches it. dun's is
  `site/oauth/client.json`, served at
  `https://dun.iodesystems.com/oauth/client.json`; its redirect URI is the
  default port's. Set as `client_id`, it needs no registration.
- **Redirect**: a loopback listener on a fixed port (default 33418,
  `redirect_port`), `127.0.0.1:<port>/oauth/callback`, because the redirect is
  registered exactly. If the port is busy, another session is logging in and
  this one waits for that login to land in the shared token file. Loopback only
  works where the browser and the engine share a host; a web login through
  dund's public callback is not built.
- **Login URL is surfaced** (`mcp_login` event, TUI line, plain-mode stderr)
  and also opened (`OpenBrowser`, off with `DUN_NO_BROWSER`), because the
  engine runs under dund, often with no display, and the person is wherever the
  TUI is. The login runs before the handshake, so it is outside the discovery
  timeout.
- **Resource and audience**: `resource` defaults to the discovered resource;
  with an explicit `issuer` it defaults to none, **because Google rejects
  unknown parameters**; `"none"` omits it; any other value is sent as given.
  `send` chooses `access_token` or `id_token` for the Authorization header.
  Nonce and `aud` are checked for ID tokens; the signature is not, because the
  token came straight from the token endpoint. Google needs `auth_params`
  `access_type=offline` (and `prompt=consent`) for a refresh token.
- **Token exchange**: a public client sends its id; a confidential one uses
  basic auth when the server lists it or lists nothing (RFC 8414's default),
  else post.
- **Storage**: `$DUN_HOME/oauth/<id>.json`, 0600, with issuer, token and
  revocation endpoints, auth method and resource, so a refresh needs no
  discovery. One OAuth client per server id; the file is shared by every
  session of the user. Tokens are plaintext (no keyring).
- **Refresh**: 60 s before expiry and on a 401; if refresh fails, interactive
  login bounded at 5 min. **A 401 within 30 s of a login is an error, not
  another browser tab.**
- **Login gate diagnosis** (`mcpgate.go`): a server behind Google IAP,
  Cloudflare Access or oauth2-proxy answers with a redirect or an HTML login
  page, which used to surface as a JSON decode error or a discovery timeout.
  An unauthenticated `initialize` is sent without following redirects; a 3xx
  to another host or a login-looking path, a 200 `text/html`, or IAP's
  `X-Goog-IAP-Generated-Response` header is reported as a gate with the fix
  (IAP: Google ID token with the IAP client as audience; Cloudflare:
  `cloudflared access token` into a `cf-access-token` header). A 401 with
  `resource_metadata` on a server with no `oauth` block says to add one.
  A gate that answers 401 without the IAP header reads as an OAuth server.

## MCP docs, skills and prompts

(USER 2026-10-21.) Agent Skills plus MCP-served ones; docs ingested on add and
on refresh; servers saved for every project; HTTP transport built in agentkit.

- **Docs** (`mcpdocs.go`): a server's text resources are copied into
  `$DUN_HOME/mcp-docs/<id>/` and raglit index `mcp-<id>`, so the proactive
  search surfaces the matching one without the model knowing to call a tool.
  The copy runs on `/mcp add` and `/mcp refresh <id>` only, not every session:
  listing and reading every resource is network traffic nobody asked for.
  A resource the server stops listing is forgotten.
- **Skills** (`skills.go`): the format is Agent Skills (a directory with
  `SKILL.md`, frontmatter name and description). Only name and description ride
  each request; the body costs tokens when the model calls `skill`. Sources
  lowest first: `~/.claude/skills`, `$DUN_HOME/skills`, MCP resources
  `skill://.../SKILL.md`, the project's `.claude/skills`, then `.dun/skills`.
  The `skill://<name>/SKILL.md` layout is an assumption; there is no MCP
  standard. A local skill directory is a host path, so scripts in it are not
  visible to a Docker exec backend.
- **The skill list belongs in the `skill` tool's description, not the system
  prompt.** Measured (n=1 each): with the list in the system prompt the model
  did not load the matching skill for "triage ticket"; with the list in the
  tool description it loaded it first and followed all three steps. The model
  matches a task to the tools it has and never to a skill listed elsewhere.
- **Prompts** are `/<server>:<prompt>` slash commands.

## File tools

`agentkit/files` is mounted in `rebuildTools` next to exec (`files.go`):
`file_read`, `file_edit`, `file_write` on by default; `file_grep`,
`file_list`, `file_delete` and `file_move` opt in. USER wanted a toolset "minimal,
configurable, but efficient and complete". Why: without them the model reads
with `cat`, edits with `sed` and heredocs, and searches with `grep -r` through
exec, with unbounded output, no line numbers and an exit code as the only
receipt of an edit.

- Root is the session working tree (the worktree when one is in use), so a path
  that climbs out is refused. Docker sessions work on the same tree through the
  mount.
- Rebuilt on `Rehoist`, **kept across other rebuilds so read-tracking survives**
  (write's overwrite guard refuses to overwrite a file not read).
- Config is the `files` section of any dun.json layer, field by field;
  `tools: []` is off, `null` inherits (so no `omitempty`), `ignore` accumulates.
- `AfterWrite` is not wired: poly-lsp-mcp watches the tree itself (fsnotify,
  200 ms debounce), with `Index.LookupExisting` as the backstop for a missed
  event.
- **grep and list are not defaults** (2026-10-27, agentkit v0.14): `file_read`
  lists a directory and, with `grep`, searches a file or a tree, with the same
  engines and output. USER: "I thought it would be file_read(path, grep)". A
  benchmark had dun's model read the last 120 lines of a 218 KB CI log (~5.8k
  tokens, carried for four more requests) where pi's model grepped it for 265
  chars; a search in the tool the model already reaches for is one it makes,
  and two fewer definitions ride every request. A head or tail read of a file
  of 64 KB or more ends with a one-line note to grep it. A config that names
  grep or list still gets them, and the TUI renders `file_grep`/`file_list`
  steps as before.
- Sub-agents get the same set; no read-only research role exists to hook.
  The system prompt has one line naming the held file tools and preferring them
  to exec.

## Project registry and config layers

(USER 2026-10-22.) "we don't want to pollute git trees and we cannot tell
people to use dun": no `.dun/` created in a repo, no `.gitignore` entry asked of
anyone. Code: `project.go`, `servers_config.go` (`serverConfigPaths`).

- `$DUN_HOME/projects.json` maps an absolute source dir to an id, written
  temp-and-rename under flock. State lives in `$DUN_HOME/projects/<id>/`
  (`dun.json`, `dun.local.json`, `agents_read`, `worktrees/`, `overlays/`).
- **The id is minted at first sight** (`<slug>-<6 hex>`), not derived from the
  path: a path hash is unreadable by a human, and a bare directory name
  collides between two checkouts of one repo. The cost: a moved repo gets a new
  id and its old state is orphaned.
- A git worktree of any kind maps to its source repo's id through
  `git rev-parse --git-common-dir`, and what dun writes for that worktree goes
  to an overlay that wins over the project's. Bare repos and submodules stand
  for themselves.
- **Config layer order**, lowest first: built-in, `$DUN_HOME/mcp.json`, repo
  `dun.json`, repo `.dun/dun.json`, repo `dun.local.json`, repo
  `.dun/dun.local.json` (older layout), project state, overlay; the session
  sidecar is above all. Reading never registers a directory.
- **Repo-side files are read-only to dun.** A project may commit `dun.json`,
  `.dun/dun.json` and `.dun/skills/` on purpose, so dun reads them and never
  creates them. Everything dun writes goes under `$DUN_HOME/projects/<id>/`.
- `go.mod replace => ../x` still resolves through a mount symlink beside the
  worktree; go.mod is never rewritten. Discovery reads `replace ( ... )` blocks
  and mounts `../x/sub` as `x`. `replace` that climbs past the worktree's
  parent, and a go.mod not at the repo root, do not resolve on the host.
- Migration: on first sight `.dun/dun.local.json` and `.dun/agents_read` are
  **copied** in (the original is the user's file in the user's tree), logged
  once. Old `<repo>/.dun/worktrees/*` is still listed, reused and pruned;
  nothing new goes there.
- The AGENTS.md guard's state path reaches the hook through
  `DUN_AGENTS_STATE`.
- A `/rag auto` made in a linked worktree belongs to that worktree (its
  overlay); pruning a session tree also removes its overlay.
- poly-lsp-mcp still writes `<root>/.poly-lsp-mcp/cache.gob` into the repo; it
  has no flag to put it elsewhere and is owned by another project.

## dun's own tools in /mcp (2026-10-28)

/mcp lists dun's built-in tools as groups beside the servers — `dun:core`,
`dun:files`, `dun:agents`, `dun:peers`, `dun:loops`, `dun:recap`,
`dun:memory`, `dun:skills`, `dun:mount` — each switched on, off or unset per
scope like a server (`/mcp off dun:peers --project`), and each of its tools
alone from `t` (`/mcp toggle dun:files file_write off`). The nearest scope
decides: a tool's own switch, then its group's; unset everywhere, on.
`dun:core` (exec, ask_user, session, read_saved) is shown but cannot be
switched off. A tool switched off leaves the definitions and is refused if
called. Stored in `builtins.json` at each scope (builtins.go). A switch
changes the listed tools, the prompt's prefix: the server re-reads the
conversation once.
