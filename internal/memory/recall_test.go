package memory

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iodesystems/dun/internal/raglit"
	"github.com/iodesystems/dun/proto"
)

// fakeRag is a Rag that records calls and answers searches from hits;
// down makes every call fail as unreachable (harness memrecall_test.go has
// the full one).
type fakeRag struct {
	mu      sync.Mutex
	down    bool
	ingests []proto.RagIngest
	forgets []proto.RagForget
	syncs   []proto.RagSync
	tags    []proto.RagTag
	hits    map[string][]proto.RagHit // by project ("" global)
	mode    string
}

func (f *fakeRag) isDown() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.down
}

func (f *fakeRag) setDown(v bool) {
	f.mu.Lock()
	f.down = v
	f.mu.Unlock()
}

func (f *fakeRag) Search(_ context.Context, q proto.RagSearch) (*proto.RagHits, error) {
	if f.isDown() {
		return nil, ErrDown
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	mode := f.mode
	if mode == "" {
		mode = "fts"
	}
	return &proto.RagHits{Mode: mode, Hits: f.hits[q.Project]}, nil
}

func (f *fakeRag) Ingest(_ context.Context, q proto.RagIngest) (*proto.RagIngested, error) {
	if f.isDown() {
		return nil, ErrDown
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ingests = append(f.ingests, q)
	return &proto.RagIngested{Job: int64(len(f.ingests)), URL: "file://" + q.Path}, nil
}

func (f *fakeRag) Forget(_ context.Context, q proto.RagForget) (*proto.RagForgotten, error) {
	if f.isDown() {
		return nil, ErrDown
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forgets = append(f.forgets, q)
	return &proto.RagForgotten{Removed: true}, nil
}

func (f *fakeRag) Sync(_ context.Context, q proto.RagSync) (*proto.RagSynced, error) {
	if f.isDown() {
		return nil, ErrDown
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.syncs = append(f.syncs, q)
	return &proto.RagSynced{Queued: len(q.Files), Removed: len(q.Gone)}, nil
}

func (f *fakeRag) Tag(_ context.Context, q proto.RagTag) (*proto.RagTagged, error) {
	if f.isDown() {
		return nil, ErrDown
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tags = append(f.tags, q)
	return &proto.RagTagged{URL: "file://" + q.Path}, nil
}

func (f *fakeRag) syncCalls() []proto.RagSync {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.syncs)
}

// memWith is a store and finder over f, in a temp DUN_HOME, with the
// notices it gave.
func memWith(t *testing.T, f *fakeRag) (*Store, *Finder, *[]string) {
	t.Helper()
	t.Setenv("DUN_HOME", t.TempDir())
	var mu sync.Mutex
	var notices []string
	l := NewLink(f, "proj", func(s string) { mu.Lock(); notices = append(notices, s); mu.Unlock() })
	m := NotesFor(l, t.TempDir())
	m.dir = memoryDir("proj")
	return m, &Finder{l: l, m: m}, &notices
}

// A note goes to the memories index of its scope, with its tags; global
// notes live in their own directory.
func TestMemRecall_AddIngestsIntoTheScopesIndexWithTags(t *testing.T) {
	f := &fakeRag{}
	m, _, _ := memWith(t, f)
	out, err := m.apply(toolArgs{Action: "add", Content: "Builds need CGO off.", Question: "how to build?", Tags: []string{"Build", " build ", "CI"}}, NoteMeta{})
	if err != nil || !strings.HasPrefix(out, "saved ") || strings.Contains(out, "global") {
		t.Fatalf("project add: %q %v", out, err)
	}
	out, err = m.apply(toolArgs{Action: "add", Content: "The user writes fragments, not prose.", Scope: "global"}, NoteMeta{})
	if err != nil || !strings.HasSuffix(out, "(global)") {
		t.Fatalf("global add: %q %v", out, err)
	}
	if _, err := m.apply(toolArgs{Action: "add", Content: "x", Scope: "team"}, NoteMeta{}); err == nil {
		t.Error("an unknown scope was taken")
	}
	if _, err := m.apply(toolArgs{Action: "add", Content: "x", Tags: []string{"a,b"}}, NoteMeta{}); err == nil {
		t.Error("a tag with a comma was taken")
	}
	if len(f.ingests) != 2 {
		t.Fatalf("ingests %+v", f.ingests)
	}
	p, g := f.ingests[0], f.ingests[1]
	if p.Project != "proj" || p.Index != "memories" || filepath.Dir(p.Path) != m.dir || p.Title != "how to build?" ||
		!slices.Equal(p.Tags, []string{"build", "ci", "kind:memory"}) {
		t.Errorf("project ingest %+v", p)
	}
	if g.Project != "" || filepath.Dir(g.Path) != memoryDir(globalMemoryDir) {
		t.Errorf("global ingest %+v", g)
	}
	b, _ := os.ReadFile(p.Path)
	if !strings.Contains(string(b), "tags: build, ci\n") {
		t.Errorf("the note's frontmatter lacks its tags:\n%s", b)
	}
	// refine keeps the tags it is not given, and finds a global note too.
	if _, err := m.apply(toolArgs{Action: "refine", OldText: "CGO off", Content: "Builds need CGO on.", Why: "w", Source: "user"}, NoteMeta{}); err != nil {
		t.Fatal(err)
	}
	if last := f.ingests[len(f.ingests)-1]; !slices.Equal(last.Tags, []string{"build", "ci", "kind:memory"}) {
		t.Errorf("refine dropped the tags: %+v", last)
	}
	if _, err := m.apply(toolArgs{Action: "void", OldText: "fragments", Why: "w", Source: "user"}, NoteMeta{}); err != nil {
		t.Fatal(err)
	}
	if len(f.forgets) != 1 || f.forgets[0].Project != "" {
		t.Errorf("forget %+v, want the global index", f.forgets)
	}
	ls := m.List()
	if len(ls) != 2 || !ls[1].Global || !ls[1].Void || !slices.Equal(ls[0].Tags, []string{"build", "ci"}) {
		t.Errorf("list %+v", ls)
	}
}

// A note found twice (its text and the caption its tags became) is one hit,
// with the best of both measures; a hit outside the notes, or on a file that
// is gone, is dropped.
func TestMemRecall_FindKeepsOneHitPerNote(t *testing.T) {
	f := &fakeRag{mode: "hybrid"}
	m, mf, _ := memWith(t, f)
	_, _ = m.apply(toolArgs{Action: "add", Content: "Deploys go through the zebra cluster.", Question: "where do deploys go?"}, NoteMeta{})
	note := f.ingests[0].Path
	sim := 0.71
	f.hits = map[string][]proto.RagHit{"proj": {
		{Path: "file://" + note, Title: "where do deploys go?", Text: "Deploys go through the zebra cluster.", Score: 0.03, Matched: 1, Terms: 2},
		{Path: "file://" + note, Title: "where do deploys go?", Text: "caption", Score: 0.02, Similarity: &sim},
		{Path: "file:///elsewhere/x.md", Score: 0.5},
		{Path: "file://" + filepath.Join(m.dir, "gone.md"), Score: 0.5},
	}}
	hits, err := mf.Find(context.Background(), []string{"zebra deploys"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].DocID != note || hits[0].Score != 0.03 {
		t.Fatalf("hits %+v, want the one note", hits)
	}
	h, ok := mf.l.hit(note)
	if !ok || !h.Meta.HasSim || h.Meta.Sim != 0.71 || h.Meta.Matched != 1 || h.Meta.Terms != 2 || !h.Meaning || h.Index != "proj/memories" {
		t.Errorf("measures %+v", h)
	}
}

// dund down: the note is written and the result says it is not indexed yet;
// recall searches the files and says once that it is degraded; on the next
// contact the whole set goes to dund, voided notes as gone.
func TestMemRecall_DundDownDegradesAndReconcilesOnReconnect(t *testing.T) {
	f := &fakeRag{down: true}
	m, mf, notices := memWith(t, f)
	out, err := m.apply(toolArgs{Action: "add", Content: "Never stash to tidy: a stash lost untracked work.", Question: "may I git stash?"}, NoteMeta{})
	if err != nil || !strings.Contains(out, "dund is not reachable") {
		t.Fatalf("add with dund down: %q %v", out, err)
	}
	_, _ = m.apply(toolArgs{Action: "add", Content: "The tests need postgres.", Scope: "global"}, NoteMeta{})
	out, err = m.apply(toolArgs{Action: "void", OldText: "postgres", Why: "sqlite now", Source: "user"}, NoteMeta{})
	if err != nil || !strings.Contains(out, "leaves the index when dund is") {
		t.Fatalf("void with dund down: %q %v", out, err)
	}
	hits, err := mf.Find(context.Background(), []string{"should I stash my untracked work before rebasing?"})
	if err != nil || len(hits) != 1 || !strings.Contains(hits[0].DocID, "may-i-git-stash") {
		t.Fatalf("local recall: %+v %v", hits, err)
	}
	if h, _ := mf.l.hit(hits[0].DocID); h.Meaning || h.Meta.Terms == 0 || h.Meta.Matched == 0 || h.Index != localIndex {
		t.Errorf("a local hit is for the coverage rule: %+v", h)
	}
	if hits, _ := mf.Find(context.Background(), []string{"postgres"}); len(hits) != 0 {
		t.Errorf("a voided note was recalled: %+v", hits)
	}
	if len(*notices) != 1 || !strings.Contains((*notices)[0], "plain word search") {
		t.Errorf("notices %q, want one", *notices)
	}

	f.setDown(false)
	if _, err := mf.Find(context.Background(), []string{"stash"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return len(f.syncCalls()) == 2 })
	s := f.syncCalls()
	byProject := map[string]proto.RagSync{s[0].Project: s[0], s[1].Project: s[1]}
	if p := byProject["proj"]; len(p.Files) != 1 || !strings.Contains(p.Files[0].Path, "may-i-git-stash") || p.Files[0].Title != "may I git stash?" {
		t.Errorf("project sync %+v", p)
	}
	if g := byProject[""]; len(g.Files) != 0 || len(g.Gone) != 1 || strings.HasSuffix(g.Gone[0], voidSuffix) {
		t.Errorf("global sync %+v, want the voided note as gone", g)
	}
	// Synced: another contact does not send the set again.
	_, _ = mf.Find(context.Background(), []string{"stash"})
	time.Sleep(50 * time.Millisecond)
	if n := len(f.syncCalls()); n != 2 {
		t.Errorf("%d syncs after a second contact, want 2", n)
	}
	if len(*notices) != 1 {
		t.Errorf("degraded said %d times", len(*notices))
	}
}

func waitFor(t *testing.T, limit time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("condition never held within %s", limit)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// fakeRaglitClient records the layer a document went to and serves forgets;
// down makes every call fail (daemon unreachable).
type fakeRaglitClient struct {
	mu      sync.Mutex
	down    bool
	uploads []struct {
		index, name, text string
	}
	forgets []struct{ index, path string }
}

func (f *fakeRaglitClient) Upload(_ context.Context, index string, doc raglit.UploadDoc) (raglit.UploadResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return raglit.UploadResult{}, raglit.ErrUnavailable
	}
	f.uploads = append(f.uploads, struct {
		index, name, text string
	}{index, doc.Name, doc.Text})
	return raglit.UploadResult{Index: index, Path: "/layers/" + index + "/" + doc.Name}, nil
}

func (f *fakeRaglitClient) Forget(_ context.Context, index, path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return raglit.ErrUnavailable
	}
	f.forgets = append(f.forgets, struct{ index, path string }{index, path})
	return nil
}

// memWithRaglit is memWith plus a raglit registration: the link uploads
// notes to their layer.
func memWithRaglit(t *testing.T, f *fakeRag, rc *fakeRaglitClient) (*Store, raglit.Registration) {
	t.Helper()
	t.Setenv("DUN_HOME", t.TempDir())
	reg := raglit.Registration{ProjectID: "r", Namespace: "ns", Base: "ns__base", Private: "ns__u1-priv", Public: "ns__u1-pub"}
	l := NewLinkWithRaglit(f, "proj", func(s string) {}, rc, reg)
	m := NotesFor(l, t.TempDir())
	m.dir = memoryDir("proj")
	return m, reg
}

// A note goes to the private layer by default, to the public layer when
// public: true; a refine re-uploads the same name to the same layer; a void
// forgets it from that layer and keeps the file; a layer failure leaves the
// note saved locally.
func TestMemRecall_RaglitLayers(t *testing.T) {
	f := &fakeRag{}
	rc := &fakeRaglitClient{}
	m, reg := memWithRaglit(t, f, rc)
	if _, err := m.apply(toolArgs{Action: "add", Content: "Builds need CGO off.", Question: "how to build?"}, NoteMeta{}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.apply(toolArgs{Action: "add", Content: "The deploy uses the canary lane.", Public: true}, NoteMeta{}); err != nil {
		t.Fatal(err)
	}
	rc.mu.Lock()
	if len(rc.uploads) != 2 || rc.uploads[0].index != reg.Private || rc.uploads[1].index != reg.Public {
		t.Fatalf("uploads %+v, want private then public", rc.uploads)
	}
	p0 := rc.uploads[0].name // the document name: the note's file name
	rc.mu.Unlock()
	// The daemon's path is in the note's frontmatter, so the void forgets
	// the right document.
	pth := ""
	if ps, _ := filepath.Glob(filepath.Join(m.dir, "*.md")); len(ps) == 2 {
		for _, p := range ps {
			b, _ := os.ReadFile(p)
			if strings.Contains(string(b), "how to build?") {
				pth = p
			}
		}
	}
	if n, _ := readNote(pth); n.meta["raglit_path"] != "/layers/"+reg.Private+"/"+p0 {
		t.Errorf("raglit_path in frontmatter = %q, want the upload's path", n.meta["raglit_path"])
	}
	// A refine re-uploads under the same name, to the same layer.
	if _, err := m.apply(toolArgs{Action: "refine", OldText: "CGO off", Content: "Builds need CGO on.", Why: "w", Source: "user"}, NoteMeta{}); err != nil {
		t.Fatal(err)
	}
	rc.mu.Lock()
	if len(rc.uploads) != 3 || rc.uploads[2].index != reg.Private || rc.uploads[2].name != p0 {
		t.Fatalf("refine uploads %+v, want the same name to the private layer", rc.uploads)
	}
	rc.mu.Unlock()
	// A void forgets the note from its layer and keeps the file.
	if _, err := m.apply(toolArgs{Action: "void", OldText: "canary", Why: "w", Source: "user"}, NoteMeta{}); err != nil {
		t.Fatal(err)
	}
	rc.mu.Lock()
	if len(rc.forgets) != 1 || rc.forgets[0].index != reg.Public || !strings.HasPrefix(rc.forgets[0].path, "/layers/"+reg.Public+"/") {
		t.Fatalf("forgets %+v, want the public layer's path", rc.forgets)
	}
	rc.mu.Unlock()
	if vs, _ := filepath.Glob(filepath.Join(m.dir, "*.md"+voidSuffix)); len(vs) != 1 {
		t.Errorf("voided file kept: %v", vs)
	}
	// A layer failure is a down-note, never a hard failure: the note is
	// saved locally and the dund ingest proceeds.
	rc.mu.Lock()
	rc.down = true
	rc.mu.Unlock()
	out, err := m.apply(toolArgs{Action: "add", Content: "The queue drains on restart.", Question: "drain?"}, NoteMeta{})
	if err != nil || !strings.HasPrefix(out, "saved ") {
		t.Fatalf("a layer failure failed the write: %q %v", out, err)
	}
}
