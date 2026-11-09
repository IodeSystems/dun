package ragd

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/dun/internal/dunconfig"
	"github.com/iodesystems/dun/proto"
)

// open is a Host with no models: FTS only, offline.
func open(t *testing.T, root string) *Host {
	t.Helper()
	h, err := Open(root, Roles{}, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close(time.Second) })
	return h
}

func writeDoc(t *testing.T, name, text string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// waitDone waits for job id on an index to end, and fails unless it is done.
func waitDone(t *testing.T, h *Host, project, index string, id int64) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		js, err := h.Jobs(proto.RagJobs{Project: project, Index: index})
		if err != nil {
			t.Fatal(err)
		}
		for _, j := range js.Jobs {
			if j.ID != id {
				continue
			}
			switch j.State {
			case "done":
				return
			case "error":
				t.Fatalf("job %d: %s", id, j.Error)
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %d not done after 30s: %+v", id, js.Jobs)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func ingest(t *testing.T, h *Host, q proto.RagIngest) {
	t.Helper()
	r, err := h.Ingest(q)
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, h, q.Project, q.Index, r.Job)
}

func search(t *testing.T, h *Host, q proto.RagSearch) *proto.RagHits {
	t.Helper()
	r, err := h.Search(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestIngestMarkdownThenSearchFTS(t *testing.T) {
	h := open(t, t.TempDir())
	doc := writeDoc(t, "notes.md", "# Harbor\n\nThe harbor pilot logged a zeppelin sighting.\n\nUnrelated line about gardening.\n")
	ingest(t, h, proto.RagIngest{Project: "dun", Index: "docs", Path: doc})

	r := search(t, h, proto.RagSearch{Project: "dun", Index: "docs", Query: "zeppelin"})
	if len(r.Hits) == 0 {
		t.Fatal("search found nothing after the job finished")
	}
	if r.Hits[0].Path != "file://"+doc || !strings.Contains(r.Hits[0].Text, "zeppelin") {
		t.Fatalf("hit = %+v", r.Hits[0])
	}
	// Hybrid was asked (the default) with no embed model: it says so and
	// runs by text.
	if r.Mode != "fts" || r.Note == "" {
		t.Fatalf("mode %q note %q, want fts with a note", r.Mode, r.Note)
	}
	if r := search(t, h, proto.RagSearch{Project: "dun", Index: "docs", Query: "zeppelin", Mode: "fts",
		Filter: proto.RagFilter{PathPrefix: "file:///nowhere/"}}); len(r.Hits) != 0 {
		t.Fatalf("a path prefix that excludes the file: %d hits", len(r.Hits))
	}
}

func TestTagsFilterBeforeRanking(t *testing.T) {
	h := open(t, t.TempDir())
	a := writeDoc(t, "a.md", "walrus migration notes\n")
	b := writeDoc(t, "b.md", "walrus feeding notes\n")
	ingest(t, h, proto.RagIngest{Project: "dun", Index: "memories", Path: a, Tags: []string{"Lead  Paint", "Build"}})
	ingest(t, h, proto.RagIngest{Project: "dun", Index: "memories", Path: b})

	q := proto.RagSearch{Project: "dun", Index: "memories", Query: "walrus", Mode: "fts"}
	if r := search(t, h, q); len(r.Hits) < 2 {
		t.Fatalf("unfiltered: %d hits, want both documents", len(r.Hits))
	}
	// Normalized on write and on read: "lead paint" matches "Lead  Paint".
	q.Filter.Tags = []string{"lead paint", "build"}
	r := search(t, h, q)
	if len(r.Hits) == 0 {
		t.Fatal("tag filter found nothing")
	}
	for _, x := range r.Hits {
		if x.Path != "file://"+a {
			t.Fatalf("tag filter let through %s", x.Path)
		}
	}
	// Whole tags, AND: "paint" is not "lead paint"; a missing tag excludes.
	for _, tags := range [][]string{{"paint"}, {"build", "absent"}} {
		q.Filter.Tags = tags
		if r := search(t, h, q); len(r.Hits) != 0 {
			t.Fatalf("tags %v: %d hits, want none", tags, len(r.Hits))
		}
	}
}

func TestGlobalAndProjectAreSeparate(t *testing.T) {
	h := open(t, t.TempDir())
	g := writeDoc(t, "g.md", "global lighthouse keeper\n")
	p := writeDoc(t, "p.md", "project lighthouse keeper\n")
	ingest(t, h, proto.RagIngest{Index: "memories", Path: g})
	ingest(t, h, proto.RagIngest{Project: "dun", Index: "memories", Path: p})

	for _, c := range []struct{ project, want string }{{"", g}, {"dun", p}} {
		r := search(t, h, proto.RagSearch{Project: c.project, Index: "memories", Query: "lighthouse", Mode: "fts"})
		if len(r.Hits) != 1 || r.Hits[0].Path != "file://"+c.want {
			t.Fatalf("project %q: hits %+v, want only %s", c.project, r.Hits, c.want)
		}
	}
	if r := search(t, h, proto.RagSearch{Project: "other", Index: "memories", Query: "lighthouse"}); len(r.Hits) != 0 {
		t.Fatalf("another project sees %d hits", len(r.Hits))
	}
	for _, d := range []string{"global/memories", "dun/memories"} {
		if _, err := os.Stat(filepath.Join(h.root, d, "index.sqlite")); err != nil {
			t.Fatalf("layout: %v", err)
		}
	}
}

func TestUnknownIndexIsRefused(t *testing.T) {
	h := open(t, t.TempDir())
	doc := writeDoc(t, "x.md", "x\n")
	for _, c := range []struct{ project, index string }{
		{"dun", "bogus"}, {"", "code"}, {"", "docs"}, {"global", "memories"}, {"../up", "docs"}, {"Dun", "docs"}, {"dun", ""},
	} {
		if _, err := Name(c.project, c.index); err == nil {
			t.Errorf("Name(%q, %q) accepted", c.project, c.index)
		}
		if _, err := h.Search(context.Background(), proto.RagSearch{Project: c.project, Index: c.index, Query: "x"}); err == nil {
			t.Errorf("search %q/%q accepted", c.project, c.index)
		}
		if _, err := h.Ingest(proto.RagIngest{Project: c.project, Index: c.index, Path: doc}); err == nil {
			t.Errorf("ingest %q/%q accepted", c.project, c.index)
		}
	}
	if _, err := h.Search(context.Background(), proto.RagSearch{Index: "memories", Query: "x", Mode: "vector"}); err == nil {
		t.Error("an unknown mode was accepted")
	}
	if _, err := h.Ingest(proto.RagIngest{Index: "memories", Path: "relative.md"}); err == nil {
		t.Error("a relative path was accepted")
	}
	if ents, _ := os.ReadDir(h.root); len(ents) != 0 {
		t.Fatalf("refused requests made %d entries under the root", len(ents))
	}
}

func TestExistingDoesNotCreate(t *testing.T) {
	h := open(t, t.TempDir())
	if _, err := h.Existing("dun/docs"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Existing of an unopened index: %v", err)
	}
	// Neither does a search or a job list of an index with nothing in it.
	if r := search(t, h, proto.RagSearch{Project: "dun", Index: "docs", Query: "x"}); len(r.Hits) != 0 {
		t.Fatal("hits from an index that does not exist")
	}
	if js, err := h.Jobs(proto.RagJobs{Project: "dun", Index: "docs"}); err != nil || len(js.Jobs) != 0 {
		t.Fatalf("jobs %v, err %v", js, err)
	}
	if _, err := os.Stat(filepath.Join(h.root, "dun")); !os.IsNotExist(err) {
		t.Fatalf("a read made the project's directory: %v", err)
	}
	if n := h.Names(); len(n) != 0 {
		t.Fatalf("names %v", n)
	}
	// The first ingest makes it, and registers it with the runner.
	if _, err := h.Ingest(proto.RagIngest{Project: "dun", Index: "docs", Path: writeDoc(t, "d.md", "d\n")}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Existing("dun/docs"); err != nil {
		t.Fatalf("after ingest: %v", err)
	}
}

func TestCloseDrainsAndTheNextStartResumes(t *testing.T) {
	root := t.TempDir()
	h, err := Open(root, Roles{}, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	r, err := h.Ingest(proto.RagIngest{Project: "dun", Index: "docs", Path: writeDoc(t, "q.md", "quokka census\n")})
	if err != nil {
		t.Fatal(err)
	}
	if !h.Close(5 * time.Second) {
		t.Fatal("drain did not finish")
	}
	if _, err := h.Ingest(proto.RagIngest{Project: "dun", Index: "docs", Path: writeDoc(t, "z.md", "z\n")}); err == nil {
		t.Fatal("ingest after Close was accepted")
	}
	// Whether the job ran before the drain or not, the next dund opens the
	// index at start and the job ends done, not error.
	h2 := open(t, root)
	if n := h2.Names(); len(n) != 1 || n[0] != "dun/docs" {
		t.Fatalf("names at start: %v", n)
	}
	waitDone(t, h2, "dun", "docs", r.Job)
	if r := search(t, h2, proto.RagSearch{Project: "dun", Index: "docs", Query: "quokka"}); len(r.Hits) == 0 {
		t.Fatal("search found nothing after restart")
	}
}

func TestResolveFacets(t *testing.T) {
	cfg := dunconfig.DunConfig{Provider: "gw", Providers: []dunconfig.ProviderConfig{
		{Name: "gw", URL: "http://gw:4000", Key: "k1"}, {Name: "far", URL: "https://far", Key: "k2"}}}
	r := Resolve(cfg)
	if e := r.Models.Embed; e.Model != dunconfig.DefaultEmbedModel || e.BaseURL != "http://gw:4000" || e.APIKey != "k1" {
		t.Fatalf("default embed = %+v", e)
	}
	if r.Models.Vision.Model != "" || r.Models.Identity.Model != "" || r.Models.Segment.Model != "" {
		t.Fatalf("roles on with no facets: %+v", r.Models)
	}
	if len(r.Said) != 4 {
		t.Fatalf("said %q", r.Said)
	}

	cfg.Facets = map[string]dunconfig.Facet{
		"embed":    {Provider: "far", Model: "e5"},
		"vision":   {Provider: "gone", Model: "v"},
		"identity": {Model: "cap"},
		"typo":     {Model: "x"},
	}
	r = Resolve(cfg)
	if e := r.Models.Embed; e.Model != "e5" || e.BaseURL != "https://far" || e.APIKey != "k2" {
		t.Fatalf("embed = %+v", e)
	}
	if r.Models.Vision.Model != "" {
		t.Fatalf("vision on an unsaved provider: %+v", r.Models.Vision)
	}
	if i := r.Models.Identity; i.Model != "cap" || i.BaseURL != "http://gw:4000" {
		t.Fatalf("identity = %+v", i)
	}
	all := strings.Join(r.Said, "\n")
	for _, want := range []string{"vision: off (no saved provider gone", "typo: not a role"} {
		if !strings.Contains(all, want) {
			t.Fatalf("said %q, missing %q", all, want)
		}
	}

	// The unified resolver: the old vision_model serves the vision role, and
	// the chat roles are roles, not typos.
	cfg.Facets = map[string]dunconfig.Facet{"main": {Model: "chat"}, "child": {Model: "kid"}}
	cfg.VisionModel, cfg.VisionProvider = "vl", "far"
	r = Resolve(cfg)
	if v := r.Models.Vision; v.Model != "vl" || v.BaseURL != "https://far" {
		t.Fatalf("vision from vision_model = %+v", v)
	}
	if all := strings.Join(r.Said, "\n"); strings.Contains(all, "not a role") {
		t.Fatalf("main or child said as no role: %q", all)
	}

	// Logged out: no default embed.
	if r := Resolve(dunconfig.DunConfig{}); r.Models.Embed.Model != "" {
		t.Fatalf("embed with no provider: %+v", r.Models.Embed)
	}
}

// waitIdle waits until an index has no pending or running job.
func waitIdle(t *testing.T, h *Host, project, index string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		busy := 0
		for _, s := range []string{"pending", "running"} {
			js, err := h.Jobs(proto.RagJobs{Project: project, Index: index, State: s})
			if err != nil {
				t.Fatal(err)
			}
			busy += len(js.Jobs)
		}
		if busy == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s/%s: %d jobs still queued after 30s", project, index, busy)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Sync queues what the index lacks or holds stale, skips what is current,
// and removes what is gone; Forget removes one document and is idempotent.
func TestSyncAndForget(t *testing.T) {
	h := open(t, t.TempDir())
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.md"), filepath.Join(dir, "b.md")
	for p, text := range map[string]string{a: "ocelot den\n", b: "capybara pond\n"} {
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	files := []proto.RagSyncFile{{Path: a, Tags: []string{"wild"}}, {Path: b}}
	r, err := h.Sync(proto.RagSync{Project: "dun", Index: "memories", Files: files})
	if err != nil || r.Queued != 2 || r.Current != 0 {
		t.Fatalf("first sync %+v %v, want 2 queued", r, err)
	}
	waitIdle(t, h, "dun", "memories")
	if r, err = h.Sync(proto.RagSync{Project: "dun", Index: "memories", Files: files}); err != nil || r.Queued != 0 || r.Current != 2 {
		t.Fatalf("second sync %+v %v, want both current", r, err)
	}
	q := proto.RagSearch{Project: "dun", Index: "memories", Query: "ocelot", Mode: "fts", Filter: proto.RagFilter{Tags: []string{"wild"}}}
	if got := search(t, h, q); len(got.Hits) == 0 {
		t.Fatal("a synced file's tags do not filter")
	}

	// Changed bytes: queued again. Gone: removed.
	if err := os.WriteFile(a, []byte("ocelot den, moved\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if r, err = h.Sync(proto.RagSync{Project: "dun", Index: "memories", Files: files[:1], Gone: []string{b}}); err != nil || r.Queued != 1 || r.Removed != 1 {
		t.Fatalf("sync after a change %+v %v, want 1 queued, 1 removed", r, err)
	}
	waitIdle(t, h, "dun", "memories")
	if got := search(t, h, proto.RagSearch{Project: "dun", Index: "memories", Query: "capybara", Mode: "fts"}); len(got.Hits) != 0 {
		t.Fatalf("a gone file still found: %+v", got.Hits)
	}
	if got := search(t, h, proto.RagSearch{Project: "dun", Index: "memories", Query: "moved", Mode: "fts"}); len(got.Hits) == 0 {
		t.Fatal("the changed text is not indexed")
	}

	f, err := h.Forget(proto.RagForget{Project: "dun", Index: "memories", Path: a})
	if err != nil || !f.Removed {
		t.Fatalf("forget %+v %v", f, err)
	}
	if f, err = h.Forget(proto.RagForget{Project: "dun", Index: "memories", Path: a}); err != nil || f.Removed {
		t.Fatalf("second forget %+v %v, want not removed and no error", f, err)
	}
	// An index that does not exist: nothing made.
	if r, err := h.Sync(proto.RagSync{Project: "other", Index: "memories", Gone: []string{a}}); err != nil || r.Removed != 0 {
		t.Fatalf("sync of no index %+v %v", r, err)
	}
	if _, err := os.Stat(filepath.Join(h.root, "other")); err == nil {
		t.Fatal("a gone-only sync made an index")
	}
}

// A note is one hit in a raw search, for a word in its name and its detail
// both: it is ingested as text (its name and detail, one fragment), its
// frontmatter is no fragment, and its tags are filter data (SetTags), not a
// caption. Before raglit v0.6 the same note came back up to three times.
func TestTaggedNoteIsOneHit(t *testing.T) {
	h := open(t, t.TempDir())
	note := writeDoc(t, "pelican.md", "---\nname: pelican-drill\nkind: strategy\ndate: 2026-11-08\npublic: false\n---\nRun the pelican drill before a release.\n")
	text := "pelican-drill\n\nRun the pelican drill before a release."
	r, err := h.Ingest(proto.RagIngest{Project: "dun", Index: "strategies", Path: note, Title: "strategy:pelican-drill",
		Tags: []string{"drills", "kind:strategy"}, Text: text})
	if err != nil || r.Job != 0 {
		t.Fatalf("ingest %+v %v, want no job", r, err)
	}
	for _, f := range []proto.RagFilter{{}, {Tags: []string{"kind:strategy"}}, {Tags: []string{"drills", "kind:strategy"}}} {
		for _, q := range []string{"pelican", "release", "drill"} {
			r := search(t, h, proto.RagSearch{Project: "dun", Index: "strategies", Query: q, Mode: "fts", Filter: f})
			if len(r.Hits) != 1 || strings.Contains(r.Hits[0].Text, "---") {
				t.Fatalf("%q, filter %+v: %d hits, want 1 with no frontmatter: %+v", q, f, len(r.Hits), r.Hits)
			}
		}
	}
	// Frontmatter words are not text: "public" is not found.
	if r := search(t, h, proto.RagSearch{Project: "dun", Index: "strategies", Query: "public", Mode: "fts"}); len(r.Hits) != 0 {
		t.Fatalf("a frontmatter word: %d hits", len(r.Hits))
	}
	// "kind" is not the tag "kind:strategy": whole tags only.
	if r := search(t, h, proto.RagSearch{Project: "dun", Index: "strategies", Query: "pelican", Mode: "fts",
		Filter: proto.RagFilter{Tags: []string{"kind"}}}); len(r.Hits) != 0 {
		t.Fatalf("tag kind: %d hits, want 0", len(r.Hits))
	}

	// Sync with text: the hash of the ingested text decides. The file's
	// frontmatter changing alone (marked important) is current; new text is
	// ingested again; gone is removed, its hash with it.
	if err := os.WriteFile(note, []byte("---\nname: pelican-drill\nkind: strategy\nimportant: true\n---\nRun the pelican drill before a release.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sf := proto.RagSyncFile{Path: note, Title: "strategy:pelican-drill", Tags: []string{"kind:strategy", "important"}, Text: text}
	if got, err := h.Sync(proto.RagSync{Project: "dun", Index: "strategies", Files: []proto.RagSyncFile{sf}}); err != nil || got.Current != 1 || got.Queued != 0 {
		t.Fatalf("sync of the same text: %+v %v, want current", got, err)
	}
	if r := search(t, h, proto.RagSearch{Project: "dun", Index: "strategies", Query: "pelican", Mode: "fts",
		Filter: proto.RagFilter{Tags: []string{"important"}}}); len(r.Hits) != 1 {
		t.Fatalf("a current file's tags not set: %d hits", len(r.Hits))
	}
	sf.Text = "pelican-drill\n\nRun the pelican drill after a release."
	if got, err := h.Sync(proto.RagSync{Project: "dun", Index: "strategies", Files: []proto.RagSyncFile{sf}}); err != nil || got.Queued != 1 {
		t.Fatalf("sync of new text: %+v %v, want it ingested", got, err)
	}
	if r := search(t, h, proto.RagSearch{Project: "dun", Index: "strategies", Query: "after", Mode: "fts"}); len(r.Hits) != 1 {
		t.Fatalf("new text: %d hits", len(r.Hits))
	}
	if got, err := h.Sync(proto.RagSync{Project: "dun", Index: "strategies", Gone: []string{note}}); err != nil || got.Removed != 1 {
		t.Fatalf("sync gone: %+v %v", got, err)
	}
	if v, _ := storeOf(t, h, "dun/strategies").Meta(textHashKey("file://" + note)); v != "" {
		t.Fatalf("the text hash outlived its document: %q", v)
	}
	if js, err := h.Jobs(proto.RagJobs{Project: "dun", Index: "strategies"}); err != nil || len(js.Jobs) != 0 {
		t.Fatalf("jobs %+v %v, want none: text is ingested directly", js, err)
	}
}

// RagTag replaces the tags of a committed document with no ingest job, and
// tags set before the first ingest are applied at its commit.
func TestTagWithoutReingest(t *testing.T) {
	h := open(t, t.TempDir())
	a := writeDoc(t, "a.md", "heron watch\n")
	ingest(t, h, proto.RagIngest{Project: "dun", Index: "memories", Path: a, Tags: []string{"kind:memory"}})
	q := proto.RagSearch{Project: "dun", Index: "memories", Query: "heron", Mode: "fts", Filter: proto.RagFilter{Tags: []string{"important"}}}
	if r := search(t, h, q); len(r.Hits) != 0 {
		t.Fatalf("before the tag: %d hits", len(r.Hits))
	}
	if _, err := h.Tag(proto.RagTag{Project: "dun", Index: "memories", Path: a, Tags: []string{"kind:memory", "important"}}); err != nil {
		t.Fatal(err)
	}
	if r := search(t, h, q); len(r.Hits) != 1 {
		t.Fatalf("after the tag: %d hits, want 1", len(r.Hits))
	}
	if js, err := h.Jobs(proto.RagJobs{Project: "dun", Index: "memories"}); err != nil || len(js.Jobs) != 1 {
		t.Fatalf("jobs %+v %v, want the one ingest", js, err)
	}

	// Tag, then ingest: the commit applies the held tags.
	b := writeDoc(t, "b.md", "heron nest\n")
	if _, err := h.Tag(proto.RagTag{Project: "dun", Index: "memories", Path: b, Tags: []string{"important"}}); err != nil {
		t.Fatal(err)
	}
	ingest(t, h, proto.RagIngest{Project: "dun", Index: "memories", Path: b})
	if r := search(t, h, q); len(r.Hits) != 2 {
		t.Fatalf("after the held tag's ingest: %d hits, want 2", len(r.Hits))
	}
}

// Two runners: memories and strategies in one, code and docs in the other.
// A memory note is ingested while a docs job is held running, and Close
// waits for both runners before it closes the indexes.
func TestTwoRunners(t *testing.T) {
	root := t.TempDir()
	h, err := Open(root, Roles{}, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.runners) != 2 {
		t.Fatalf("%d runners, want 2", len(h.runners))
	}
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
	if _, err := h.Ingest(proto.RagIngest{Project: "dun", Index: "docs", URL: slow.URL + "/slow.txt"}); err != nil {
		t.Fatal(err)
	}
	ingest(t, h, proto.RagIngest{Project: "dun", Index: "memories", Path: writeDoc(t, "m.md", "egret roost\n")})
	ingest(t, h, proto.RagIngest{Index: "strategies", Path: writeDoc(t, "s.md", "ibis wade\n")})

	mem, code := storeSubset{h, []string{"memories", "strategies"}}, storeSubset{h, []string{"code", "docs"}}
	if n := mem.Names(); !slices.Equal(n, []string{"dun/memories", "global/strategies"}) {
		t.Fatalf("memory runner's indexes %v", n)
	}
	if n := code.Names(); !slices.Equal(n, []string{"dun/docs"}) {
		t.Fatalf("code runner's indexes %v", n)
	}
	if _, err := mem.Existing("dun/docs"); err == nil {
		t.Fatal("the memory runner sees dun/docs")
	}

	go func() { time.Sleep(200 * time.Millisecond); close(release) }()
	if !h.Close(5 * time.Second) {
		t.Fatal("Close: a job did not stop in time")
	}
	for _, r := range h.runners {
		select {
		case <-r.Done():
		default:
			t.Fatal("Close returned before a runner was done")
		}
	}
	if n := h.Names(); len(n) != 0 {
		t.Fatalf("indexes still open after Close: %v", n)
	}
}
