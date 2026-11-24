# Spike: DUN_HOME consumers and spawn-env audit

Base: main@1156318df, 2026-11-22.

## 1. DUN_HOME consumers in harness non-test code

Every call site that resolves the home directory via `DunHome()` (session.go:378)
or `dunhome.Dir()` (dunhome/dunhome.go), in non-test `.go` files:

| File | Function / line | What it stores under home |
|------|----------------|--------------------------|
| session.go:28 | `SessionsDir()` | `sessions/<root>/` |
| session.go:378 | `DunHome()` | (the resolver itself) |
| capstore.go:35 | `CapsDir()` | `capabilities/<hash>.json` |
| effort.go:94 | `effortFile()` | `effort.json` |
| effort.go:136 | (MkdirAll) | creates home dir |
| rephrase.go:54 | `rephraseFile()` | `rephrase.txt` |
| rephrase.go:73 | (MkdirAll) | creates home dir |
| window.go:72 | `contextFile()` | `context.json` |
| window.go:102 | (MkdirAll) | creates home dir |
| roundstats.go:306 | `DefaultStatsDir()` | `stats/` |
| spend.go:10 | `DefaultLedgerDir()` | `spend/` |
| images.go:217 | (vision.json path) | `vision.json` |
| images.go:108 | (error message) | `config.json` (display only) |
| raglitauth.go:48 | `RaglitTokenFile()` | `oauth/raglit.token` |
| raglitauth.go:51 | `raglitKeyIDFile()` | `oauth/raglit.key` |
| codeguard.go:57 | (poly-lsp-empty-root) | `poly-lsp-empty-root` |
| nodir.go:21 | (nodir) | `nodir` |
| repaint.go:29 | (bugreports) | `bugreports/` |
| subisolation.go:106 | (nodir) | `nodir` |
| switches.go:106 | (kind.json) | `<kind>.json` |

That is **19 distinct call sites in 15 non-test files**, all funneled through
`DunHome()` → `SessionsDir()` → `dunhome.Dir()`.

The plan's approach (store resolved home in `Config.Home`, thread it through a
method on `*Harness`) covers all of them: every call site is inside a harness
method or a package-level helper that a harness method calls. The package-level
helpers (`SessionsDir`, `CapsDir`, `effortFile`, etc.) need a home parameter
added, or a `*Harness` receiver, so they stop calling `DunHome()` directly.

## 2. Spawned subprocesses that read DUN_HOME

**None.** This is the critical finding for the plan.

The test suite spawns subprocesses in four ways:

1. **fakebin_test.go re-exec**: `fakeBin()` writes a shell wrapper that
   `exec`s the test binary itself with `DUN_FAKE_BIN=<persona>`. The re-exec'd
   process enters `TestMain`, sees `DUN_FAKE_BIN` set, calls
   `fakeBinMain(persona)` and returns — it never runs the tests and never
   calls `t.Setenv`. The fake server code (`serveFake`, `fakeBinMain`) reads
   only `DUN_FAKE_*` env vars; it does not call `DunHome()` or
   `dunhome.Dir()`. The inherited `DUN_HOME` value is present in the
   subprocess environment but unused.

2. **`serveDund` (memrecall_dund_test.go)**: creates `dund.New(t.TempDir())`
   and `ragd.Open(t.TempDir())` in-process. No subprocess, no DUN_HOME.

3. **extension_test.go**: spawns `python3 server.py`
   (testdata/extensions/hello/). The Python server is a self-contained
   JSON-RPC loop; it reads no env vars at all.

4. **git / go build / docker in test helpers**: `commit_test.go`,
   `project_test.go`, `isolation_test.go`, `subisolation_test.go`,
   `servers_runtime_test.go`, `ledger_test.go` spawn `git` or `go build`
   subprocesses. None read DUN_HOME.

5. **prune.go / indexkind.go / indexstate.go / lspkind.go / raglitauth.go /
   mcpdocs.go**: production code that spawns the real raglit or poly-lsp
   binary. In tests, these are replaced by the fakeBin re-exec (case 1 above),
   which does not read DUN_HOME.

**Conclusion: every test file that sets `DUN_HOME` can drop the `t.Setenv`
and use `Config.Home` without changing subprocess behaviour.**

## 3. Test file classification

