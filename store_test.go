package dun

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/agent"
)

// A session written to disk reloads with the same entries — the resume contract.
func TestSessionStore_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	ctx := context.Background()

	s, err := openSessionStore(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Append(ctx, "x", agent.Entry{ID: "1", Kind: agent.KindUser, Content: "hello"})
	s.Append(ctx, "x", agent.Entry{ID: "2", Kind: agent.KindAssistant, Content: "hi there"})

	re, err := openSessionStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if re.Loaded() != 2 {
		t.Fatalf("want 2 entries reloaded, got %d", re.Loaded())
	}
	got, _ := re.Context(ctx, "x")
	if got[0].Content != "hello" || got[1].Content != "hi there" {
		t.Fatalf("content lost on reload: %+v", got)
	}
}

// A large content is extracted to a blob (not inlined) and re-materialized on
// load — the "file refs extracted" contract.
func TestSessionStore_BlobExtraction(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	ctx := context.Background()
	big := strings.Repeat("x", blobThreshold+100)

	s, _ := openSessionStore(path)
	s.Append(ctx, "x", agent.Entry{ID: "1", Kind: agent.KindToolResult, Content: big})

	// The JSONL must NOT inline the big content (it's a ref); the blob holds it.
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), big) {
		t.Fatal("large content was inlined, not extracted to a blob")
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, "blobs")); len(entries) != 1 {
		t.Fatalf("want 1 blob, got %d", len(entries))
	}

	// Reload re-materializes the full content.
	re, _ := openSessionStore(path)
	got, _ := re.Context(ctx, "x")
	if len(got) != 1 || got[0].Content != big {
		t.Fatal("blob not re-materialized to full content on load")
	}
}

// Compaction drops subsumed entries and persists the marker.
func TestSessionStore_CompactPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	ctx := context.Background()
	s, _ := openSessionStore(path)
	s.Append(ctx, "x", agent.Entry{ID: "1", Kind: agent.KindUser, Content: "a"})
	s.Append(ctx, "x", agent.Entry{ID: "2", Kind: agent.KindAssistant, Content: "b"})
	s.Compact(ctx, "x", agent.Compaction{
		Subsumes: []agent.Entry{{ID: "1"}, {ID: "2"}},
		Marker:   agent.Entry{ID: "m", Kind: agent.KindUser, Content: "summary"},
	})

	re, _ := openSessionStore(path)
	got, _ := re.Context(ctx, "x")
	if len(got) != 1 || got[0].ID != "m" {
		t.Fatalf("compaction not persisted: %+v", got)
	}
}

// History maps loaded store entries to replayable items in chronological order:
// user/assistant/notification pass through, a tool_call decodes its JSON args,
// compaction markers and empty assistant turns are dropped.
func TestHarness_History(t *testing.T) {
	ctx := context.Background()
	s, err := openSessionStore("")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []agent.Entry{
		{ID: "1", Kind: agent.KindUser, Content: "do it", CreatedAt: 1},
		{ID: "2", Kind: agent.KindAssistant, Content: "", CreatedAt: 2},          // empty → dropped
		{ID: "3", Kind: agent.KindAssistant, Content: "working", CreatedAt: 3},
		{ID: "4", Kind: agent.KindToolCall, ToolName: "node_read", ToolCallID: "c1", Content: `{"sel":"F"}`, CreatedAt: 4},
		{ID: "5", Kind: agent.KindToolResult, ToolName: "node_read", ToolCallID: "c1", Content: "func F(){}", CreatedAt: 5},
		{ID: "6", Kind: agent.KindCompaction, Content: "summary", CreatedAt: 6},  // marker → dropped
		{ID: "7", Kind: agent.KindNotification, Content: "job done", CreatedAt: 7},
	} {
		if err := s.Append(ctx, "dun", e); err != nil {
			t.Fatal(err)
		}
	}
	h := &Harness{store: s}
	items := h.History()

	var kinds []string
	for _, it := range items {
		kinds = append(kinds, it.Kind)
	}
	want := "user assistant tool_call tool_result notification"
	if got := strings.Join(kinds, " "); got != want {
		t.Fatalf("history kinds = %q, want %q", got, want)
	}
	if items[2].Args["sel"] != "F" {
		t.Fatalf("tool_call args not decoded: %v", items[2].Args)
	}
	if items[2].CallID != "c1" || items[3].CallID != "c1" {
		t.Fatal("tool_call and result should share the call id for folding")
	}
}
