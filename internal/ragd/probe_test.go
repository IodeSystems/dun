package ragd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iodesystems/agentkit/llm"
	"github.com/iodesystems/raglit"

	"github.com/iodesystems/dun"
	"github.com/iodesystems/dun/internal/dunconfig"
	"github.com/iodesystems/dun/proto"
)

// fakeModels is an OpenAI-compatible endpoint: /v1/embeddings gives each
// input a vector from a hash of the model and the text (so the same model
// gives the same vector, and two models two spaces), and
// /v1/chat/completions answers with chat, streamed when asked.
type fakeModels struct {
	dim      int
	maxBatch int  // 0: any
	noisy    bool // embeddings differ per request
	chat     func(req map[string]any) string

	mu      sync.Mutex
	calls   int
	batches []int
	bodies  []string
}

func (f *fakeModels) serve(t *testing.T) string {
	ts := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(ts.Close)
	return ts.URL
}

func (f *fakeModels) handler(w http.ResponseWriter, r *http.Request) {
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r.Body)
	var req map[string]any
	_ = json.Unmarshal(buf.Bytes(), &req)
	f.mu.Lock()
	f.calls++
	call := f.calls
	f.bodies = append(f.bodies, buf.String())
	f.mu.Unlock()
	switch {
	case strings.HasSuffix(r.URL.Path, "/embeddings"):
		model, _ := req["model"].(string)
		in, _ := req["input"].([]any)
		f.mu.Lock()
		f.batches = append(f.batches, len(in))
		f.mu.Unlock()
		if f.maxBatch > 0 && len(in) > f.maxBatch {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, `{"error":{"message":"batch of %d is over %d"}}`, len(in), f.maxBatch)
			return
		}
		var data []map[string]any
		for i, x := range in {
			s, _ := x.(string)
			seed := model + "\n" + s
			if f.noisy {
				seed += fmt.Sprint(call)
			}
			data = append(data, map[string]any{"index": i, "embedding": hashVec(seed, f.dim)})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	case strings.HasSuffix(r.URL.Path, "/chat/completions"):
		text := "ok"
		if f.chat != nil {
			text = f.chat(req)
		}
		if st, _ := req["stream"].(bool); st {
			w.Header().Set("Content-Type", "text/event-stream")
			b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": text}}}})
			fmt.Fprintf(w, "data: %s\n\n", b)
			e, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}})
			fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", e)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": text}}}})
	default:
		http.NotFound(w, r)
	}
}

func hashVec(seed string, dim int) []float32 {
	v := make([]float32, dim)
	for i := range v {
		h := fnv.New32a()
		fmt.Fprintf(h, "%d\n%s", i, seed)
		v[i] = float32(h.Sum32()%2000)/1000 - 1
	}
	return v
}

// cfgWith is a config with one saved provider at url and the given facets.
func cfgWith(url string, facets map[string]dunconfig.Facet) dunconfig.DunConfig {
	return dunconfig.DunConfig{
		Provider:  "fake",
		Providers: []dunconfig.ProviderConfig{{Name: "fake", URL: url}},
		Facets:    facets,
	}
}

func probeOne(t *testing.T, cfg dunconfig.DunConfig, capsDir, facet string) ProbeResult {
	t.Helper()
	res, err := Probe(context.Background(), cfg, capsDir, facet)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 {
		t.Fatalf("%d results for one facet", len(res))
	}
	return res[0]
}

func fact(t *testing.T, caps llm.Capabilities, key string) llm.Fact {
	t.Helper()
	f, ok := caps.Facts[key]
	if !ok {
		t.Fatalf("no %s in the record: %s", key, dun.CapsReport(caps))
	}
	if f.Source != llm.SourceProbed {
		t.Fatalf("%s has source %s, want probed", key, f.Source)
	}
	return f
}

