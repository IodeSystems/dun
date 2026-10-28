package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iodesystems/dun"
	"github.com/iodesystems/dun/proto"
)

// The replies to what the person typed or toggled are kept, spelled as they
// would type them; views and the engine's own server events are not.
func TestRecordReplies_SpellsCommandsAsTyped(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	sf := filepath.Join(t.TempDir(), "s.jsonl")
	h, err := dun.Start(context.Background(), dun.Config{Workspace: t.TempDir(), SessionFile: sf, NoTools: true})
	if err != nil {
		t.Fatal(err)
	}
	rec := recordReplies(h)
	rec(&proto.ControlEvent{ID: "files", Action: "add ~/notes --global", Message: "files: open"})
	rec(&proto.ControlEvent{ID: "switches", Action: "indexes on p:x --session", Message: "p:x on for this session"})
	rec(&proto.ControlEvent{ID: "slash", Action: "rag status", Message: "raglit: running"})
	rec(&proto.ControlEvent{ID: "ext", Action: "view", Message: "the list"})
	rec(&proto.ControlEvent{ID: "unqueue", Action: "", Message: "[]"})
	rec(&proto.ServerEvent{ID: dun.ServerDocs, Action: "start", Message: "raglit up"})
	rec(&proto.ServerEvent{ID: dun.ServerDocs, Action: "on", Message: "raglit: on for this session"})
	h.Close()
	var lines []string
	for _, it := range dun.SessionHistory(sf) {
		if it.Kind == "command" {
			l, _, _ := strings.Cut(it.Content, "\n")
			lines = append(lines, l)
		}
	}
	want := []string{"/file add ~/notes --global", "/rag on p:x --session", "/rag status", "/rag on"}
	if strings.Join(lines, " | ") != strings.Join(want, " | ") {
		t.Fatalf("kept:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
}

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
