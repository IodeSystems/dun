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
