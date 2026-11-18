package ragd

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/iodesystems/dun/proto"
)

// The last-minute window counts what was added in the last nBuckets
// buckets: an add stays in the sum for a minute, and a bucket reused a
// minute later starts from zero.
func TestWindowRolls(t *testing.T) {
	var w window
	t0 := time.Unix(1_000_000_000, 0) // a bucket boundary
	w.add(t0, counts{files: 2, embeds: 1})
	w.add(t0.Add(3*time.Second), counts{files: 1})
	w.add(t0.Add(30*time.Second), counts{chunks: 5})
	if got := w.sum(t0.Add(30 * time.Second)); got.files != 3 || got.chunks != 5 || got.embeds != 1 {
		t.Fatalf("sum at 30s: %+v", got)
	}
	// 60s on, t0's bucket has rolled out; the 30s one is still in.
	if got := w.sum(t0.Add(60 * time.Second)); got.files != 0 || got.chunks != 5 {
		t.Fatalf("sum at 60s: %+v", got)
	}
	// The bucket of t0 is reused at t0+60s and starts from zero.
	w.add(t0.Add(61*time.Second), counts{files: 7})
	if got := w.sum(t0.Add(61 * time.Second)); got.files != 7 || got.chunks != 5 {
		t.Fatalf("sum at 61s: %+v", got)
	}
	if got := w.sum(t0.Add(10 * time.Minute)); got != (counts{}) {
		t.Fatalf("sum 10 minutes on: %+v", got)
	}
}