// The embed probe records dimension, the largest batch that worked, latency
// per batch and determinism, in the store /model probe writes: the stored
// record is what the result says.
func TestProbeEmbedRecordsItsFactsInTheCapabilityStore(t *testing.T) {
	f := &fakeModels{dim: 12, maxBatch: 8}
	url := f.serve(t)
	caps := t.TempDir()
	r := probeOne(t, cfgWith(url, map[string]dunconfig.Facet{"embed": {Model: "emb"}}), caps, "embed")
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	if v := fact(t, r.Caps, CapEmbedDimension).Value; v != "12" {
		t.Errorf("dimension %s, want 12", v)
	}
	bm := fact(t, r.Caps, CapEmbedBatchMax)
	if bm.Value != "8" || !strings.Contains(bm.Evidence, "32 failed") {
		t.Errorf("batch_max %q (%s), want 8 with 32 failed", bm.Value, bm.Evidence)
	}
	if v := fact(t, r.Caps, CapEmbedLatency).Value; !strings.HasPrefix(v, "1:") || !strings.Contains(v, " 8:") || strings.Contains(v, "32:") {
		t.Errorf("latency %q, want batches 1 and 8 only", v)
	}
	if v := fact(t, r.Caps, CapEmbedDeterministic).Value; v != "true" {
		t.Errorf("deterministic %s, want true", v)
	}
	if got := fmt.Sprint(f.batches); got != "[1 8 32 1]" {
		t.Errorf("batches sent %s, want [1 8 32 1]: stop at the first failure, then the same input again", got)
	}
	stored := dun.LoadCapsIn(caps, r.Caps.Endpoint, "emb")
	if stored.Facts[CapEmbedDimension].Value != "12" || stored.Facts[CapEmbedBatchMax].Value != "8" {
		t.Errorf("stored record %s", dun.CapsReport(stored))
	}
	if embedDimension(caps, raglit.ModelRole{BaseURL: url, Model: "emb"}) != 12 {
		t.Error("dund does not read the probed dimension back")
	}
}

func TestProbeEmbedSaysWhenTheSameInputGivesAnotherVector(t *testing.T) {
	f := &fakeModels{dim: 6, noisy: true}
	r := probeOne(t, cfgWith(f.serve(t), map[string]dunconfig.Facet{"embed": {Model: "emb"}}), t.TempDir(), "embed")
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	d := fact(t, r.Caps, CapEmbedDeterministic)
	if d.Value != "false" || !strings.Contains(d.Evidence, "cosine") {
		t.Errorf("deterministic %q (%s), want false with the cosine", d.Value, d.Evidence)
	}
	if v := fact(t, r.Caps, CapEmbedBatchMax).Value; v != "64" {
		t.Errorf("batch_max %s, want 64 (no limit)", v)
	}
}

func TestProbeEmbedFailingOneInputIsAnError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(ts.Close)
	r := probeOne(t, cfgWith(ts.URL, map[string]dunconfig.Facet{"embed": {Model: "emb"}}), t.TempDir(), "embed")
	if r.Err == nil {
		t.Fatalf("no error: %s", r.Report())
	}
	if _, ok := r.Caps.Facts[CapEmbedDimension]; ok {
		t.Error("a dimension was recorded from no vector")
	}
}

// The OCR fixture is a real PNG; a model that reads it exactly scores 0, one
// that misreads scores the edit distance over the length.
func TestProbeVisionScoresCharacterErrorRate(t *testing.T) {
	img, err := png.Decode(bytes.NewReader(ocrFixture()))
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() < 300 || b.Dy() < 80 {
		t.Fatalf("fixture is %dx%d: too small to read", b.Dx(), b.Dy())
	}
	answer := "DUN OCR PROBE 2026\nquick brown fox 0123"
	f := &fakeModels{chat: func(map[string]any) string { return answer }}
	url := f.serve(t)
	cfg := cfgWith(url, map[string]dunconfig.Facet{"vision": {Model: "vlm"}})
	r := probeOne(t, cfg, t.TempDir(), "vision")
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	if v := fact(t, r.Caps, CapOCRCER).Value; v != "0.000" {
		t.Errorf("cer %s for an exact read, want 0.000", v)
	}
	if !strings.Contains(f.bodies[0], "data:image/png;base64,") {
		t.Error("the request carried no image")
	}

	answer = "DUN 0CR PROBE 2026 quick brown fox" // one substitution, five dropped
	r = probeOne(t, cfg, t.TempDir(), "vision")
	want := fmt.Sprintf("%.3f", 6.0/float64(len("DUN OCR PROBE 2026 quick brown fox 0123")))
	if v := fact(t, r.Caps, CapOCRCER).Value; v != want {
		t.Errorf("cer %s, want %s", v, want)
	}
}

