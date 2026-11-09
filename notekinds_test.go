package dun

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/iodesystems/dun/proto"
)

// itemHarness is a session with a RagLink over f, in a temp DUN_HOME.
func itemHarness(t *testing.T, f *fakeRag) (*Harness, string) {
	t.Helper()
	t.Setenv("DUN_HOME", t.TempDir())
	ws := t.TempDir()
	cfg := Config{Workspace: ws}
	if f != nil {
		cfg.Rag = f
	}
	h, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return h, ws
}

func ingestsOf(f *fakeRag) []proto.RagIngest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.ingests)
}

// A strategy and a constraint go to the strategies index of their scope,
// in their own directories; a memory note stays in memories.
func TestStrategies_ScopeAndIndexAreSeparate(t *testing.T) {
	f := &fakeRag{}
	h, ws := itemHarness(t, f)
	out := callTool(t, h, "memory", map[string]any{"action": "add", "kind": "strategy", "name": "Bisect flaky tests",
		"content": "Run the flaky test alone with -count=20 before changing it."})
	if !strings.Contains(out, "(strategy:bisect-flaky-tests)") {
		t.Fatalf("strategy add: %q", out)
	}
	out = callTool(t, h, "memory", map[string]any{"action": "add", "kind": "constraint", "name": "no push unless asked", "scope": "global"})
	if !strings.Contains(out, "(constraint:no-push-unless-asked)") || !strings.Contains(out, "(global)") {
		t.Fatalf("constraint add: %q", out)
	}
	callTool(t, h, "memory", map[string]any{"action": "add", "question": "how to build?", "content": "Builds need CGO off."})
	if out := callTool(t, h, "memory", map[string]any{"action": "add", "kind": "strategy", "name": "bisect flaky tests"}); !strings.Contains(out, "exists") {
		t.Errorf("a second strategy of one name: %q", out)
	}
	if out := callTool(t, h, "memory", map[string]any{"action": "add", "kind": "constraint", "content": "x"}); !strings.Contains(out, "name is required") {
		t.Errorf("a constraint with no name: %q", out)
	}
	in := ingestsOf(f)
	if len(in) != 3 {
		t.Fatalf("ingests %+v", in)
	}
	proj := raglitProject(ws)
	s, c, m := in[0], in[1], in[2]
	if s.Index != "strategies" || s.Project != proj || filepath.Dir(s.Path) != strategiesDir(proj) || s.Title != "strategy:bisect-flaky-tests" {
		t.Errorf("strategy ingest %+v", s)
	}
	if c.Index != "strategies" || c.Project != "" || filepath.Dir(c.Path) != strategiesDir(globalMemoryDir) {
		t.Errorf("constraint ingest %+v", c)
	}
	if m.Index != "memories" || m.Project != proj || filepath.Dir(m.Path) != memoryDir(proj) {
		t.Errorf("memory ingest %+v", m)
	}
	b, _ := os.ReadFile(c.Path)
	if !strings.Contains(string(b), "name: no-push-unless-asked\nkind: constraint\n") {
		t.Errorf("constraint file:\n%s", b)
	}
	// Refine and void by path.
	out = callTool(t, h, "memory", map[string]any{"action": "refine", "old_text": "strategy:bisect-flaky-tests",
		"content": "Run it alone with -count=50.", "why": "the user said 50", "source": "user"})
	if !strings.HasPrefix(out, "refined ") {
		t.Fatalf("refine by path: %q", out)
	}
	out = callTool(t, h, "memory", map[string]any{"action": "void", "old_text": "constraint:no-push-unless-asked", "why": "the user lifted it", "source": "user"})
	if !strings.HasPrefix(out, "voided ") {
		t.Fatalf("void by path: %q", out)
	}
}

func systemOf(h *Harness) string {
	h.turnMu.Lock()
	defer h.turnMu.Unlock()
	return h.Session.System
}

