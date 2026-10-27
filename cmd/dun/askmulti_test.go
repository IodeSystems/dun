package main

import (
	"strings"
	"testing"
)

// Multi-select (USER 2026-10-27): typing on the custom row adds an option,
// checked, and keeps what was checked; tab edits an option's own text; done
// sends them all.
func TestAsk_MultiCustomAddsAndEditsOptions(t *testing.T) {
	m := newTUIModel(&dunProc{stdin: &echoWC{}}, "/ws")
	m = resized(m, 80, 24)
	m = m.handleEvent(evMsg{"type": "ask", "question": "Fix which?", "options": []any{"lint", "tests", "docs"}, "multi": true})
	m = key(m, kEnter) // check lint
	for range 3 {
		m = key(m, kDown) // to the custom row
	}
	m = typeStr(m, "bench")
	m = key(m, kEnter)
	if !m.asking || len(m.askOptions) != 4 || m.askOptions[3] != "bench" || !m.askChecked[0] || !m.askChecked[3] || m.askChecked[1] {
		t.Fatalf("asking=%v options=%v checked=%v", m.asking, m.askOptions, m.askChecked)
	}
	m.askSel = 1
	m = key(m, kTab) // edit "tests"
	if m.input.Value() != "tests" {
		t.Fatalf("edit field: %q", m.input.Value())
	}
	m = typeStr(m, " (unit)")
	m = key(m, kEnter)
	if m.askOptions[1] != "tests (unit)" || !m.askChecked[1] {
		t.Fatalf("options=%v checked=%v", m.askOptions, m.askChecked)
	}
	for range 4 {
		m = key(m, kDown) // to done
	}
	m = key(m, kEnter)
	if m.asking || !strings.Contains(convoText(m), "lint, tests (unit), bench") {
		t.Errorf("asking=%v convo:\n%s", m.asking, convoText(m))
	}
}
