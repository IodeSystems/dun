# Sessions and dund

Why the session service is built the way it is. Wire format: `event-protocol.md`.
Short rules about supervision: "Sessions, supervision and diagnosis" in
`decisions.md`. Code: `proto/`, `internal/dund/`, `internal/dundapi/`,
`cmd/dun/attach.go`.

## Shape and owner choices

- The engine is its own process (`dun -p`), JSON lines over stdio. dund
  (`dun dund`, same binary) runs one engine per session as a child, numbers its
  events, and lets any number of clients attach. The TUI always attaches; it has
  no direct-spawn mode. Quitting the TUI leaves the session and its background
  jobs running.
- A session is visible only to its owner (the principal that created it).
  A session has a member list (owner only for now). Sharing is not built.
- **UI state is per (session, principal)**: read position, draft, sent-input
  history, open agent view, focus, collapsed blocks. It is shared by that
  user's devices and never by other users. **Session toggles are one value per
  session** (rephrase, suggest mode, docker, model, servers); their events say
  who changed them. Stored today: draft and sent history. Scroll position,
  agent scope and collapsed blocks are not.
- Local transport is a unix socket, 0600; the owner is `unix:<uid>` from
  SO_PEERCRED. A non-member gets "no such session", not "forbidden".
  Engine flags from clients pass an allowlist (`allowedFlags`, no `--key`, no
  `--resume`); the engine reads the saved key itself.
- Canon followed for the browser surface: one handler serves REST and GraphQL
  (huma + gat, `/api/v1`, `/api/graphql`), live events by graphql-transport-ws
  with an HMAC-gated subscribe, behind hz over HTTP only. Divergences, both
  recorded in `~/doc/patterns/caveats.md`: React Native + react-native-web
  instead of a plain web client (one codebase for web and phone), and OIDC
  validated in the daemon.

## Which TUI state belongs to whom

The test for moving the TUI onto dund was: can this state be rebuilt from
events?

- **Session** (every client the same, must come from events): conversation,
  streaming text, pending tool, branch, busy, the open ask, agents, jobs,
  skills and prompts, retry banner, queued count, context stats, model and
  provider, suggest mode.
- **Per user** (dund, per principal): draft, sent history, focus, selection,
  activity selection, agent scope, search, read position, ask selection in
  progress, suggestion list.
- **Local only**: viewport, caches, size, spinner, overlays and forms while
  open, mouse mode, idle clock, trace, replay.
- **Supervision** (dund's, not the TUI's): restart counters, whether the
  engine was ever up, reload.

A second client needed events the engine did not send: `user` (an echo of every
message taken, `queued` when mid-turn), `turn` (kind user or continue; a turn
started by a background wake has no message), `answered`, and `settings`
(model, provider, url, rephrase, suggest; a state type). Clients render from
these, never from what they themselves sent.

## seq, snapshot, restart

- **dund's `proto.Log` numbers events, not the engine**, so seq survives an
  engine restart. The Log keeps 20 000 events and the latest event of each
  state type.
- A subscriber asking after a seq the Log no longer holds (`ErrGap`), or from
  another **epoch** (every Log stamps one on its events; a dund restart makes a
  new Log, so old seqs mean nothing), gets a **snapshot** built from the
  session file instead. `history` therefore means "replace the scrollback",
  and clients must treat it so.
- **A backlog over 256 events (`maxReplay`) is also sent as a snapshot.** Root
  cause of the old "lost the session service" report: replaying a long
  session overflowed the connection's queue and `send` closed the connection
  silently. Now a slow subscriber is dropped with `ErrSlow` and resubscribes
  after its last seq, and `send` waits up to 10 s (logged) instead of closing.
- **Restart policy**: a crashed engine restarts with `--resume <id>`, at most 3
  per 2 minutes, then `engine{gave_up}`; the next command retries. A crash loop
  is reported, not hidden. A stop on request does not restart.
- Idle engines stop after 30 minutes with no subscriber, turn, ask, job or
  agent, and restart on the next command or subscribe. The same conversation
  resumes, so nothing is lost.
- Socket path limit is 107 bytes (per OS). A long `$DUN_HOME` puts the socket in
  `$XDG_RUNTIME_DIR/dund-<hash>.sock`; `--socket` overrides; a path still too
  long fails with a clear error.
- dund re-execs into a new build once nothing is busy.

## Reuse of idle sessions

Plain `dun` attaches to the workspace's newest session that has no client
attached and has the same engine args. dund decides, atomically
(`Create{ReuseIdle}`, a 30 s hold, `reuseHold`), so two `dun` starts cannot
take the same session. `/new` and `--new` ask for a fresh one. Consequences:
after `/model` (different engine args) plain `dun` makes a new session, and a
served web terminal also takes idle sessions. Suggestions are not generated
before the first user message and do not carry across /clear, /new and /resume.