86 test files contain `t.Setenv("DUN_HOME", …)`. They split into three groups:

### Pure DUN_HOME-only (73 files) — convert to `Config.Home` + `t.Parallel()`

agent_events, asktool, attach, autoturn, builtin_servers, builtins, capstore,
codesearch, compacting, effort, emptyturn, exec_nooutput, extcmd, extension,
fileaccess, files, fold, gateway, idlecheck, indexanyway, indexkind,
indexstate, instance_agent, instance, isolation, ledger, loopctl, lspkind,
lspsafe, mcpdefault, mcpscope, mcptools, mcpuser, memory, memrecall_dund,
memrecall, midreply, notekinds, notices, notools, peers, plannudge, project,
prompttrim, prune, rag_ingest, ragproject, ragsetup, ragworkspace, repaint,
replyformat, roles, rootguard, rules, rules_user, serverlog, serverrestart,
servers_oauth, session_id, sessiondirs, sessionindex_dund, settle, setupstate,
skills, skillview, subagent_ask, subagent_dir, subagent_persist,
subagent_provider, subagent_wake, subisolation, switches,
systemprompt_alone, textcall, turnlimit

These 73 files set DUN_HOME and no other env var, and mutate no package
global. They are the mechanical batch for subtasks 3a–3c (25 files each,
roughly).

### Special: slowstart_test.go (1 file)

Sets DUN_HOME **and** mutates the `serverStartGrace` package global
(servers_runtime.go:290, two tests at lines 19 and 98). Can convert DUN_HOME →
Config.Home, but stays serial because of the global. This is the "remaining"
subtask in the plan.

### Multi-env-var (12 files) — stay serial, DUN_HOME partially convertible

| File | Other env var(s) set in the same test |
|------|---------------------------------------|
| asidettl_test.go | `DUN_ASIDE_ESCALATE_MS` |
| compaction_test.go | `DUN_CONTEXT_TOKENS` |
| fileadd_test.go | `PATH` (one test, line 131) |
| forget_test.go | `TMPDIR` |
| harnessinfo_test.go | `TMPDIR` |
| isotmp_test.go | `TMPDIR` |
| jobs_test.go | `TMPDIR` |
| netwait_test.go | `DUN_TURN_RETRY_BUDGET` |
| raglitauth_test.go | `DUN_RAGLIT` |
| recap_test.go | `TMPDIR` |
| recapstub_test.go | `TMPDIR` |
| retry_test.go | `DUN_TURN_RETRY_BUDGET`, `DUN_RETRY_BUDGET`, `DUN_RETRY_5XX_ATTEMPTS` |

These 12 files can have their DUN_HOME `t.Setenv` removed (replaced by
`Config.Home`), but cannot add `t.Parallel()` because the other env var
still forbids it.

Note: the prior research (s_1m8567n4kf63a3vg4wkxyx) counted 11 multi-env files;
this audit finds 12. The extra one is `jobs_test.go` (TMPDIR at line 118).

### session_id_test.go note

Line 13: `t.Setenv("DUN_HOME", "")` — this test exercises the empty-DUN_HOME
fallback path. With `Config.Home`, the equivalent is `Home: ""` (zero value),
which is already the default. The test converts cleanly.

## 4. TestMain (mcpuser_test.go)

`TestMain` sets `DUN_HOME` globally for the entire test binary (line 37),
plus `userHomeDir` and `isolateTools(home)`. After the Config.Home threading,
`TestMain`'s `os.Setenv("DUN_HOME", home)` becomes a safety net for any code
path that still calls `DunHome()` directly. It should be kept until all
non-test code paths are threaded through Config, then removed.

## 5. Summary for the plan

- **73 files** are mechanically convertible: remove `t.Setenv("DUN_HOME", …)`,
  add `Home: t.TempDir()` to their `Config`, add `t.Parallel()`.
- **1 file** (slowstart) converts DUN_HOME but stays serial (package global).
- **12 files** convert DUN_HOME → Config.Home but stay serial (other env var).
- **0 files** need to keep `t.Setenv("DUN_HOME")` for a spawned subprocess.
- The 19 non-test call sites all go through `DunHome()` / `dunhome.Dir()` and
  are reachable from `*Harness` methods, so the Config.Home threading is
  straightforward.