// The segment probe runs raglit's segmenter: valid emit_fragments that hold
// the document passes; answers that never validate fail, with raglit's reason.
func TestProbeSegmentSchemaCompliance(t *testing.T) {
	good, _ := json.Marshal(map[string]any{"continues_previous": false, "fragments": []map[string]string{
		{"text": segmentFixture[:strings.Index(segmentFixture, "Garden notes")]},
		{"text": segmentFixture[strings.Index(segmentFixture, "Garden notes"):]},
	}})
	reply := string(good)
	f := &fakeModels{chat: func(map[string]any) string { return reply }}
	cfg := cfgWith(f.serve(t), map[string]dunconfig.Facet{"segment": {Model: "seg"}})
	r := probeOne(t, cfg, t.TempDir(), "segment")
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	if s := fact(t, r.Caps, CapSegmentSchema); s.Value != "true" || !strings.Contains(s.Evidence, "2 fragments") {
		t.Errorf("segment %q (%s), want true with 2 fragments", s.Value, s.Evidence)
	}

	reply = `{"fragments": "not a list"}`
	r = probeOne(t, cfg, t.TempDir(), "segment")
	if s := fact(t, r.Caps, CapSegmentSchema); s.Value != "false" || !strings.Contains(s.Evidence, "fell back") {
		t.Errorf("segment %q (%s), want false: fell back", s.Value, s.Evidence)
	}

	reply = `{"continues_previous": false, "fragments": [{"text": "Harbor operations"}]}`
	r = probeOne(t, cfg, t.TempDir(), "segment")
	if s := fact(t, r.Caps, CapSegmentSchema); s.Value != "false" || !strings.Contains(s.Evidence, "of the document's words") {
		t.Errorf("segment %q (%s), want false: words missing", s.Value, s.Evidence)
	}
}

// identity runs the chat probes; a role that is off is said, not probed.
func TestProbeIdentityRunsTheChatProbesAndOffRolesAreSaid(t *testing.T) {
	f := &fakeModels{chat: func(map[string]any) string { return "sure" }}
	cfg := cfgWith(f.serve(t), map[string]dunconfig.Facet{"identity": {Model: "chat"}})
	res, err := Probe(context.Background(), cfg, t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]ProbeResult{}
	for _, r := range res {
		by[r.Facet] = r
	}
	if len(by) != 4 {
		t.Fatalf("%d roles, want 4", len(by))
	}
	id := by["identity"]
	if _, ok := id.Caps.Facts[llm.CapToolsChoiceForced]; !ok {
		t.Errorf("no chat-probe fact: %s", id.Report())
	}
	for _, off := range []string{"vision", "segment"} {
		if by[off].Caps.Model != "" || !strings.Contains(by[off].Report(), "off") {
			t.Errorf("%s: %q, want off and not probed", off, by[off].Report())
		}
	}
	if _, err := Probe(context.Background(), cfg, t.TempDir(), "bogus"); err == nil {
		t.Error("an unknown role was probed")
	}
}

