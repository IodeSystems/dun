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

// fitHints fits a row of hints to w cells, dropping whole hints from the end.
func fitHints(s string, w int) string {
	if w <= 0 || ansi.StringWidth(s) <= w {
		return s
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
// the extras, then back and cancel.
func formKeys(name, enter string, extra ...string) string {
	return name + "  ·  " + strings.Join(append(append([]string{enter}, extra...), "shift+tab back", "esc cancel"), " · ")
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
