package tui

import (
	"strings"
	"testing"

	"charm.land/glamour/v2"
)

// A reply that leaks the private step headings gets no H1 band for them.
func TestRenderMarkdown_LeakedStepHeadsAreNotHeadings(t *testing.T) {
	src := "# Thinking: Found a live port\nthe default is `9000`\n\n# Summary: `bin/report` uses it\n\n```\n# Thinking: in code\n```\n\n# Real heading"
	got := demoteStepHeads(src)
	for _, want := range []string{"*Thinking:* Found a live port", "*Summary:* `bin/report` uses it", "# Thinking: in code", "# Real heading"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	r, err := glamour.NewTermRenderer(glamour.WithStandardStyle("dark"), glamour.WithWordWrap(60))
	if err != nil {
		t.Fatal(err)
	}
	out := renderMarkdown(r, src)
	bands := 0 // rows with glamour's dark H1 band: only the real heading's
	for _, row := range strings.Split(out, "\n") {
		if strings.Contains(row, "48;5;63") {
			bands++
		}
	}
	if bands != 1 {
		t.Errorf("rows with an H1 band: %d, want 1\n%s", bands, stripANSI(out))
	}
}
