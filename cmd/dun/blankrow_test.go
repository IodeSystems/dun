package main

import (
	"strings"
	"testing"
)

// rowsAfter returns the screen rows that follow the last row containing marker.
func rowsAfter(screen, marker string) []string {
	rows := strings.Split(screen, "\n")
	for i := len(rows) - 1; i >= 0; i-- {
		if strings.Contains(rows[i], marker) {
			return rows[i+1:]
		}
	}
	return nil
}

// Reasoning models open their reply with "\n\n". Until real text follows, that
// reply draws nothing — and it used to take a row anyway, a blank one between
// the message just sent and what came next. A round that then called a tool
// finalized it, and the blank row stayed in the log for good.
func TestTUI_WhitespaceReplyTakesNoRow(t *testing.T) {
	v := newVtui(80, 24)
	v.event(map[string]any{"type": "ready", "tools": []any{"exec"}})
	long := "L100- " + strings.Repeat("abcdefghi ", 9) + "end"
	v.send(long)
	v.event(map[string]any{"type": "turn"})
	v.event(map[string]any{"type": "token", "text": "\n\n"})
	nm, _ := v.m.Update(renderTickMsg{})
	v.m = nm.(tuiModel)
	if n, c := len(v.m.frame.blockH), len(v.m.convo); n != c {
		t.Errorf("a whitespace-only reply took a block: %d blocks for %d entries", n, c)
	}

	v.event(map[string]any{"type": "tool_call", "tool": "exec", "args": map[string]any{"command": "echo 0"}})
	v.event(map[string]any{"type": "tool_result", "tool": "exec", "result": "0\n"})
	after := rowsAfter(v.view(), "end")
	if len(after) < 2 || !strings.HasPrefix(strings.TrimSpace(after[0]), "✓ $ echo 0") || strings.Contains(v.view(), "1 step") {
		t.Fatalf("the rows after the message are %q, want the one call, once\n%s", after[:2], v.view())
	}
	for _, e := range v.m.convo {
		if e.mdSource == "" && e.collapsed == "" && !e.blank && e.think == nil && e.run == nil && e.docs == nil {
			t.Errorf("an empty block was finalized: %+v", e)
		}
	}

	// Real text after the newlines is drawn, without the newlines.
	v.event(map[string]any{"type": "token", "text": "\n\nHere it is."})
	nm, _ = v.m.Update(renderTickMsg{})
	v.m = nm.(tuiModel)
	if after := rowsAfter(v.view(), "$ echo 0"); len(after) == 0 || !strings.Contains(after[0], "Here it is.") {
		t.Errorf("reply not directly under the call: %q", after)
	}
}