// openWith is a Host with an embed role on url.
func openWith(t *testing.T, root, url, model, capsDir string) *Host {
	t.Helper()
	h, err := Open(root, Roles{Models: raglit.Models{Embed: raglit.ModelRole{BaseURL: url, Model: model}}, CapsDir: capsDir}, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func storeOf(t *testing.T, h *Host, name string) *raglit.Store {
	t.Helper()
	st, err := h.Existing(name)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func waitNoMissing(t *testing.T, st *raglit.Store) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		n, err := st.MissingVectors()
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d fragments still have no vector", n)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// The index model record: an index records the embed model and dimension it
// was built with. Opened with another embed model, it is searched by text
// only and gets no vectors, until a re-embed, which rebuilds them.
func TestIndexModelRecordRefusesAnotherVectorSpaceUntilReembed(t *testing.T) {
	f := &fakeModels{dim: 8}
	url := f.serve(t)
	root, caps := t.TempDir(), t.TempDir()
	// The probe's dimension is what the index records.
	if r := probeOne(t, cfgWith(url, map[string]dunconfig.Facet{"embed": {Model: "nomic-embed-text"}}), caps, "embed"); r.Err != nil {
		t.Fatal(r.Err)
	}

	h := openWith(t, root, url, "nomic-embed-text", caps)
	ingest(t, h, proto.RagIngest{Project: "p", Index: "docs", Path: writeDoc(t, "a.md", "The harbor pilot logged a zeppelin sighting.\n")})
	st := storeOf(t, h, "p/docs")
	if st.EmbedModel() != "nomic-embed-text" || metaInt(st, metaEmbedDim) != 8 {
		t.Fatalf("record %q dim %d, want nomic-embed-text 8", st.EmbedModel(), metaInt(st, metaEmbedDim))
	}
	if got := search(t, h, proto.RagSearch{Project: "p", Index: "docs", Query: "zeppelin"}); got.Mode != "hybrid" || len(got.Hits) == 0 {
		t.Fatalf("search %+v, want hybrid with hits", got)
	}
	h.Close(time.Second)

	// Another model: text search only, said, and no vector written.
	h = openWith(t, root, url, "nomic-embed-text-v1.5", caps)
	t.Cleanup(func() { h.Close(time.Second) })
	got := search(t, h, proto.RagSearch{Project: "p", Index: "docs", Query: "zeppelin"})
	if got.Mode != "fts" || !strings.Contains(got.Note, "nomic-embed-text (dimension 8)") || len(got.Hits) == 0 {
		t.Fatalf("search %+v, want fts with hits and the record named", got)
	}
	ingest(t, h, proto.RagIngest{Project: "p", Index: "docs", Path: writeDoc(t, "b.md", "Tomatoes want full sun.\n")})
	st = storeOf(t, h, "p/docs")
	if n, _ := st.MissingVectors(); n == 0 {
		t.Fatal("a vector was written to an index of another model's space")
	}
	if st.EmbedModel() != "nomic-embed-text" {
		t.Fatalf("opening changed the record to %q", st.EmbedModel())
	}

	re, err := h.Reembed(proto.RagReembed{Project: "p", Index: "docs"})
	if err != nil {
		t.Fatal(err)
	}
	if !re.Dropped || re.Model != "nomic-embed-text-v1.5" || re.Missing < 2 {
		t.Fatalf("reembed %+v, want dropped, the new model, both documents missing", re)
	}
	waitNoMissing(t, st)
	if st.EmbedModel() != "nomic-embed-text-v1.5" {
		t.Fatalf("record %q after reembed", st.EmbedModel())
	}
	if got := search(t, h, proto.RagSearch{Project: "p", Index: "docs", Query: "zeppelin"}); got.Mode != "hybrid" || got.Note != "" {
		t.Fatalf("after reembed: %+v, want hybrid", got)
	}
	if _, err := h.Reembed(proto.RagReembed{Project: "p", Index: "code"}); err == nil {
		t.Error("reembed of an index that does not exist answered")
	}
}

// A dimension that differs under the same model id also refuses, and a
// re-embed says it cannot drop vectors of the same model.
func TestIndexModelRecordRefusesAnotherDimension(t *testing.T) {
	f := &fakeModels{dim: 8}
	url := f.serve(t)
	root, caps := t.TempDir(), t.TempDir()
	cfg := cfgWith(url, map[string]dunconfig.Facet{"embed": {Model: "nomic-embed-text"}})
	probeOne(t, cfg, caps, "embed")
	h := openWith(t, root, url, "nomic-embed-text", caps)
	ingest(t, h, proto.RagIngest{Index: "memories", Path: writeDoc(t, "a.md", "narwhal tusk survey\n")})
	h.Close(time.Second)

	f.dim = 16
	probeOne(t, cfg, caps, "embed")
	h = openWith(t, root, url, "nomic-embed-text", caps)
	t.Cleanup(func() { h.Close(time.Second) })
	got := search(t, h, proto.RagSearch{Index: "memories", Query: "narwhal"})
	if got.Mode != "fts" || !strings.Contains(got.Note, "dimension 16") {
		t.Fatalf("search %+v, want fts naming dimension 16", got)
	}
	if _, err := h.Reembed(proto.RagReembed{Index: "memories"}); err == nil || !strings.Contains(err.Error(), "same model") {
		t.Fatalf("reembed: %v, want the same-model refusal", err)
	}
}

// Fragments ingested while embed was off get vectors when embed is on.
func TestOpenBackfillsVectorsOfAnIndexWithoutThem(t *testing.T) {
	root := t.TempDir()
	h := open(t, root)
	ingest(t, h, proto.RagIngest{Index: "memories", Path: writeDoc(t, "a.md", "narwhal tusk survey\n")})
	h.Close(time.Second)

	f := &fakeModels{dim: 8}
	h = openWith(t, root, f.serve(t), "nomic-embed-text", "")
	t.Cleanup(func() { h.Close(time.Second) })
	st := storeOf(t, h, "global/memories")
	waitNoMissing(t, st)
	if got := search(t, h, proto.RagSearch{Index: "memories", Query: "narwhal"}); got.Mode != "hybrid" || len(got.Hits) == 0 {
		t.Fatalf("search %+v, want hybrid with hits", got)
	}
}

// A re-embed is refused while the index has a running ingest job: raglit's
// SetEmbedder is a plain field write that the job's worker reads. Once the
// job ends, it runs.
func TestReembedRefusedWhileAJobRuns(t *testing.T) {
	f := &fakeModels{dim: 8}
	h := openWith(t, t.TempDir(), f.serve(t), "nomic-embed-text", "")
	t.Cleanup(func() { h.Close(time.Second) })
	ingest(t, h, proto.RagIngest{Project: "p", Index: "docs", Path: writeDoc(t, "a.md", "zeppelin sighting\n")})

	// A source that answers only when released holds its job in running.
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("slow harbor notes\n"))
	}))
	t.Cleanup(slow.Close)
	r, err := h.Ingest(proto.RagIngest{Project: "p", Index: "docs", URL: slow.URL + "/slow.txt"})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		js, err := h.Jobs(proto.RagJobs{Project: "p", Index: "docs", State: "running"})
		if err != nil {
			t.Fatal(err)
		}
		if len(js.Jobs) > 0 {
			break
		}
		if time.Now().After(deadline) {
			close(release)
			t.Fatal("the slow job never ran")
		}
		time.Sleep(20 * time.Millisecond)
	}
	_, err = h.Reembed(proto.RagReembed{Project: "p", Index: "docs"})
	if err == nil || !strings.Contains(err.Error(), "1 running ingest job") || !strings.Contains(err.Error(), "--project p --index docs") {
		close(release)
		t.Fatalf("reembed with a running job: %v, want the refusal", err)
	}
	close(release)
	waitDone(t, h, "p", "docs", r.Job)
	if _, err := h.Reembed(proto.RagReembed{Project: "p", Index: "docs"}); err != nil {
		t.Fatalf("reembed after the job ended: %v", err)
	}
}
