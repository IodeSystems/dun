# Product brief — dun

The fixed half of every dun run: who dun is for, what sets the bar, and what
dun must do that its category does not. Every persona reads it; Ives judges
against it (canon IVE-6). Owner decisions, 2026-10-23.

## Audience

People who work on code from a terminal: from an engineer who lives in tmux
and vim, to a team member who is not the strongest programmer on the team and
was told to "type dun in your project". They use it on a large monitor and on
a phone (Termux), often in tmux.

## Category and its best in class

A coding assistant that lives in the terminal. The bar is the best of:

- **for the job:** Claude Code, Codex CLI, Gemini CLI, aider — how fast a
  person gets from install to a useful answer about their project, and how
  little stands in the way.
- **for terminal design:** tmux, vim/neovim, lazygit, htop — terminal idioms
  (status lines, pane borders, reverse-video selection, key hints), dense but
  calm screens, one accent, nothing that imitates a desktop app.

## Constraints and aims

- **Weaker models.** dun is built to work with cheaper and weaker models,
  whose thinking and tool use cannot be trusted to be correct. How an answer
  was produced is part of the answer: thinking, tool calls, steps and sources
  stay visible in the turn, readable and open to inspection. They let the
  person check the work, and they add value of their own. Never hidden,
  collapsed away or dimmed below readable contrast.
- **Read while writing.** The person reads the conversation while typing in
  the input. Neither pane loses contrast to show focus; focus is the
  divider: the tmux half-line in the accent on the focused side, leaning
  toward the focused pane (a thin line on that edge, thicker at the far ends).
- **No bounce.** Nothing on screen jumps when something opens, closes,
  updates or moves focus. A shrink leaves blank space, at most to the bottom
  of the current view.
- **Phone to large screen.** It must work at about 48 columns and at about
  180.
