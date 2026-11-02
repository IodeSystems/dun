package tui

import (
	"strings"
	"testing"
)

// On resume the command shows, dim, with what dun answered under it.
func TestReplay_ShowsTheCommandAndItsAnswer(t *testing.T) {
	v := newVtui(100, 30)
	v.event(map[string]any{"type": "history", "items": []any{
		map[string]any{"kind": "command", "content": "/file add ~/notes\nfiles: /home/me/notes — directory open, read only"},
	}})
	if c := v.convo(); !strings.Contains(c, "› /file add ~/notes") || !strings.Contains(c, "directory open, read only") {
		t.Fatalf("replay:\n%s", c)
	}
}
