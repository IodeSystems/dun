---
question: What is the right shape for a Go test in dun that exercises
two-worktree concurrent-suite isolation (D4 spec sp_1m7vxftqq3fvhepg8faz3y),
robust to the flake-under-load that killed the earlier attempt?
source: st_1m85mwea4fmas61gz0jk4n (research), 2026-11-24
---

## The flake's root cause does not apply to dun

The flake under load (aw4 issue i_1m824y53s379s08k80xh3a), `d4/d4_test.go`,
`bin/testpg`, and the Postgres-connection-exhaustion analysis all belong to the
**aw4** codebase. `dun.dir.grep` over dun's tree finds no `d4/` path, no
`testpg`, no `postgres://`, no `pq.` driver. The resource that killed the aw4
test at load avg ~21 — a shared Postgres on 5433 at the default
max_connections 100, fed by unbounded per-test pools from two concurrent
suites — is not a shared surface in dun's suite. That removes the dominant
hazard the original test hit, and simplifies the guard accordingly.

## What the suite already isolates

All five `TestMain`s (cmd/dun, internal/tui, internal/harness, internal/dund,
internal/dundapi) move `HOME` and `DUN_HOME` to per-process temp dirs; the
tool daemons (poly-lsp-mcp, raglit) are pointed at per-test temp dirs;
`DUN_REGISTRY=off` keeps it off the network; every HTTP server in the suite is
`httptest.NewServer` (ephemeral ports). There are no fixed listening ports, no
shared pid files, no shared DB, no fixed worktree paths.

The one resource two concurrent runs DO share is `GOCACHE` and `GOMODCACHE`,
which `keepGoCaches()` in cmd/dun/main_test.go deliberately keeps shared (so a
`go build` inside a test does not recompile from an empty cache). That is safe:
the go build cache is content-addressed and lock-protected, so two concurrent
`go test` runs share it cleanly; worst case is a redundant build, never a
corruption.

## The shape of the guard

1. **Run a bounded subset in each worktree, not the full `go test ./...`.**
   The full suite is ~108–219s wall and CPU-heavy (internal/tui,
   internal/harness). Running it twice concurrently reproduces the load
   condition that flaked aw4. The property being guarded — "two independent
   checkouts of the same commit do not interfere" — does not require the full
   suite. Run the same small real subset in both worktrees (e.g. a fast
   package like `./internal/dund/...`, ~6s, that touches worktree/registry
   code). The SAME subset in both is what proves no cross-contamination. Use
   `-count=1` so the test cache cannot mask a real pass.

2. **Create/tear down the second worktree with plain `git worktree add` under
   `t.TempDir()`.** Do not use `harness.NewWorktree`: it couples the test to
   `$DUN_HOME`'s project store and mount symlinks, which the isolation property
   does not need. `git -C <repo> worktree add -b <branch> <tmpdir>/wt2 HEAD`
   into a `t.TempDir()`-parented dir is the cleanest form; t.TempDir
   auto-removes the parent, and a best-effort `git worktree remove` in cleanup
   handles the `.git` entry. The `gitrun` helper in
   internal/harness/isolation_test.go is the model to reuse.

3. **Assert pass/fail, never wall-clock; bound per-worktree parallelism.** The
   flake was resource EXHAUSTION under load, so the guard must not gate on
   timing: assert both suites exit 0, with a generous outer deadline so
   scheduling jitter does not wall-clock-kill a run that would otherwise pass.
   Cap per-worktree `-p` (the aw4 test used `-p=4`) so two concurrent suites
   do not stack 2× the cores.

4. **Place the test in `internal/harness/isolation_test.go`.** It already holds
   the worktree isolation tests (`TestWorktree_IsolatesChanges`,
   `TestWorktree_SymlinksItsMounts`) and the `gitrun` helper, and it tests
   worktrees with bare git and no DUN_HOME coupling — the same shape this test
   needs. `TestTwoWorktreesRunTheSuiteAtOnceAndBothPass` fits there.

## Bottom line

For dun the guard is far simpler and safer than the aw4 original: no DB, no
fixed ports, no shared pid files; the suite is already hermetic per worktree.
Two `git worktree add`'d checkouts of the same commit, each running the same
small subset with `-count=1` and capped `-p`, both must exit 0 — asserted on
pass/fail only.