// An important item is in the system prompt by path, every session, with
// no retrieval; one that is not is not. The list holds importantCap across
// the kinds, and a listed one is not surfaced again.
func TestStrategies_ImportantAlwaysShown(t *testing.T) {
	f := &fakeRag{}
	h, ws := itemHarness(t, f)
	for _, a := range []string{
		"add constraint no-push Do not push unless the user asks.",
		"add strategy global bisect-flaky Run the test alone with -count=20.",
		"add memory build-cgo Builds need CGO off.",
		"add strategy plain-one Something nobody marked.",
	} {
		if _, err := h.MemoriesCommand(a); err != nil {
			t.Fatalf("%s: %v", a, err)
		}
	}
	if strings.Contains(systemOf(h), "Standing") {
		t.Fatal("a standing list with nothing important")
	}
	for _, p := range []string{"constraint:no-push", "strategy:bisect-flaky", "memory:build-cgo"} {
		if out, err := h.MemoriesCommand("important " + p); err != nil || !strings.Contains(out, "standing list") {
			t.Fatalf("important %s: %q %v", p, out, err)
		}
	}
	sys := systemOf(h)
	ci, si, mi := strings.Index(sys, "- constraint:no-push (this project)"), strings.Index(sys, "- strategy:bisect-flaky (every project)"),
		strings.Index(sys, "- memory:build-cgo (this project)")
	if ci < 0 || si < ci || mi < si || strings.Contains(sys, "plain-one") || strings.Contains(sys, "Do not push") {
		t.Fatalf("standing list (constraints, strategies, memories; names only):\n%s", sys)
	}
	// A new session on the project shows it too.
	h2, err := Start(context.Background(), Config{Workspace: ws, Rag: f})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h2.Close)
	if !strings.Contains(systemOf(h2), "- constraint:no-push (this project)") {
		t.Error("the next session has no standing list")
	}
	// Listed: not surfaced by retrieval as well.
	path := h.strategyStore().named(kindConstraint, "no-push")
	f.mu.Lock()
	f.hits = map[string][]proto.RagHit{raglitProject(ws): {{Path: "file://" + path, Title: "constraint:no-push", Text: "x", Score: 1, Matched: 2, Terms: 2}}}
	f.mu.Unlock()
	if aside := surfaceOnce(t, h, "push unless"); strings.Contains(aside, "no-push") {
		t.Errorf("a listed item surfaced again:\n%s", aside)
	}
	if _, err := h.MemoriesCommand("unimportant constraint:no-push"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(systemOf(h), "constraint:no-push") {
		t.Error("unimportant, still listed")
	}
	// The cap: across the kinds, the rest named as a count.
	for i := range importantCap + 1 {
		if _, err := h.MemoriesCommand(fmt.Sprintf("add constraint c%02d", i)); err != nil {
			t.Fatal(err)
		}
		if _, err := h.MemoriesCommand(fmt.Sprintf("important constraint:c%02d", i)); err != nil {
			t.Fatal(err)
		}
	}
	sys = systemOf(h)
	if n := strings.Count(sys, "\n- constraint:") + strings.Count(sys, "\n- strategy:") + strings.Count(sys, "\n- memory:"); n != importantCap || !strings.Contains(sys, "3 more marked important are not listed") {
		t.Errorf("%d listed, want %d and a count of the rest:\n%s", n, importantCap, sys)
	}
}

// A strategy that is not important surfaces by retrieval, in its own
// group, project first.
func TestStrategies_NonImportantIsRetrieved(t *testing.T) {
	f := &fakeRag{}
	h, ws := itemHarness(t, f)
	if _, err := h.MemoriesCommand("add strategy bisect-flaky Run the flaky test alone with -count=20 first."); err != nil {
		t.Fatal(err)
	}
	if _, err := h.MemoriesCommand("add constraint global frozen-api The public API is frozen until the release."); err != nil {
		t.Fatal(err)
	}
	s := h.strategyStore()
	sp, cp := s.named(kindStrategy, "bisect-flaky"), s.named(kindConstraint, "frozen-api")
	f.mu.Lock()
	f.hits = map[string][]proto.RagHit{
		raglitProject(ws): {{Path: "file://" + sp, Title: "strategy:bisect-flaky", Text: "x", Score: 1, Matched: 2, Terms: 2}},
		"":                {{Path: "file://" + cp, Title: "constraint:frozen-api", Text: "x", Score: 9, Matched: 2, Terms: 2}},
	}
	f.mu.Unlock()
	aside := surfaceOnce(t, h, "flaky test")
	si, ci := strings.Index(aside, "-count=20"), strings.Index(aside, "frozen until")
	if si < 0 || ci < si || !strings.Contains(aside, "A strategy or constraint saved") {
		t.Fatalf("want the project strategy, then the global constraint:\n%s", aside)
	}
}

