package tui

import (
	"strings"
	"testing"
)

// The grace starting is one dim line in the TUI, not a bell.
func TestTUI_BudgetSpentLine(t *testing.T) {
	m := newTUIModel(&dunProc{}, "/ws")
	m = m.handleEvent(evMsg{"type": "notification", "kind": "budget",
		"text": "turn budget 30m spent — the model is told to wrap up; cut in 6m"})
	var all strings.Builder
	for _, e := range m.convo {
		all.WriteString(e.view())
	}
	if got := all.String(); !strings.Contains(got, "⏱") || !strings.Contains(got, "cut in 6m") || strings.Contains(got, "🔔") {
		t.Fatalf("budget line: %q", got)
	}
}
