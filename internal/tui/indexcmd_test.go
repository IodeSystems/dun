package tui

import (
	"strings"
	"testing"

	"github.com/iodesystems/dun"
)

// A running poly-lsp whose index builds says indexing on the splash and the
// status line; when the build ends the words go, and an index report is
// said in the conversation even beside the live splash.
func TestIndexing_SplashStatusAndReport(t *testing.T) {
	v := newVtui(100, 21)
	v.event(map[string]any{"type": "session", "id": "s1", "resumed": float64(0)})
	servers := func(index string) []any {
		return []any{
			map[string]any{"id": "shell", "running": true, "tools": float64(1)},
			map[string]any{"id": "poly-lsp", "slash": "poly-lsp", "auto": true, "running": true,
				"index": index, "indexSeconds": float64(12), "tools": float64(4)},
		}
	}
	v.event(map[string]any{"type": "ready", "tools": []any{"eval"}, "servers": servers(dun.IndexBuilding)})
	v.event(map[string]any{"type": "settings", "model": "m", "url": "http://llm/v1"})
	if got := stripANSI(convoText(v.m)); !strings.Contains(got, "poly-lsp indexing 12s") {
		t.Fatalf("the splash does not say indexing:\n%s", got)
	}
	if got := lastRow(v.m); !strings.Contains(got, "ready · poly-lsp indexing 12s") {
		t.Fatalf("status: %q", got)
	}
	n := len(v.m.convo)
	msg := "poly-lsp: the index holds .claude/worktrees/ (21,557 files, 97% of the index, 48 other git checkouts). To leave it out: /lsp exclude .claude/worktrees"
	v.event(map[string]any{"type": "server", "id": "poly-lsp", "action": "index", "background": true,
		"message": msg, "servers": servers(dun.IndexReady), "tools": []any{"eval", "node_query"}})
	if len(v.m.convo) <= n || !strings.Contains(stripANSI(convoText(v.m)), "the index holds .claude/worktrees/") {
		t.Fatalf("the report is not in the conversation:\n%s", convoText(v.m))
	}
	if got := lastRow(v.m); strings.Contains(got, "indexing") {
		t.Fatalf("still indexing after ready: %q", got)
	}
	// The splash's tools row names a server only while it is changing or wrong.
	if got := stripANSI(convoText(v.m)); strings.Contains(got, "poly-lsp indexing") {
		t.Fatalf("the splash still says indexing:\n%s", got)
	}
}