// file_read takes memory:, strategy: and constraint: paths; any other path
// goes to the files server.
func TestStrategies_FileReadTakesItemPaths(t *testing.T) {
	f := &fakeRag{}
	h, ws := itemHarness(t, f)
	if _, err := h.MemoriesCommand("add strategy bisect-flaky Run the flaky test alone with -count=20."); err != nil {
		t.Fatal(err)
	}
	callTool(t, h, "memory", map[string]any{"action": "add", "name": "cgo", "question": "how to build?", "content": "Builds need CGO off."})
	if out := callTool(t, h, "file_read", map[string]any{"path": "strategy:bisect-flaky"}); !strings.Contains(out, "-count=20") {
		t.Errorf("strategy read: %q", out)
	}
	if out := callTool(t, h, "file_read", map[string]any{"path": "memory:cgo"}); !strings.Contains(out, "CGO off") {
		t.Errorf("memory read: %q", out)
	}
	if out := callTool(t, h, "file_read", map[string]any{"path": "strategy:nope"}); !strings.HasPrefix(out, "ERROR") || !strings.Contains(out, "strategy:bisect-flaky") {
		t.Errorf("unknown name: %q", out)
	}
	if out := callTool(t, h, "file_read", map[string]any{"path": "constraint:nope"}); !strings.Contains(out, "there are none") {
		t.Errorf("unknown constraint: %q", out)
	}
	writeRule(t, filepath.Join(ws, "a.txt"), "plain file\n")
	if out := callTool(t, h, "file_read", map[string]any{"path": "a.txt"}); !strings.Contains(out, "plain file") {
		t.Errorf("a file: %q", out)
	}
}

// dund down: a strategy is written and said not indexed; recall reads the
// files.
func TestStrategies_DundDownDegrades(t *testing.T) {
	f := &fakeRag{down: true}
	h, _ := itemHarness(t, f)
	out := callTool(t, h, "memory", map[string]any{"action": "add", "kind": "strategy", "name": "warm cache",
		"content": "The narwhal job needs a warm cache before timing."})
	if !strings.Contains(out, "dund is not reachable") {
		t.Fatalf("add with dund down: %q", out)
	}
	mf := &memFinder{l: h.stratLink(), m: h.strategyStore()}
	hits, err := mf.Find(context.Background(), []string{"timing the narwhal job"})
	if err != nil || len(hits) != 1 || hits[0].Title != "strategy:warm-cache" {
		t.Fatalf("local recall: %+v %v", hits, err)
	}
}

// Off (DUN_MEMORY=0 is no RagLink, as under the Harbor adapter): no tool,
// no store, no standing list even with an important file on disk.
func TestStrategies_OffWithoutARagLink(t *testing.T) {
	h, ws := itemHarness(t, nil)
	dir := strategiesDir(raglitProject(ws))
	writeRule(t, filepath.Join(dir, "constraint-x.md"), "---\nname: x\nkind: constraint\nimportant: true\n---\nX.\n")
	h2, err := Start(context.Background(), Config{Workspace: ws})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h2.Close)
	for _, s := range []*Harness{h, h2} {
		if s.strategyStore() != nil || slices.Contains(modelToolNames(s), "memory") || strings.Contains(systemOf(s), "constraint:x") {
			t.Error("strategies with no RagLink")
		}
	}
	if _, err := h2.MemoriesCommand("add strategy y"); err == nil {
		t.Error("/memories add with no RagLink")
	}
}

