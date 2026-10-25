# Panes (design, not built yet)

USER 2026-10-25: the TUI needs panes that come and go on alternate screens.
The first case is `/mcp`: the server table on top, the selected server's
activity log in a pane below it. This file is the design; the work item is an
aw4 issue in project dun.

## What there is now

The layout is a fixed stack in `drawView` (`cmd/dun/tui.go`). From the top:
the head row, the activity strip (only when it has rows), the conversation,
one divider, the lower area, the upgrade bar (only when there is one), and the
status row. Only the conversation's height is left over; every other region
is measured (`convoHeightBudget`).

Alternate views are flags, and each has its own key routing, decided by the
order of the if-chain in `update`:

| flag | replaces |
|---|---|
| `inspecting` | the whole screen |
| `paging` (`/mcp`, `/mcp tools`, `/skill`, `/context-file`, `pg.id` holds the view) | the conversation |
| `jobViewing` | the conversation |
| `asking`, `picking`, `modelPicking`, `mcpForm`, `rag`, the palette | the lower area |
| `searching`, `confirm`, flash | the status row |

Focus is one int with three values: input, convo, activity. Tab cycles them
(`cycleFocus`). The pager, the job view and asks do not change focus. The
divider leans up for any focus that is not the input.

The owner rules that pin the layout (`docs/ui-decisions.md`) are enforced in
the conversation's own code: no bounce (`holdTop`, `holdShrink`), total
height never decreases (`maxH`), panels do not push the conversation
(`panelHeld`), and the vertical axis rule (up/down at an edge scroll the
conversation). Tests: `bounce_test.go`, `noshrink_test.go`, `pane_test.go`,
`resize_anchor_test.go`, `TestFocus_ArrowsStopAtEveryEdge`.

## The model

- A **pane** is a region with a view and keys:
  `view(w, h) []string`, a height policy, `key(msg) (handled bool)`,
  `focusable()`, `hint()` (the status row's text while it has focus), and
  `mouse(x, y, msg)`.
- **Height policy**: `measured` (its own height, capped at a max: the
  activity strip, the lower area), or `fill` with a weight (the conversation,
  a log). Fill panes split what the measured ones leave, by weight, with a
  minimum each.
- A **screen** is an ordered list of panes, with one focused. The head and
  status rows stay outside the panes, as chrome.
- **Screens stack.** The base screen is activity, conversation, lower area.
  `/mcp` pushes a screen: the table (fill 2), the log of the selected row
  (fill 1), and the lower area, so a typed command still works. esc in a
  pushed screen's top pane pops it. The inspector is a screen with one pane.
- **Dividers** are drawn between adjacent panes. A divider next to the
  focused pane leans toward it, as now (the ▁/▄ and ▔/▀ mirror pairs). A
  divider with focus on neither side is dim, with no bump.
- **Keys**: the screen handles the global keys first (ctrl+c, ctrl+g, tab
  and shift+tab to move focus between its focusable panes), then the focused
  pane. A pane that refuses up/down at its edge passes them to the screen's
  scroll pane (the conversation on the base screen, the table on `/mcp`):
  that is the vertical axis rule, kept.
- **The owner rules stay in the panes that need them.** The holds and `maxH`
  are the conversation pane's. The layout only gives each pane its rows; the
  conversation decides what fills them, as `refresh()` does now.

## Order of work

1. The types and the layout, with the three current regions wrapped as panes
   and no change in behaviour. The layout tests above pass unchanged. The
   divider takes N panes.
2. The pager's views become pushed screens. `/mcp` gets the log pane, the
   first new use.
3. The job view and the inspector move onto screens.
4. `update`'s if-chain is replaced by the screen's dispatch. Lower-area
   modes (ask, pickers, forms, palette) stay variants of the lower pane.

## Risks

- `refresh()` has four jobs (wrap, measure, set the viewport, decide the
  scroll) and about 100 callers. `ui-decisions.md` says to split content
  from scroll policy before changing that code. Step 1 must not need it; if
  it does, the split goes first.
- Mouse routing uses fixed row arithmetic (`actTopRow`, `convoTopRow`,
  `lowerTopRow`). It moves onto pane rectangles in step 1.
- The holds assume the conversation's top row on screen is fixed while a
  panel opens. A pushed screen changes the rows the conversation gets; it is
  not drawn while the screen is up, so it must keep its offset, not
  re-anchor, when the screen pops.

## Decisions for the owner

- Under a pushed screen, does the lower area stay (type a command while
  `/mcp` is open), or does the screen take the whole height?
- With three panes, does tab visit the lower area, or only the screen's own
  panes?
