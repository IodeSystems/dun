# MCP servers, tools and project state

Why the tool-server layer is built the way it is. Short rules on startup,
rebuilds and config files: "Tool servers and extensions" in `decisions.md`.
Switch scopes (session, project, global): `mcpscope.go`.

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
`file_read`, `file_edit`, `file_write`, `file_grep`, `file_list` on by default,
`file_delete` and `file_move` opt in. USER wanted a toolset "minimal,
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