// The user's /memories add of an existing name replaces it: the old detail
// is in the log, the file is indexed again, important stays; the model's
// add of the same name is refused.
func TestStrategies_UserAddReplaces(t *testing.T) {
	f := &fakeRag{}
	h, _ := itemHarness(t, f)
	if _, err := h.MemoriesCommand("add constraint no-push Do not push."); err != nil {
		t.Fatal(err)
	}
	if _, err := h.MemoriesCommand("important constraint:no-push"); err != nil {
		t.Fatal(err)
	}
	n := len(ingestsOf(f))
	out, err := h.MemoriesCommand("add constraint no-push Do not push unless the user asks.")
	if err != nil || !strings.HasPrefix(out, "replaced constraint:no-push") {
		t.Fatalf("replace: %q %v", out, err)
	}
	p := h.strategyStore().named(kindConstraint, "no-push")
	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), "unless the user asks") || !strings.Contains(string(b), "important: true") {
		t.Errorf("replaced file:\n%s", b)
	}
	in := ingestsOf(f)
	if len(in) != n+1 || in[len(in)-1].Path != p {
		t.Errorf("not indexed again: %+v", in)
	}
	log := readAttest(p)
	if len(log) != 1 || log[0].Kind != "corrected" || log[0].Was != "Do not push." || log[0].By != "user" {
		t.Errorf("log %+v", log)
	}
	// Another scope is another item.
	if out, err := h.MemoriesCommand("add constraint global no-push Never push."); err != nil || strings.HasPrefix(out, "replaced") {
		t.Errorf("global add: %q %v", out, err)
	}
	if out := callTool(t, h, "memory", map[string]any{"action": "add", "kind": "constraint", "name": "no-push"}); !strings.Contains(out, "exists") {
		t.Errorf("the model's add of an existing name: %q", out)
	}
}

// Marking an item important, or a refine of its tags alone, sends its tags
// (rag_tag) and no ingest: the text the index holds is unchanged.
func TestStrategies_ImportantAndRetagSendTagsNotAnIngest(t *testing.T) {
	f := &fakeRag{}
	h, _ := itemHarness(t, f)
	if _, err := h.MemoriesCommand("add strategy pelican-drill Run the pelican drill before a release."); err != nil {
		t.Fatal(err)
	}
	n := len(ingestsOf(f))
	if n == 0 || !slices.Equal(ingestsOf(f)[n-1].Tags, []string{"kind:strategy"}) {
		t.Fatalf("add: ingests %+v, want one tagged kind:strategy", ingestsOf(f))
	}
	tagsOf := func() []proto.RagTag {
		f.mu.Lock()
		defer f.mu.Unlock()
		return slices.Clone(f.tags)
	}
	if _, err := h.MemoriesCommand("important strategy:pelican-drill"); err != nil {
		t.Fatal(err)
	}
	if got := len(ingestsOf(f)); got != n {
		t.Fatalf("important: %d ingests, want %d (none)", got, n)
	}
	tg := tagsOf()
	if len(tg) != 1 || tg[0].Index != strategiesIndex || !slices.Equal(tg[0].Tags, []string{"kind:strategy", "important"}) {
		t.Fatalf("important: tag calls %+v", tg)
	}
	if _, err := h.MemoriesCommand("unimportant strategy:pelican-drill"); err != nil {
		t.Fatal(err)
	}
	if tg = tagsOf(); len(tg) != 2 || !slices.Equal(tg[1].Tags, []string{"kind:strategy"}) || len(ingestsOf(f)) != n {
		t.Fatalf("unimportant: tag calls %+v, ingests %d", tg, len(ingestsOf(f)))
	}

	// The memory tool's refine: the same text with new tags is a retag;
	// new text is an ingest.
	m := h.memoryStore()
	if _, err := m.apply(memoryArgs{Action: "add", Content: "Builds need CGO off.", Question: "how to build?"}, noteMeta{}); err != nil {
		t.Fatal(err)
	}
	n = len(ingestsOf(f))
	if _, err := m.apply(memoryArgs{Action: "refine", OldText: "CGO off", Content: "Builds need CGO off.", Tags: []string{"build"}, Why: "w", Source: "user"}, noteMeta{}); err != nil {
		t.Fatal(err)
	}
	if tg = tagsOf(); len(ingestsOf(f)) != n || len(tg) != 3 || !slices.Equal(tg[2].Tags, []string{"build", "kind:memory"}) {
		t.Fatalf("retag: tag calls %+v, ingests %d want %d", tg, len(ingestsOf(f)), n)
	}
	if _, err := m.apply(memoryArgs{Action: "refine", OldText: "CGO off", Content: "Builds need CGO on.", Why: "w", Source: "user"}, noteMeta{}); err != nil {
		t.Fatal(err)
	}
	if len(ingestsOf(f)) != n+1 || len(tagsOf()) != 3 {
		t.Fatalf("refine of the text: ingests %d want %d, tag calls %d", len(ingestsOf(f)), n+1, len(tagsOf()))
	}
}
