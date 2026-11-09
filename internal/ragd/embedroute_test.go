package ragd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iodesystems/dun/proto"
)

// A search never waits behind ingest embedding: with every ingest embed
// request held open by the model, so every admission slot of the embed
// model is taken, a hybrid search still embeds its query and answers.
func TestSearchDoesNotWaitForIngestAdmission(t *testing.T) {
	var hold atomic.Bool
	var held atomic.Int32
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		query := len(req.Input) == 1 && strings.HasPrefix(req.Input[0], "search_query: ")
		if hold.Load() && !query {
			held.Add(1)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		var data []map[string]any
		for i, s := range req.Input {
			data = append(data, map[string]any{"index": i, "embedding": hashVec(s, 8)})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(ts.Close)
	h := openWith(t, t.TempDir(), ts.URL, "nomic-embed-text", "")
	t.Cleanup(func() { h.Close(time.Second) })
	var once sync.Once
	free := func() { once.Do(func() { close(release) }) }
	t.Cleanup(free) // before Close (cleanups run last first)

	ingest(t, h, proto.RagIngest{Project: "p", Index: "docs", Path: writeDoc(t, "a.md", "zeppelin sighting\n")})
	waitNoMissing(t, storeOf(t, h, "p/docs"))

	// Hold every ingest embed call; queue more jobs than runners.
	hold.Store(true)
	for i := range 4 {
		if _, err := h.Ingest(proto.RagIngest{Project: "p", Index: "docs",
			Path: writeDoc(t, fmt.Sprintf("b%d.md", i), fmt.Sprintf("harbor note %d\n", i))}); err != nil {
			t.Fatal(err)
		}
	}
	// Wait until the held count stops growing: every runner is in an embed
	// call, held by the model or waiting for a slot.
	deadline := time.Now().Add(20 * time.Second)
	for last, since := int32(-1), time.Now(); ; time.Sleep(20 * time.Millisecond) {
		if n := held.Load(); n != last {
			last, since = n, time.Now()
		} else if n > 0 && time.Since(since) > 300*time.Millisecond {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no ingest embed call reached the model")
		}
	}
	// The admission is full: an ingest-path embed call waits for a slot and
	// never reaches the model.
	before := held.Load()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	_, err := h.embed.EmbedDocs(ctx, []string{"probe"})
	cancel()
	if err == nil || held.Load() != before {
		t.Fatalf("ingest admission not full: probe err %v, held %d → %d", err, before, held.Load())
	}

	ctx, cancel = context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	got, err := h.Search(ctx, proto.RagSearch{Project: "p", Index: "docs", Query: "zeppelin"})
	took := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if got.Mode != "hybrid" || len(got.Hits) == 0 || took > time.Second {
		t.Fatalf("search with ingest admission full: mode %s, note %q, %d hits, %v; want hybrid with hits in under 1s",
			got.Mode, got.Note, len(got.Hits), took)
	}
	free()
}
