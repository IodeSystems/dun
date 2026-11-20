package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// rowsOf strips styling and the band's right padding.
func rowsOf(s string) []string {
	rows := strings.Split(ansi.Strip(s), "\n")
	for i, r := range rows {
		rows[i] = strings.TrimRight(r, " ")
	}
	return rows
}

// composerRows is what the composer draws for text at a terminal width.
func composerRows(text string, termW int) []string {
	in := newMultilineInput()
	in.maxLines = 1000
	in.width = termW - 2 // tui_input.go: the composer's width on a resize
	in.SetValue(text)
	return rowsOf(in.View())
}

// historyRows is the sent message in the conversation pane at the same
// terminal width: vp.Width = termW, wrapW = vp.Width-1 (tui_scroll.go).
func historyRows(text string, termW int) []string {
	return rowsOf(userRowAt(text, termW-1))
}

func TestTypedRow_KeepsLineBreaks(t *testing.T) {
	text := "one\ntwo\n\nfour  indented\nfive"
	got := historyRows(text, 60)
	want := []string{"› one", "  two", "", "  four  indented", "  five"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("rows = %q, want %q", got, want)
	}
}

// The wrap columns equal the composer's, past the reading cap, at several
// widths: the message looks as it did when typed.
func TestTypedRow_WrapsAtComposerWidth(t *testing.T) {
	long := strings.Repeat("alpha beta gamma delta ", 30)
	text := long + "\nshort line\n" + strings.Repeat("x", 300)
	for _, w := range []int{40, 80, 120, 180} {
		got, want := historyRows(text, w), composerRows(text, w)
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("termW %d: history rows differ from composer rows\nhistory:\n%s\ncomposer:\n%s",
				w, strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	}
	// 180 columns: a row runs past bandCap (100).
	widest := 0
	for _, r := range historyRows(text, 180) {
		widest = max(widest, ansi.StringWidth(r))
	}
	if widest <= bandCap {
		t.Errorf("widest row at 180 columns = %d, want past the reading cap %d", widest, bandCap)
	}
}

// A resize re-wraps to the new composer width.
func TestTypedRow_Resize(t *testing.T) {
	text := strings.Repeat("alpha beta gamma delta ", 12)
	narrow, wide := historyRows(text, 50), historyRows(text, 150)
	if len(narrow) <= len(wide) {
		t.Errorf("narrow %d rows, wide %d rows: the wide render should be shorter", len(narrow), len(wide))
	}
	for _, w := range []int{50, 150, 70} {
		if got, want := historyRows(text, w), composerRows(text, w); strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("termW %d: history differs from composer", w)
		}
	}
}
