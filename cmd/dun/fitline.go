package main

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// One rule for a row wider than the window, on every surface below the
// conversation (forms, pickers, the palette, the status line): nothing is
// cut mid-word. A row of key hints ("a · b · c") loses whole hints from its
// end — so the important ones go first — and any other row ends at a word,
// with an ellipsis. The terminal used to cut both at the edge, mid-letter.

// hintSep separates key hints on a row.
const hintSep = " · "

// fitHints fits a row of hints to w cells, dropping whole hints from the end
// — except the way out (esc …, ctrl+c quit), which is never dropped while a
// hint before it can go instead.
func fitHints(s string, w int) string {
	if w <= 0 || ansi.StringWidth(s) <= w {
		return s
	}
	if kept, ok := fitKeepingWayOut(s, w); ok {
		return kept
	}
	plain := ansi.Strip(s)
	cut := -1
	for i := 0; ; {
		j := strings.Index(plain[i:], hintSep)
		if j < 0 {
			break
		}
		at := i + j
		if ansi.StringWidth(plain[:at]) > w {
			break
		}
		cut, i = at, at+len(hintSep)
	}
	if cut < 0 {
		return fitText(s, w)
	}
	return ansi.Truncate(s, ansi.StringWidth(plain[:cut]), "")
}

// wayOut is a hint that leaves: a narrow row keeps it.
func wayOut(h string) bool {
	h = strings.TrimSpace(ansi.Strip(h))
	return strings.HasPrefix(h, "esc ") || strings.HasPrefix(h, "ctrl+c ") || strings.HasPrefix(h, "/exit ")
}

// fitKeepingWayOut drops hints from the end, skipping the way out, until
// the row fits. The row is re-styled dim as one (every hint row is dim);
// a row with a leading part in another style (a spinner, a warning) is not
// this function's: ok false.
func fitKeepingWayOut(s string, w int) (string, bool) {
	plain := ansi.Strip(s)
	if stDim.Render(plain) != s {
		return "", false
	}
	head, rest, found := strings.Cut(plain, "  ·  ")
	if !found {
		head, rest = "", plain
	}
	hints := strings.Split(rest, hintSep)
	keep := -1
	for i, h := range hints {
		if wayOut(h) {
			keep = i
		}
	}
	if keep < 0 {
		return "", false
	}
	join := func(hs []string) string {
		if head == "" && !found {
			return strings.Join(hs, hintSep)
		}
		return head + "  ·  " + strings.Join(hs, hintSep)
	}
	for len(hints) > 1 && ansi.StringWidth(join(hints)) > w {
		drop := len(hints) - 1
		for drop >= 0 && wayOut(hints[drop]) {
			drop--
		}
		if drop <= 0 {
			break // only the first hint and the way out are left
		}
		hints = append(hints[:drop:drop], hints[drop+1:]...)
	}
	if ansi.StringWidth(join(hints)) > w {
		return "", false
	}
	return stDim.Render(join(hints)), true
}

// fitClause fits a sentence to w cells by ending it at a clause — before
// ": ", "; ", " — ", " (" or ", " — so the part kept still means
// something; with no clause short enough, at a word (fitText).
func fitClause(s string, w int) string {
	if w <= 0 || ansi.StringWidth(s) <= w {
		return s
	}
	// The strong breaks first (a colon, a semicolon, a dash, a bracket): a
	// comma may end a clause or only a list item ("run: on, in a …").
	for _, tier := range [][]string{{": ", "; ", " — ", " ("}, {", "}} {
		best := -1
		for _, sep := range tier {
			for i := 0; ; {
				j := strings.Index(s[i:], sep)
				if j < 0 {
					break
				}
				at := i + j
				if ansi.StringWidth(s[:at]) <= w && at > best {
					best = at
				}
				i = at + len(sep)
			}
		}
		if best > 0 {
			return s[:best]
		}
	}
	return fitText(s, w)
}

// fitText fits s to w cells, ending at a word with an ellipsis.
func fitText(s string, w int) string {
	if w <= 0 || ansi.StringWidth(s) <= w {
		return s
	}
	plain := ansi.Strip(s)
	cut := ansi.Truncate(plain, w-1, "") // room for the ellipsis
	if sp := strings.LastIndexAny(cut, " "); sp > len(cut)/2 {
		cut = strings.TrimRight(cut[:sp], " ·,;:—")
	}
	return ansi.Truncate(s, ansi.StringWidth(cut), "") + "…"
}

// fitRows applies fitText to each row of a panel. A panel's key hints are on
// the status line (fitHints), not in the panel.
func fitRows(block string, w int) string {
	rows := strings.Split(block, "\n")
	for i, r := range rows {
		rows[i] = fitText(r, w)
	}
	return strings.Join(rows, "\n")
}

// listRow is one row of a list a person picks from — the palette, the model
// picker: the selected row has the gutter bar and its name in the accent,
// bold. pre goes before the name, post after it.
// listIndent is where a list's rows start (after listRow's gutter): a list's
// search line and its count line up with them.
const listIndent = "  "

func listRow(pre, name, post string, selected bool) string {
	if selected {
		return addGutter(pre+stSel.Render(name)+post, "▎ ", stSel)
	}
	return addGutter(pre+name+post, "  ", lipgloss.NewStyle())
}

// formLabelW is the label column of every form (the login form, /mcp add):
// one width, so their fields start in the same column.
const formLabelW = 7

// formFieldW is a form field's width in a terminal w columns wide: the row
// less the gutter and the label column, at most 56. A wider field was cut
// at the edge, and its placeholder with it.
func formFieldW(w int) int {
	if w <= 0 {
		return 56
	}
	return max(10, min(56, w-(2+formLabelW+2)-3))
}

// formNotes ends a form (the login form, /mcp add) the one way: after its
// fields and any error, a blank row, then its notes, dim, one per row.
func formNotes(rows []string, notes ...string) []string {
	rows = append(rows, "")
	for _, n := range notes {
		rows = append(rows, "  "+stDim.Render(n))
	}
	return rows
}

// formKeys is a form's status line, the one way: its name, enter's word,
// cancel, back, then the extras — a narrow row loses the extras first.
func formKeys(name, enter string, extra ...string) string {
	return name + "  ·  " + strings.Join(append([]string{enter, "esc cancel", "shift+tab back"}, extra...), " · ")
}

// formRow is one field of a form: its label, marked when it has the cursor,
// then the field.
func formRow(label string, focused bool, field string) string {
	l := fmt.Sprintf("%-*s  ", formLabelW, label)
	if focused {
		return stSel.Render("▎ "+l) + field
	}
	return stDim.Render("  "+l) + field
}
