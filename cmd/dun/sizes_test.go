package main

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// The walk at 48×24 and 180×50 (plan item 38).

// On a phone a tool call's line is cut at a word with an ellipsis, as every
// other row is; it was cut mid-word at the edge ("→ Notes: keep the greet").
func TestPhone_CallLineEndsAtAWord(t *testing.T) {
	v := newVtui(48, 24)
	v.event(map[string]any{"type": "ready", "tools": []any{"notes__project_notes"}})
	v.send("What do the team notes say?")
	v.event(map[string]any{"type": "tool_call", "tool": "notes__project_notes", "args": map[string]any{}})
	v.event(map[string]any{"type": "tool_result", "tool": "notes__project_notes", "result": "Notes: keep the greeting short. Release on Fridays."})
	var row string
	for _, l := range shown(v) {
		if strings.Contains(l, "project notes") {
			row = l
		}
	}
	if row == "" {
		t.Fatalf("no call row:\n%s", strings.Join(shown(v), "\n"))
	}
	if ansi.StringWidth(row) > 48 || !strings.HasSuffix(row, "…") || strings.HasSuffix(row, "greet…") {
		t.Errorf("call row %q (%d cols): want it cut at a word with …", row, ansi.StringWidth(row))
	}
}

// On a wide screen an answer wraps at the question band's edge (bandCap),
// not 170 columns out.
func TestWide_AnswerWrapsAtTheBandEdge(t *testing.T) {
	v := newVtui(180, 50)
	v.event(map[string]any{"type": "ready", "tools": []any{"eval"}})
	v.send("Can you help me with this project?")
	v.event(map[string]any{"type": "token", "text": strings.Repeat("This project is a small command-line tool. ", 8)})
	v.event(map[string]any{"type": "done"})
	wrapped := 0
	for _, l := range shown(v) {
		if strings.Contains(l, "small command-line") {
			wrapped++
			if w := ansi.StringWidth(l); w > bandCap {
				t.Errorf("answer row %d cols, want ≤ %d: %q", w, bandCap, l)
			}
		}
	}
	if wrapped < 2 {
		t.Errorf("the answer did not wrap:\n%s", strings.Join(shown(v), "\n"))
	}
}
