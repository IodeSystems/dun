package dun

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iodesystems/dun/proto"
)

// fakeRag is a RagLink that records calls and answers searches from hits;
// down makes every call fail as unreachable.
type fakeRag struct {
	mu      sync.Mutex
	down    bool
	ingests []proto.RagIngest
	forgets []proto.RagForget
	syncs   []proto.RagSync
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
		return nil, ErrRagDown
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
		return nil, ErrRagDown
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ingests = append(f.ingests, q)
	return &proto.RagIngested{Job: int64(len(f.ingests)), URL: "file://" + q.Path}, nil
}

func (f *fakeRag) Forget(_ context.Context, q proto.RagForget) (*proto.RagForgotten, error) {
	if f.isDown() {
		return nil, ErrRagDown
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forgets = append(f.forgets, q)
	return &proto.RagForgotten{Removed: true}, nil
}

func (f *fakeRag) Sync(_ context.Context, q proto.RagSync) (*proto.RagSynced, error) {
	if f.isDown() {
		return nil, ErrRagDown
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.syncs = append(f.syncs, q)
	return &proto.RagSynced{Queued: len(q.Files), Removed: len(q.Gone)}, nil
}

func (f *fakeRag) syncCalls() []proto.RagSync {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.syncs)
}

// memWith is a store and finder over f, in a temp DUN_HOME, with the
// notices it gave.
func memWith(t *testing.T, f *fakeRag) (*memoryStore, *memFinder, *[]string) {
	t.Helper()
	t.Setenv("DUN_HOME", t.TempDir())
	var mu sync.Mutex
	var notices []string
	l := newMemLink(f, "proj", func(s string) { mu.Lock(); notices = append(notices, s); mu.Unlock() })
	m := memoryFor(l, t.TempDir())
	m.dir = memoryDir("proj")
	return m, &memFinder{l: l, m: m}, &notices
}

// A note goes to the memories index of its scope, with its tags; global
// notes live in their own directory.
func TestMemRecall_AddIngestsIntoTheScopesIndexWithTags(t *testing.T) {
	f := &fakeRag{}
	m, _, _ := memWith(t, f)
	out, err := m.apply(memoryArgs{Action: "add", Content: "Builds need CGO off.", Question: "how to build?", Tags: []string{"Build", " build ", "CI"}}, noteMeta{})
	if err != nil || !strings.HasPrefix(out, "saved ") || strings.Contains(out, "global") {
		t.Fatalf("project add: %q %v", out, err)
	}
	out, err = m.apply(memoryArgs{Action: "add", Content: "The user writes fragments, not prose.", Scope: "global"}, noteMeta{})
	if err != nil || !strings.HasSuffix(out, "(global)") {
		t.Fatalf("global add: %q %v", out, err)
	}
	if _, err := m.apply(memoryArgs{Action: "add", Content: "x", Scope: "team"}, noteMeta{}); err == nil {
		t.Error("an unknown scope was taken")
	}
	if _, err := m.apply(memoryArgs{Action: "add", Content: "x", Tags: []string{"a,b"}}, noteMeta{}); err == nil {
		t.Error("a tag with a comma was taken")
	}
	if len(f.ingests) != 2 {
		t.Fatalf("ingests %+v", f.ingests)
	}
	p, g := f.ingests[0], f.ingests[1]
	if p.Project != "proj" || p.Index != "memories" || filepath.Dir(p.Path) != m.dir || p.Title != "how to build?" ||
		!slices.Equal(p.Tags, []string{"build", "ci"}) {
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
	if _, err := m.apply(memoryArgs{Action: "refine", OldText: "CGO off", Content: "Builds need CGO on.", Why: "w", Source: "user"}, noteMeta{}); err != nil {
		t.Fatal(err)
	}
	if last := f.ingests[len(f.ingests)-1]; !slices.Equal(last.Tags, []string{"build", "ci"}) {
		t.Errorf("refine dropped the tags: %+v", last)
	}
	if _, err := m.apply(memoryArgs{Action: "void", OldText: "fragments", Why: "w", Source: "user"}, noteMeta{}); err != nil {
		t.Fatal(err)
	}
	if len(f.forgets) != 1 || f.forgets[0].Project != "" {
		t.Errorf("forget %+v, want the global index", f.forgets)
	}
	ls := m.list()
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
	_, _ = m.apply(memoryArgs{Action: "add", Content: "Deploys go through the zebra cluster.", Question: "where do deploys go?"}, noteMeta{})
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
	if !ok || !h.meta.HasSim || h.meta.Sim != 0.71 || h.meta.Matched != 1 || h.meta.Terms != 2 || !h.meaning || h.index != "proj/memories" {
		t.Errorf("measures %+v", h)
	}
}

// dund down: the note is written and the result says it is not indexed yet;
// recall searches the files and says once that it is degraded; on the next
// contact the whole set goes to dund, voided notes as gone.
func TestMemRecall_DundDownDegradesAndReconcilesOnReconnect(t *testing.T) {
	f := &fakeRag{down: true}
	m, mf, notices := memWith(t, f)
	out, err := m.apply(memoryArgs{Action: "add", Content: "Never stash to tidy: a stash lost untracked work.", Question: "may I git stash?"}, noteMeta{})
	if err != nil || !strings.Contains(out, "dund is not reachable") {
		t.Fatalf("add with dund down: %q %v", out, err)
	}
	_, _ = m.apply(memoryArgs{Action: "add", Content: "The tests need postgres.", Scope: "global"}, noteMeta{})
	out, err = m.apply(memoryArgs{Action: "void", OldText: "postgres", Why: "sqlite now", Source: "user"}, noteMeta{})
	if err != nil || !strings.Contains(out, "leaves the index when dund is") {
		t.Fatalf("void with dund down: %q %v", out, err)
	}
	hits, err := mf.Find(context.Background(), []string{"should I stash my untracked work before rebasing?"})
	if err != nil || len(hits) != 1 || !strings.Contains(hits[0].DocID, "may-i-git-stash") {
		t.Fatalf("local recall: %+v %v", hits, err)
	}
	if h, _ := mf.l.hit(hits[0].DocID); h.meaning || h.meta.Terms == 0 || h.meta.Matched == 0 || h.index != localIndex {
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

// The memory tool is there with a RagLink and no docs server, and goes with
// DUN_MEMORY's nil link.
func TestMemRecall_ToolNeedsOnlyARagLink(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws := t.TempDir()
	h, err := Start(context.Background(), Config{Workspace: ws, Rag: &fakeRag{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	if !slices.Contains(modelToolNames(h), "memory") || h.docsPrep == nil {
		t.Fatalf("tools %v, preparer %v: want memory and a proactive search", modelToolNames(h), h.docsPrep != nil)
	}
	h2, err := Start(context.Background(), Config{Workspace: ws})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h2.Close)
	if slices.Contains(modelToolNames(h2), "memory") || h2.memoryStore() != nil {
		t.Error("memory with no RagLink")
	}
}