## Coordinated upgrade

Goal (USER, 2026-10-24): "Why make the user do that themselves?" A new dund
build must not cut work short, and no one should have to coordinate it.

- dund broadcasts `upgrade {state, version, waiting, deadline}` to every
  connection. The TUI status row and the web banner name each wait and the key
  that ends it (ctrl+c, `/jobs kill`).
- Restart happens when idle, on `/upgrade now` or "Restart now", or after
  `upgradeGrace` (10 min).
- A restart that cuts work (also SIGTERM and a service restart) first
  interrupts turns with a reason (`Interrupt(reason)`), flags `resume` in the
  registry, and the next dund wakes only those sessions with a user message
  starting `[automatic] ` (`resumePrefix`; killed jobs are listed, and a cut
  `ask_user` is not replayed, so the agent is asked to re-ask). The marker is
  that text prefix, not a transcript tag.
- A transcript that ends mid-turn and is under 30 minutes old at start is a
  crash and is woken the same way (`crashWindow`).
- The TUI re-execs into its own binary when that is dund's new build; the web
  app reloads on a new build.

## Remote API

- **gat calls operations in-process, so huma per-operation middleware does not
  run for GraphQL** (found by a test, then confirmed in gwag). gwag v2.1.3
  fixed this for later releases; dund keeps its own defence regardless: one
  HTTP middleware in front of all of `/api` checks the token, and handlers
  refuse an empty principal (`internal/dundapi/api.go`).
- `sessionEvents` is one handler on three surfaces (`gat.RegisterSSE`): SSE at
  `/api/v1/session-events`, `Subscription.dund_sessionEvents` over
  graphql-transport-ws, and gRPC (not served). gwag checks the token before
  the handler on every surface (401 / error frame 401 / Unauthenticated), so no
  handler runs unauthenticated. REST SSE needs the bearer and the HMAC from
  `subscribeToken` (principal|session|ts, in-memory secret, +/-5 min). The
  WS upgrade skips the bearer check but queries over the socket still find no
  principal. Replay-after-seq comes from dund's Log, because gat's pubsub keeps
  no history. A long-poll (`pollSessionEvents`) stays for simple clients.
- **Loopback needs a token too.** Loopback TCP is reachable by every local
  user. The local token (`dun dund token`) is generated into `$DUN_HOME`, only
  its SHA-256 is stored, and it is compared in constant time. The 0600 unix
  socket keeps peer-uid auth.
- The server URL in the app defaults to the hz name and can be overridden in
  Settings; this is how a standalone install is reached.

## Sign-in (OIDC)

Decided by the USER 2026-10-21, modelled on corrallm (`internal/dundapi/oidc.go`):

- dund runs `/auth/login` to the provider (Zitadel) with PKCE and **auth method
  None**. A client with no secret is refused Basic auth, so `client_id` goes in
  the body (`oauth2.AuthStyleInParams`).
- The callback verifies the ID token (audience = client id, nonce,
  `email_verified`) and checks an email allow-list. **Every allowed person maps
  to the unix owner**, so the phone sees the TUI's sessions. dund then mints its
  own sign-in token (hashed), returned in the URL **fragment**, never a query,
  because servers and proxies log queries.
- The issuer must byte-match the provider's (go-oidc compares exactly; no
  trailing slash). The phone app goes through dund's `https` callback too, so
  the provider needs one redirect URI only.
- No Zitadel admin credential exists anywhere; the provider app is created by
  hand in the console.
- corrallm probes the authorize endpoint (302 good; 400 app-not-found or
  redirect-uri-missing bad) as a preflight. dund does not.

## Slash commands are data

The engine publishes its commands in a `commands` event (name, usage,
description, choices, group, route, current toggle state) and routes typed
lines itself (control `slash` to `routeSlash`). The TUI, web palette and phone
all use that router, so a command added to the engine reaches every client.
The TUI keeps only commands that need the terminal (help, config, model,
context, reload, perf, mouse, scrolldebug, resume, trace, reconnect, skill,
exit, and the `/mcp add` form); a test pins that none of them shadows an engine
command. Provider management follows the same split: `applyProviderOp` is the
one implementation, called directly by the TUI (it must work when no engine
starts) and through control `provider` for web and phone.

`ask_user` needs at least two options and a `recommended` one (not for
multi-select), else the call fails back to the model (USER). The TUI marks and
preselects it; the web stars it.
