---
question: Where does make test spend its time, and what was cut?
source: test speed-up 2026-11-18; aw4 i_1m7rcycns7p3f1fk52rzq7, i_1m7rcydjs33e96z8aghtwq
---
Measured with `GOWORK=off go test -json ./...` (make test's flags: none) on
the 24-core dev box. The box was shared: load average 81 for "before", 32-45
for "after" (`-count=1`, so nothing came from the test cache). Part of the
drop in internal/tui and internal/harness is the lower load; cmd/dun and the
named tests below changed for the reasons given.

Wall time of the go test step: 219 s before, 108 s after. It is the
slowest package plus the build: packages run side by side (`-p` defaults
to GOMAXPROCS, 24 here), and inside the slow packages tests run one after
another: before, no test in internal/tui, cmd/dun, internal/harness or
internal/ragd called t.Parallel; 26 tui, 18 cmd/dun and 88 harness test
files use t.Setenv, which forbids it in that test.

| package | before (s) | after (s) |
|---|---:|---:|
| internal/tui | 197.6 | 96.2 |
| cmd/dun | 165.1 | 14.6 |
| internal/harness | 139.4 | 104.7 |
| internal/ragd | 35.0 | 30.0 |
| internal/dund | 6.8 | 6.1 |
| internal/dundapi | 3.9 | 2.4 |
| internal/update | 3.1 | 2.2 |
| internal/mcpserver | 2.1 | 1.6 |
| others | < 2 each | < 2 each |

The 20 slowest top-level tests before:

| # | test | package | before (s) | after (s) |
|---:|---|---|---:|---:|
| 1 | TestAdoptAcrossDundReexec | cmd/dun | 86.79 | 8.14 |
| 2 | TestResize_OffsetStaysInRangeAcrossEveryHeightChange | internal/tui | 45.79 | 15.63 |
| 3 | TestCLI_BareWordIsRefused | cmd/dun | 43.41 | 4.37 |
| 4 | TestNoShrink_RandomEventStreams | internal/tui | 41.41 | ~9 (8 parallel subtests, 8.8-9.3 each) |
| 5 | TestMCP_StartSettleOffByDefault | cmd/dun | 20.01 | 0.00 |
| 6 | TestScreen_ScrollingMatchesView | internal/tui | 5.59 | 1.22 |
| 7 | TestNoBounce_LiveSplashRewritesItsRowsInPlace | internal/tui | 5.17 | 1.19 |
| 8 | TestScrollFold_ScrolledUpWhileOutputArrives | internal/tui | 4.87 | 5.29 |
| 9 | TestFlags_RealBinary | cmd/dun | 4.82 | 4.59 |
| 10 | TestStart_AddedServerStartsInTheBackground | internal/harness | 4.29 | 4.28 |
| 11 | TestBox_DescendScrollAndLeave | internal/tui | 3.86 | 2.14 |
| 12 | TestBox_DoneKeepsTheHeight | internal/tui | 3.57 | 1.09 |
| 13 | TestStart_SlowServerJoinsAfterReady | internal/harness | 3.29 | 3.25 |
| 14 | TestBox_SelectionMovesPastBoxes | internal/tui | 3.23 | 1.66 |
| 15 | TestHostShell_DonationFreesTheShellForTheNextCommand | internal/harness | 3.09 | 3.04 |
| 16 | TestOfFile_RealBinaries | internal/update | 2.55 | 1.72 |
| 17 | TestMouse_ClickInBoxBodyDescends | internal/tui | 2.50 | 1.64 |
| 18 | TestServerLog_OffWhileStartingStopsTheStart | internal/harness | 2.42 | 2.36 |
| 19 | TestPrepareImage_Downscales | internal/harness | 2.39 | 1.77 |
| 20 | TestBox_TailFollowResumesOnLeave | internal/tui | 2.38 | 1.42 |

What was slow, and the change:
- cmd/dun: TestMain moved HOME to a temp dir, so each `go build` a test ran
  used an empty GOCACHE and GOMODCACHE (a full recompile and module fetch,
  40-80 s a build; tests 1, 3 and 9 each paid it). TestMain now keeps the go
  command's own caches. The adoption test's two builds run side by side, and
  the built-binary tests are t.Parallel.
- TestMCP_StartSettleOffByDefault: the fake dund never answered the send, so
  each start waited out sendAckWait (10 s). It acks now.
- TestNoShrink_RandomEventStreams: event-only subtests, now t.Parallel.
- TestResize_*: the fixture typed 25 questions key by key; it pastes them.
- TestScreen_ScrollingMatchesView: a frameless message waited the 5 s settle
  deadline; 500 ms of silence is quiet now.

Left as they are, and why:
- internal/tui in general: tuiModel is 136 KB and copied by value per
  Update; about a third of CPU is GC (i_1m7rcycns7p3f1fk52rzq7, a product
  change). GOGC=400 gave 12-17 % on the package at 3x RSS: not applied.
- internal/harness: no single slow test; the long tail runs serially
  (i_1m7rcydjs33e96z8aghtwq). The 2-4 s tests wait for a real timeout, job
  threshold or SIGKILL grace.
- internal/ragd: 18 tests at about 2 s each, below harness's time; not
  looked into.