// An index's state: backed-off with work while the embed channel cools
// down, embedding with a request in flight, ingesting with jobs or a recent
// event, error while its last error is newer than its last success, idle.
func TestIndexState(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	for _, c := range []struct {
		name    string
		ix      proto.ProcRagIndex
		lastOK  time.Time
		chilled bool
		want    string
	}{
		{"never used", proto.ProcRagIndex{}, time.Time{}, false, StateIdle},
		{"queued", proto.ProcRagIndex{Queue: 3}, time.Time{}, false, StateIngesting},
		{"running", proto.ProcRagIndex{Running: 1}, time.Time{}, false, StateIngesting},
		{"embedding", proto.ProcRagIndex{Running: 1, EmbedInFlight: 1}, time.Time{}, false, StateEmbedding},
		{"backed off", proto.ProcRagIndex{Queue: 2}, time.Time{}, true, StateBackedOff},
		{"chilled, no work", proto.ProcRagIndex{}, time.Time{}, true, StateIdle},
		{"just active", proto.ProcRagIndex{LastActive: ago(time.Second)}, ago(time.Second), false, StateIngesting},
		{"active a while ago", proto.ProcRagIndex{LastActive: ago(time.Minute)}, ago(time.Minute), false, StateIdle},
		{"failed", proto.ProcRagIndex{LastErrAt: ago(time.Minute), LastActive: ago(time.Minute)}, ago(time.Hour), false, StateError},
		{"failed, then ok", proto.ProcRagIndex{LastErrAt: ago(time.Hour)}, ago(time.Minute), false, StateIdle},
		{"failed long ago", proto.ProcRagIndex{LastErrAt: ago(time.Hour)}, time.Time{}, false, StateIdle},
	} {
		if got := state(now, c.ix, c.lastOK, c.chilled); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
}

// The meter keeps the last error and its time; a success after it ends the
// error state, not the record.
func TestMeterLastError(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	m := newMeter("dun/code", func() time.Time { return now })
	m.embedded(40*time.Millisecond, 3, 120, nil)
	now = now.Add(time.Minute)
	m.embedded(time.Millisecond, 1, 10, errors.New("embed: 503\n  overloaded"))
	ix := m.snapshot(now, 0, 0, "", time.Time{}, false)
	if ix.State != StateError || ix.LastErr != "embed: 503 overloaded" || !ix.LastErrAt.Equal(now) {
		t.Fatalf("after a failure: %+v", ix)
	}
	if ix.Embeds != 1 || ix.Chunks != 3 || ix.Tokens != 30 || ix.EmbedMs != 40 {
		t.Fatalf("totals count the success only: %+v", ix)
	}
	if ix.Project != "dun" || ix.Kind != "code" || ix.Branch != "" {
		t.Fatalf("name split: %+v", ix)
	}
	now = now.Add(time.Minute)
	m.fetched(nil)
	m.ingested(nil)
	ix = m.snapshot(now.Add(30*time.Second), 0, 0, "", time.Time{}, false)
	if ix.State != StateIdle || ix.LastErr == "" || ix.FilesMin != 2 || ix.IdleSec != 30 {
		t.Fatalf("after a success: %+v", ix)
	}
	// A failed job newer than the meter's error is the last error.
	jobAt := now.Add(30 * time.Second)
	ix = m.snapshot(now.Add(time.Minute), 0, 0, "fetch: no such file", jobAt, false)
	if ix.LastErr != "fetch: no such file" || !ix.LastErrAt.Equal(jobAt) || ix.State != StateError {
		t.Fatalf("a failed job: %+v", ix)
	}
}

func TestSplitNameAndWhere(t *testing.T) {
	for name, want := range map[string][3]string{
		"dun/code":                      {"dun", "code", ""},
		"global/memories":               {"", "memories", ""},
		"dun/branches/wt-1a2b3c4d/docs": {"dun", "docs", "wt-1a2b3c4d"},
	} {
		p, k, b := splitName(name)
		if [3]string{p, k, b} != want {
			t.Errorf("splitName(%q) = %q %q %q", name, p, k, b)
		}
	}
	for u, want := range map[string]string{
		"http://127.0.0.1:8080/v1":  "local",
		"http://localhost:11434":    "local",
		"http://192.168.1.76:9000":  "local",
		"http://gpu.local:9000":     "local",
		"https://api.openai.com/v1": "remote",
		"https://8.8.8.8/v1":        "remote",
		"":                          "",
	} {
		if got := where(u); got != want {
			t.Errorf("where(%q) = %q, want %q", u, got, want)
		}
	}
}

// Status counts a job's file and its embed requests on its index, a text
// ingest on its own, and names the embed backend.
func TestStatusCountsIngest(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
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

	ingest(t, h, proto.RagIngest{Project: "p", Index: "docs", Path: writeDoc(t, "a.md", "zeppelin sighting over the harbor\n")})
	waitNoMissing(t, storeOf(t, h, "p/docs"))
	if _, err := h.Ingest(proto.RagIngest{Project: "p", Index: "sessions", Path: writeDoc(t, "s1", ""), Text: "a folded span"}); err != nil {
		t.Fatal(err)
	}
	st := h.Status()
	if st.Embed.Model != "nomic-embed-text" || st.Embed.Where != "local" || st.Embed.InFlight != 0 {
		t.Fatalf("embed backend: %+v", st.Embed)
	}
	by := map[string]proto.ProcRagIndex{}
	for _, ix := range st.Indexes {
		by[ix.Name] = ix
	}
	docs, ok := by["p/docs"]
	if !ok || docs.Files != 1 || docs.FilesMin != 1 || docs.Embeds < 1 || docs.EmbedsMin < 1 || docs.Chunks < 1 || docs.Tokens < 1 {
		t.Fatalf("p/docs: %+v (all: %+v)", docs, st.Indexes)
	}
	if docs.Queue != 0 || docs.Running != 0 || docs.IdleSec < 0 {
		t.Fatalf("p/docs after its job: %+v", docs)
	}
	ses, ok := by["p/sessions"]
	if !ok || ses.Files != 1 || ses.Kind != "sessions" || ses.Project != "p" {
		t.Fatalf("p/sessions: %+v", ses)
	}
	if ses.Embeds < 1 {
		t.Fatalf("the text ingest's embed is not on p/sessions: %+v", ses)
	}
}
