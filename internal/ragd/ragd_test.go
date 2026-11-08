package ragd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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

	// Logged out: no default embed.
	if r := Resolve(dunconfig.DunConfig{}); r.Models.Embed.Model != "" {
		t.Fatalf("embed with no provider: %+v", r.Models.Embed)
	}
}
