package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// A frame taller than the screen keeps its bottom — the input stays — and the
// status row names the part that grew.
func TestFitFrame_KeepsTheInputAndNamesTheCulprit(t *testing.T) {
	hook := onOverflow
	var got []string
	onOverflow = func(r string) { got = append(got, r) }
	defer func() { onOverflow = hook }()

	parts := []framePart{
		{"head", "dun"},
		{"activity", strings.Repeat("task line\n", 19) + "task line"},
		{"conversation", "c1\nc2"},
		{"divider", "──"},
		{"input", "> my input"},
		{"status", "ready"},
	}
	out := fitFrame(parts, 10, 80)
	rows := strings.Split(ansi.Strip(out), "\n")
	if len(rows) != 10 {
		t.Fatalf("%d rows on a 10-row screen:\n%s", len(rows), ansi.Strip(out))
	}
	if rows[8] != "> my input" || rows[7] != "──" {
		t.Errorf("the input is not on screen:\n%s", ansi.Strip(out))
	}
	want := "⚠ screen overflow: 26 rows on a 10-row screen — activity is 20 rows"
	if rows[9] != want {
		t.Errorf("status row %q, want %q", rows[9], want)
	}
	if len(got) != 1 || "⚠ "+got[0] != want {
		t.Errorf("hook told %q", got)
	}

	got = nil
	if out := fitFrame(parts[:1], 10, 80); out != "dun" || len(got) != 0 {
		t.Errorf("a frame that fits is untouched: %q, %q", out, got)
	}
	if out := fitFrame(parts, 0, 80); len(strings.Split(out, "\n")) != 26 || len(got) != 0 {
		t.Error("no screen size yet: nothing to check against")
	}
}
