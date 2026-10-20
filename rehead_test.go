package dun

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/agent"
)

func readPair(id, tool, args, result string) []agent.Entry {
	return []agent.Entry{
		{ID: id + "c", Kind: agent.KindToolCall, ToolName: tool, ToolCallID: id, Content: args},
		{ID: id + "r", Kind: agent.KindToolResult, ToolName: tool, ToolCallID: id, Content: result},
	}
}

// The older copy of an exact repeat is superseded; the newest is not; a read
// of something else, a small result, or a command that is not a plain read is
// never touched.
func TestSupersededReads(t *testing.T) {
	big := strings.Repeat("x", reheadMinChars)
	var es []agent.Entry
	es = append(es, readPair("1", "node_read", `{"node":"#dun/tui.go#View"}`, big)...)
	es = append(es, readPair("2", "node_read", `{"node":"#dun/tui.go#Update"}`, big)...)
	es = append(es, readPair("3", "exec", `{"command":"cat go.mod"}`, big)...)
	es = append(es, readPair("4", "exec", `{"command":"cat go.mod | head"}`, big)...)
	es = append(es, readPair("5", "node_read", `{"node":"#dun/tui.go#View"}`, big)...)
	es = append(es, readPair("6", "exec", `{"command":"cat go.mod"}`, "small")...)
	es = append(es, readPair("7", "exec", `{"command":"cat go.mod | head"}`, big)...)

	got := supersededReads(es)
	want := map[string]string{"1r": "node_read #dun/tui.go#View", "3r": "exec cat go.mod"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for id, k := range want {
		if got[id] != k {
			t.Errorf("%s: got %q want %q", id, got[id], k)
		}
	}
}

// End to end: the older copy becomes a stub, the original is in a sidecar,
// pairing is intact, and a second pass finds nothing more to do.
func TestRehead_StubsAndArchives(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	store, err := openSessionStore(path)
	if err != nil {
		t.Fatal(err)
	}
	h := &Harness{store: store, cache: &cacheStats{}, cfg: Config{SessionFile: path}}
	ctx := context.Background()
	old := strings.Repeat("old ", reheadMinChars)
	for _, e := range append(readPair("1", "node_read", `{"node":"#a.go#F"}`, old),
		readPair("2", "node_read", `{"node":"#a.go#F"}`, strings.Repeat("new ", reheadMinChars))...) {
		store.Append(ctx, "s", e)
	}

	if !h.rehead(ctx, "s") {
		t.Fatal("a superseded read should change the context")
	}
	es, _ := store.Context(ctx, "s")
	if len(es) != 4 || es[1].Kind != agent.KindToolResult || es[1].ToolCallID != "1" {
		t.Fatalf("shape or pairing changed: %+v", es)
	}
	if !strings.HasPrefix(es[1].Content, reheadStubPrefix) || !strings.Contains(es[1].Content, "s.rehead1.jsonl") {
		t.Errorf("older copy should be a stub naming its sidecar: %q", es[1].Content)
	}
	if !strings.HasPrefix(es[3].Content, "new ") {
		t.Error("the newest copy must stay whole")
	}
	side, err := os.ReadFile(filepath.Join(dir, "s.rehead1.jsonl"))
	if err != nil || !strings.Contains(string(side), "old old") {
		t.Errorf("original must be archived: %v", err)
	}
	if h.cache.ReheadEntries != 1 || h.cache.ReheadChars <= 0 {
		t.Errorf("stats: %+v", h.cache)
	}
	if h.rehead(ctx, "s") {
		t.Error("a second pass has nothing to do")
	}
}
