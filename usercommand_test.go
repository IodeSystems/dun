package dun

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// A command the person ran is kept in the session and replayed to them, and
// never reaches the model (USER 2026-10-28).
func TestRecordCommand_KeptForThePersonNotTheModel(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	sf := filepath.Join(t.TempDir(), "s.jsonl")
	h, err := Start(context.Background(), Config{Workspace: t.TempDir(), SessionFile: sf, Client: &scriptRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	h.RecordCommand("/file add ~/notes", "files: /home/me/notes — directory open, read only")
	ctx, _ := h.store.Context(context.Background(), "dun")
	for _, e := range ctx {
		if strings.Contains(e.Content, "/file add") {
			t.Fatalf("the model's context has the command: %+v", e)
		}
	}
	h.Close()
	items := SessionHistory(sf)
	if len(items) != 1 || items[0].Kind != "command" || !strings.HasPrefix(items[0].Content, "/file add ~/notes\nfiles: ") {
		t.Fatalf("history: %+v", items)
	}
}
