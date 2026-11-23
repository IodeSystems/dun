# dun: shared-state audit for two concurrent `make test` runs (sp_1m7vxftqq3fvhepg8faz3y)

Audited on main@69f77450c. Question: what would a second, independent worktree
running the full suite at the same time hit? Answer: nothing that fails a run.
One guarded fixed port.

## Ports

Every `net.Listen` site in the tree, classified:

- **Unix sockets, per-test temp paths — safe.** `internal/dund/server_test.go`
  (67, 157, 180, 222, 251, 296), `internal/dund/fakelink_test.go:166`,
  `internal/dund/rag_test.go:20`, `internal/dund/procs_rag_test.go:21`,
  `internal/dund/reuse_test.go:136`, `internal/dund/upgrade_test.go:27`,
  `internal/harness/memrecall_dund_test.go:52`, `internal/tui/autoupgrade_test.go:40`,
  `internal/tui/notices_test.go:30`, `internal/tui/upgrade_test.go:190,253` —
  all on paths under `t.TempDir()`. `internal/tui/reconnect_test.go:120` and
  `internal/tui/release_test.go:104` use `dundcli.Socket(DUN_HOME)`, but
  `DUN_HOME` is a per-process temp dir (see Home below), so the two worktrees'
  sockets never coincide.
- **TCP, free port (`:0`) — safe.** `internal/dundcli/web_test.go:24,127,194`,
  `internal/dundcli/webctl_test.go:87`, `internal/dundcli/handover_test.go:161`,
  `internal/harness/gateway.go:425`, `internal/harness/netwait_test.go:49`,
  `internal/oauth/oauthtest/oauthtest.go:218` (`FreePort`),
  `internal/tui/login_test.go:225`, `tools/ux/stub.go:41` (flag default
  `127.0.0.1:0`).
- **The one fixed port.** `internal/oauth/mcpoauth.go:61`
  (`DefaultRedirectPort = 33418`) is bound by `Client.Login` at
  `internal/oauth/mcpoauth.go:480`. In tests it is only probed, not bound:
  `internal/oauth/oauthbroker_test.go:74` does a listen-then-close and
  `t.Skipf("port %d is in use here", DefaultRedirectPort)` on failure, so the
  worst case for a concurrent run is a SKIP, never a FAIL. A second worktree
  suite does not bind 33418, so two suites cannot collide on it; only a real
  `dund`/`dun` process using the default OAuth redirect port on the same box
  would trigger the skip.
- `127.0.0.1:<n>` literals elsewhere are request URLs or asserted strings, not
  sockets (e.g. `cmd/dun/*_test.go` harness endpoints, `internal/tui/repaintreport_test.go`).

## Home directories

Every test package pins its home to a fresh temp dir in `TestMain`:

- `cmd/dun/main_test.go:13` — temp `DUN_HOME` **and** `HOME`, plus
  `POLY_LSP_NO_DAEMON`, `POLY_LSP_HOME`, `XDG_RUNTIME_DIR`, `RAGLIT_ROOT`,
  `DUN_REGISTRY=off`; `keepGoCaches` pins `GOCACHE`/`GOMODCACHE` so the build
  cache is shared (shared read/write cache is fine: go's cache is
  multi-process safe).
- `internal/dund/main_test.go:11` — temp `DUN_HOME`.
- `internal/tui/main_test.go:19` — same shape as cmd/dun.
- `internal/harness/mcpuser_test.go:28` — temp `DUN_HOME`, fake `userHomeDir`,
  `isolateTools(home)`.
- `internal/dundapi/main_test.go:18` — no home at all; it patches
  `http.DefaultTransport`'s dialer to resolve `dun.localhost` to 127.0.0.1,
  which is process-local state, and its servers are `httptest` (free ports).
- `internal/oauth` has no `TestMain`; every test that touches home does
  `t.Setenv("DUN_HOME", t.TempDir())` (verified in oauthbroker_test.go).

So neither suite writes to `~/.dun`, and the two worktrees' `$DUN_HOME` trees
are disjoint.

## Files written outside package dirs

- `os.MkdirTemp`/`os.CreateTemp` in tests all get OS temp dirs with random
  suffixes — two runs cannot pick the same name.
- `internal/harness/files_test.go:235` uses `/tmp/dun-readtmp-` (random
  suffix, same story).
- No test writes to a fixed machine-wide path.

## Locks, databases, external services

- No `flock`/`Lockf` anywhere in the tree; the `.lock`/`.pid` hits in
  `internal/dund` are all in-memory `sync.Mutex`es, and pid files are read
  under the per-package temp home.
- No database: the suite has no SQL driver and no external service; network
  is `httptest` and the in-process `oauthtest` world. `cmd/dun` pins
  `DUN_REGISTRY=off` so no registry traffic.
- Residual shared state between two concurrent suites is host-level only:
  CPU, memory, and the Go build/module caches (safe by construction).

## Verdict

Safe as-is. No dev subtask is required before the recorded two-worktree run.
The only fixed resource is the OAuth redirect port 33418, which is guarded by
an explicit skip, and two suites cannot collide on it because only a live
dun process binds it.
