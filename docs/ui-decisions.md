# TUI decisions

Rules for `cmd/dun`, with the reasons. Most are the owner's calls; they were
made after seeing the failure on a real terminal, usually a phone over tmux.
Structure and cache details are in `tui-architecture.md`. Engine-side rules
are in `decisions.md`.

## Standing owner rules

- **No bounce.** A change that adds rows keeps the view's top row (a pinned
  view follows the bottom). A change that removes rows keeps the top row and
  fills blanks below, only down to the bottom of the view as it was, so the
  furthest scroll is where the view is (`holdTop`, `holdShrink`,
  `cmd/dun/askpark.go`, `bounce_test.go`). Cause of the original bug: the ask
  picker is several rows, the composer one; on close the pane grew, pinned to
  the bottom, and every row moved. A terminal resize (the phone keyboard) still
  anchors the bottom. Holds end when content reaches them, at a new width, and
  on reset, session switch and repaint. A shrink while holding keeps the top
  row and cuts the hold (a held form at 48 columns once left the pane blank).
- **Total height never decreases (USER, 2026-10-22).** A tool run and the
  thinking between its calls form one grow-only box; every block is held at the
  most rows it has drawn (`maxH`; property test `noshrink_test.go`). Thinking
  is a bounded box (USER: "expand to some reasonable size, and subscroll
  there"): it grows to min(10, pane/3) rows, then scrolls inside, following the
  tail while live.
- **Focus moves by tab only (USER, 2026-10-23).** Quote: "the movement between
  panes is incredibly obnoxious ... arrow key back shouldn't go between the
  focus groups. it should be tab, and ctrl+tab should be the insert raw tab".
  Arrows never change zone; at an edge they are no-ops
  (`TestFocus_ArrowsStopAtEveryEdge`). Exceptions: right arrow on an empty
  composer accepts the suggestion; left leaves an agent's scope; up/down at the
  composer's edge scroll the conversation.
- **Vertical axis rule.** Up/down move inside whatever you are in (composer
  caret, suggestion picker, palette, ask options) and at its edge fall through
  and scroll the conversation. Reason: on Termux with mouse tracking off, a
  finger swipe arrives as plain up/down keys. A question you cannot scroll
  behind is one you cannot answer. History moves to ctrl+up/down (and up on an
  empty composer).
- **Focus is a half-line divider in the accent colour, leaning toward the
  focused pane. Never lower a pane's contrast for focus.** The owner's cue;
  do not recommend dimming.
- **Process stays visible.** Calls stay in the turn, one readable line each;
  never hide or demote how an answer was produced. Server-prefixed tool names
  are shown as words (`notes · project notes`); logs, inspector and copy text
  keep the real name.
- **Contrast.** `stSecondary` (246 on dark, 240 on light) is for content one
  step below the answer: previews and expanded bodies. `stDim` (240, 2.5:1) is
  for chrome only (arrows, rules, hints, labels). Orange means attention
  (a failed job, not-logged-in; an old transcript's blocked agent); neutral facts are not
  orange. Answer prose wraps at `bandCap` (100) columns.
- **Wide blocks scroll sideways, never wrap** (`wide.go`): a code block or
  table wider than the prose column renders at its own width, is cut at the pane
  edge with `... +N cols`, and scrolls with left/right. The streaming reply
  renders the same way, so finishing moves no row.
- **Text is selected, then descended with `▸`.** Right arrow opens and
  descends, left ascends: the same gesture on the activity strip, its rows,
  a job's log and the docs blocks. Selection is stored by identity, not index,
  because rows are rebuilt on every event and a new agent would shift an index
  under the reader's eyes.
- **Panels do not push the conversation.** A panel opening below (palette,
  form, picker) keeps the view's top row; it takes the blank rows, then covers
  the newest rows (`panelHeld`). Draft text survives an ask: the composer is
  copied when a question opens and restored on every kind of close.
- **First-run copy.** "AI service" replaces "provider" on first-run surfaces
  only. A question asked before login is held and put back after the model pick.
  `/login` takes an address and a key; the name is derived from the address.
  `/mcp add` takes one field ("what you were given": a command, an address or a
  preset).
- **Slash commands: bare opens the view, complete acts, partial opens the view
  at that place** (USER 2026-10-24). `/<command>` alone opens its status and
  config view (as `/mcp` opens the switch table). Arguments are operations on
  the config: when they are complete the command does the thing with no view
  (`/mcp off vay --project`); when they are not, it opens the view at that
  section with what was given filled in (`/mcp add` with no target opens the
  add form). `/skill` and `/rules` follow it (`/rules` took over
  `/context-file`, USER 2026-10-25: one name for the rules files). A list's
  status (mode, cost, what it depends on, a warning) is a pane above it,
  never a row; under 80 columns a row is two lines, the name above its
  switches and cost.
- **The skill reader is a pane below the list** (owner, 2026-10-27): enter
  on a skill opens it; the reader has the keys (tab is its next link, not
  the next pane) until esc on the skill itself closes it. Under 80 columns
  it takes the whole screen instead of a split: at 48 columns a split left
  about ten rows of text, too few to read a skill. References are read the
  way authors already write them, no new syntax: a path to a file in the
  skill's folder is a link, an `@path` (Claude Code's import) is inlined
  (dun/skillview.go). The model's view of a skill does not change.
- **Splash rows are fixed height per width,** so the layout never grows as
  facts arrive (counted estimate, then exact, then spend). Context turns orange
  at 60% of the window (`contextHeavy`).

## Terminal mechanics

- **Mouse mode 1003 (any-motion).** tmux forwards the active pane's mouse mode
  to the outer terminal. Termux raised the keyboard on tap for claude's panes
  (1000/1002/1003/1006) but not dun's (1002 only). dun requests all-motion
  (`/mouse all|cell|off` switches live). Measurement trap: a pty capture of
  another TUI from an untrusted cwd stops at its trust prompt and shows no
  mouse mode; capture from a trusted directory.
- **Bubble Tea v2 replaced a hand-built scroll region.** v1's line renderer made
  dun grow its own DECSTBM scroll region, and that created a bug class: two
  owners of what the screen shows, with commands racing (a clear overtaking
  its sync left rows frozen while the offset moved). v2's renderer is a cell
  diff with hard-scroll optimisation, so the region, the key filter, the kitty
  probe and the SGR packer were deleted rather than ported. v2 negotiates
  kitty keyboard and modifyOtherKeys itself. `normalizeKey` folds shift+enter to
  ctrl+j (keypad enter decodes as plain "enter" and would send).
- **Measure bytes, not CPU.** dun felt slow over ssh on a phone while CPU was
  fine; the cost was 1.8 MB written for a 15 s stream. Colour packing and a
  scroll region cut it 17x (v1 era; v2's renderer now does the equivalent).
  `tea.WithFPS` was rejected: 7% fewer bytes for double the keystroke latency.
  `--replay` of a recorded stream makes byte counts reproducible.
- **A blocking terminal query is not a slow render.** Dependencies can query
  the terminal in package init (`lipgloss.HasDarkBackground`, a 5 s OSC 11
  timeout that bare terminals do not answer; tmux TERMs skip it). Probes must
  run in raw mode with a deadline and accept only a valid reply; an earlier
  probe did a blocking read on a canonical tty and needed an Enter to get
  past.
- **`/repaint` captures before it fixes** (`cmd/dun/repaint.go`): expected
  (the last View) against actual (`tmux capture-pane`), the diff rows, state and
  an event ring, under `$DUN_HOME/bugreports/`. `/repaint report` redacts
  deterministically, then optionally through the model, and prints a
  `gh issue create` line; it never runs gh. Reason: mangled screens were
  reported without evidence, and the next step is to ask which side diverged.

## Interaction details with a reason

- **Descending into a sub-agent (2026-10-27).** Enter or → on an agent row
  in the activity strip shows that child's conversation in the conversation
  pane, rendered like the root's and live while it runs (its history, then
  each transcript entry, through dund for every client). The strip becomes
  the header `↰ agent #N · <task> · ← esc back`; under 60 columns it is
  `↰ #N · <task> · ← esc`, and the task is what is cut, never the way back.
  The task is secondary, not dim: it is content. esc (or ←) returns to the
  root's conversation at the row the reader left it; the root's events that
  arrived meanwhile were applied to it, not to the child's view. A question
  from the root (ask_user) leaves the child's view, because it needs the
  screen.
- **Typing while descended tells the child** (a `jobs tell`: its next
  message), and the placeholder says so. Chosen over a read-only view
  because the reason to open a child is usually to correct it, and the
  other way (go back, ask the root to tell it) costs a root turn and the
  root's context to relay one sentence. The message is shown when the
  child's transcript takes it, as the root's are, so every client shows the
  same. A running child takes no tell (the engine refuses one between turns
  only): the draft stays in the composer and the status line says to wait
  for its turn to end, rather than sending words that would be dropped.

- **Interrupt.** ctrl+g interrupts a busy turn (`interruptKey`; ctrl+k was
  tried first and took the composer's kill-to-end-of-line, USER 2026-10-22).
  esc interrupts after every other esc consumer. ctrl+c interrupts too; a
  second ctrl+c within 2 s quits the TUI, and the session keeps running. The cut
  round's streamed reply is saved, an orphan call is answered with a
  user-interrupt result, and a marker notes the reasoning's duration and last
  sentence, never the reasoning itself. Typed messages stay pending and start
  the next turn at once. Running background jobs are listed in the marker,
  not stopped.
- **Killing jobs is explicit.** `/jobs kill`, `x` on a row, or
  `exec_monitor kill`; "kill all" asks y/n. Host shells get SIGTERM to the
  group, SIGKILL after 2 s; Docker uses `docker stop --time 2` on the named
  container. A killed job's completion notice says it was killed, not that it
  failed.
- **Edit an answer.** Tab or right arrow on an ask option opens the custom
  field holding what enter would send. `multi` is described to the model as
  "pick several"; models never set it before the description named the cases.
- **ctrl+tab types a tab.** Pasted tabs are kept (bracketed paste); a tab in
  the composer is a literal tab.
- **ask_user is one question at a time** (the answer to one guides the next).
  A turn that ends for any reason closes and settles its open question.
- **Where trust shows.** Agent previews show state and spend.
  Exec shows the verdict and the last informative line, not a bare `FAIL`.

## Why the scroll bugs hid

The scroll-overlay hunt (2026-08-06 to 08-09) cost a session and ended on code
that was correct. `scrollOverlay` was right the whole time; the frame it drew
into was one row too tall, Bubble Tea's renderer keeps the last h lines, and
the row was deleted before display. Nothing in the code or the tests could see
it, for structural reasons that shaped the current code:

- **No value represented the frame.** Geometry lived in three places written by
  different functions at different times: the viewport (offset and height),
  `convoEntry.rowOffset` (render output written back into model state, valid
  only until the next `refresh()`, with no way to tell it is stale), and a
  parallel `blockH` slice. `View` computed a fourth copy inline, and that copy
  drifted. The `frame` value in `cmd/dun/tui.go` is the answer: one pure
  layout holding rows, block heights, offsets, convo height and wrap width,
  stored as `m.frame`, so "does the frame fit the terminal" is a field
  comparison and not a rendered-string assertion.
- **Tests took the leaf shortcut.** Because layout was observable only as a
  rendered string, every overlay test called the leaf function and compared its
  output; the trace replay re-implemented the leaf's search inside the test. Ten
  tests, none able to fail on the real defect. Test the frame, not the leaf.
- **`refresh()` does four jobs** (wrap blocks, measure them, set the
  viewport content, decide where to scroll) and still has about 100 callers
  (`grep -c 'refresh()' cmd/dun/tui.go`). Most want only the first three.
  Scroll policy has several owners (`updateScrollPin`, `refresh`, the mouse
  handler, resize). A change in that code should split content from scroll
  policy first; do not start it cold, it has no user story of its own.
- **Measured cost of `refresh()`** (2026-08, a resume-sized conversation of
  about 7.2k rendered lines, 4.6 MB, 550k escape sequences, Ryzen 9). Fixes
  that got it: skip refresh when only the height moved, skip wrapping blocks
  that already fit, and stop handing the viewport a joined string it only
  splits back (`convoPane`).

  | | before | after | why |
  |---|---|---|---|
  | height-only resize | 12 ms | 23 us | refresh skipped |
  | width nudge (80 to 79) | 136 ms | 0.29 ms | blocks that fit skipped |
  | width halve (80 to 40) | 143 ms | 4.1 ms | genuine re-wrap |
  | token append | | 25 us | |

  The remaining 4.1 ms on a halving is real work. `refresh()` still
  re-renders every block per call; the constant is small enough that nobody
  notices, which is how it grows back.
