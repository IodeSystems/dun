package tui

import "charm.land/lipgloss/v2"

// Styles for views outside this package (internal/psview, `dun ps
// --watch`), so they read as the TUI does. Functions, not copies:
// initMarkdownStyle changes stSecondary for a light terminal.

// AccentStyle is where you are: the selection gutter, the focused name.
func AccentStyle() lipgloss.Style { return stSel }

// SecondaryStyle is content one step below the main text.
func SecondaryStyle() lipgloss.Style { return stSecondary }

// NoteStyle is attention: a question waiting for an answer.
func NoteStyle() lipgloss.Style { return stNote }
