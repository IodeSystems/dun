package harness

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/iodesystems/dun/internal/mcpserver"
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
	tags    []proto.RagTag
	hits    map[string][]proto.RagHit // by project ("" global): the memories and strategies indexes
	mode    string
	// code and docs (codesearch.go): the hits of those indexes by index
	// name, the rag_index calls and the searches of them.
	code     map[string][]proto.RagHit
	indexes  []proto.RagIndex
	searches []proto.RagSearch
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
	if q.Index == sessionsIndex {
		f.searches = append(f.searches, q)
		return &proto.RagHits{Mode: mode, Hits: f.sessionHits(q)}, nil
	}
	if q.Index == "code" || q.Index == "docs" {
		f.searches = append(f.searches, q)
		var out []proto.RagHit
		for _, h := range f.code[q.Index] {
			if strings.HasPrefix(h.Path, q.Filter.PathPrefix) {
				out = append(out, h)
			}
		}
		return &proto.RagHits{Mode: mode, Hits: out}, nil
	}
	return &proto.RagHits{Mode: mode, Hits: f.hits[q.Project]}, nil
}

// sessionHits answers a sessions search from the files synced into it:
// those that carry every tag of the filter and hold the query.
func (f *fakeRag) sessionHits(q proto.RagSearch) []proto.RagHit {
	var out []proto.RagHit
	seen := map[string]bool{}
	for i := len(f.syncs) - 1; i >= 0; i-- {
		for _, sf := range f.syncs[i].Files {
			if seen[sf.Path] || slices.ContainsFunc(q.Filter.Tags, func(t string) bool { return !slices.Contains(sf.Tags, t) }) {
				continue
			}
			seen[sf.Path] = true
			if b, err := os.ReadFile(sf.Path); err == nil && strings.Contains(string(b), q.Query) {
				out = append(out, proto.RagHit{Path: "file://" + sf.Path, Title: sf.Title, Text: string(b), Score: 1})
			}
		}
	}
	return out
}

func (f *fakeRag) Index(_ context.Context, q proto.RagIndex) (*proto.RagIndexed, error) {
	if f.isDown() {
		return nil, ErrRagDown
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.indexes = append(f.indexes, q)
	return &proto.RagIndexed{Project: q.Project}, nil
}

func (f *fakeRag) indexCalls() []proto.RagIndex {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.indexes)
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

func (f *fakeRag) Tag(_ context.Context, q proto.RagTag) (*proto.RagTagged, error) {
	if f.isDown() {
		return nil, ErrRagDown
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

// Project and global notes are separate groups (USER 2026-11-08): each is
// ranked in its own index and passes the coverage rule on its own, and a
// pass that surfaces both puts the project's first.
func TestMemRecall_ProjectAndGlobalAreSeparateGroups(t *testing.T) {
	t.Setenv("DUN_HOME", t.TempDir())
	ws := t.TempDir()
	rag := &fakeRag{}
	h, err := Start(context.Background(), Config{Workspace: ws, Rag: rag})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	m := h.memoryStore()
	note := func(dir, name, text string) string {
		p := filepath.Join(dir, name)
		writeRule(t, p, text)
		return p
	}
	hit := func(p string, score float64) proto.RagHit {
		return proto.RagHit{Path: "file://" + p, Title: filepath.Base(p), Text: "x", Score: score, Matched: 2, Terms: 2}
	}
	setHits := func(project, global []proto.RagHit) {
		rag.mu.Lock()
		rag.hits = map[string][]proto.RagHit{mcpserver.RaglitProject(ws): project, "": global}
		rag.mu.Unlock()
	}

	// Global's bm25 score is far higher, and in one group it would win alone;
	// apart, both clear the rule and the project note comes first.
	proj := note(m.Dir(), "p1.md", "Zebra deploys in this repo need --frostbite.")
	glob := note(m.GlobalDir(), "g1.md", "The user deploys zebra on Fridays only.")
	setHits([]proto.RagHit{hit(proj, 1)}, []proto.RagHit{hit(glob, 9)})
	aside := surfaceOnce(t, h, "zebra deploys")
	pi, gi := strings.Index(aside, "--frostbite"), strings.Index(aside, "Fridays only")
	if pi < 0 || gi < 0 || pi > gi {
		t.Fatalf("want the project note, then the global one:\n%s", aside)
	}

	// The rule per group: the project's two notes are too close (under 1.5x),
	// so only the global note surfaces.
	p2 := note(m.Dir(), "p2.md", "Yak builds need CGO off.")
	p3 := note(m.Dir(), "p3.md", "Yak builds run on Mondays.")
	g2 := note(m.GlobalDir(), "g2.md", "The user wants yak builds small.")
	setHits([]proto.RagHit{hit(p2, 3), hit(p3, 2.5)}, []proto.RagHit{hit(g2, 0.1)})
	aside = surfaceOnce(t, h, "yak builds")
	if !strings.Contains(aside, "yak builds small") || strings.Contains(aside, "CGO off") || strings.Contains(aside, "Mondays") {
		t.Fatalf("want only the global note:\n%s", aside)
	}
}
