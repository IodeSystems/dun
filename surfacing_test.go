package dun

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iodesystems/agentkit/agent"
)

type fixedFinder []agent.DocHit

func (f fixedFinder) Find(context.Context, []string) ([]agent.DocHit, error) { return f, nil }

// A pass that surfaces records the entry's size; a pass that finds only
// already-seen docs still counts (it held the turn for its round trip) but
// adds no bytes.
func TestDocsPreparer_RecordsSurfacing(t *testing.T) {
	store, _ := openSessionStore("")
	ctx := context.Background()
	stats := &surfacingStats{}
	var notes []DocsNote
	finder := fixedFinder{{DocID: "d1", Title: "Why X was dropped", Score: 1, Line: "the measurement"}}
	estimate := func(s string) int { return len(s) / 4 }
	prep := docsPreparer(store, finder, agent.FinderOpts{}, stats, estimate, nil, nil, nil, nil,
		func(n DocsNote) { notes = append(notes, n) })

	store.Append(ctx, "s", agent.Entry{Kind: agent.KindUser, Content: "why was X dropped?", CreatedAt: 1})
	if err := prep.PrepareNotifications(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	if len(notes) != 1 || notes[0].Bytes == 0 || notes[0].Tokens != notes[0].Bytes/4 {
		t.Fatalf("note should carry the entry's size: %+v", notes)
	}
	if stats.Passes != 1 || stats.Entries != 1 || stats.Bytes != notes[0].Bytes || stats.Tokens != notes[0].Tokens {
		t.Fatalf("stats after one surfacing pass: %+v", stats)
	}

	// Same doc again: deduped, nothing appended, but the pass is counted.
	store.Append(ctx, "s", agent.Entry{Kind: agent.KindUser, Content: "and X again", CreatedAt: 2})
	if err := prep.PrepareNotifications(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	if stats.Passes != 2 || stats.Entries != 1 || stats.Bytes != notes[0].Bytes {
		t.Fatalf("a deduped pass counts as a pass and adds no bytes: %+v", stats)
	}
}

// The carried cost is the in-history docs tokens summed per round, and a fold
// that removes the entry stops it from accruing.
func TestSurfacingRound_CarriedStopsAfterFold(t *testing.T) {
	store, _ := openSessionStore("")
	ctx := context.Background()
	h := &Harness{store: store, surfacing: &surfacingStats{}}

	// Nothing surfaced yet: rounds are not counted, so a session without RAG
	// carries no section at all.
	h.noteSurfacingRound()
	if h.Surfacing() != nil {
		t.Fatalf("no pass yet must mean no surfacing section: %v", h.Surfacing())
	}

	docs := agent.Entry{ID: "n1", Kind: agent.KindNotification, Tag: tagDocs, Content: "0123456789abcdef"}
	store.Append(ctx, "s", docs)
	h.surfacing.notePass(0, 1, 1, len(docs.Content), 4)
	per := h.meter.Estimate(docs.Content)

	h.noteSurfacingRound()
	h.noteSurfacingRound()
	if got := h.surfacing.Carried; got != 2*per {
		t.Fatalf("two rounds re-send the entry twice: carried=%d want %d", got, 2*per)
	}

	store.Compact(ctx, "s", agent.Compaction{Subsumes: []agent.Entry{docs},
		Marker: agent.Entry{ID: "m", Kind: agent.KindNotification, Content: "summary"}})
	h.noteSurfacingRound()
	if h.surfacing.Carried != 2*per || h.surfacing.InHist != 0 || h.surfacing.InHistN != 0 {
		t.Fatalf("a folded entry must stop accruing: %+v", h.surfacing)
	}
	if h.surfacing.Rounds != 3 {
		t.Fatalf("rounds: %d", h.surfacing.Rounds)
	}
}

// Two fragments of one document count once: the document's best fragment
// competes, and the runner-up is the next DOCUMENT.
func TestDocsPreparer_OneSlotPerDocument(t *testing.T) {
	store, _ := openSessionStore("")
	ctx := context.Background()
	var notes []DocsNote
	finder := fixedFinder{
		{DocID: "a", Title: "a (caption)", Score: 9},
		{DocID: "a", Title: "a (text)", Score: 8.9}, // would fail the margin if it counted
		{DocID: "b", Title: "b", Score: 3},
	}
	prep := docsPreparer(store, finder, agent.FinderOpts{}, nil, nil, nil, nil, nil, nil,
		func(n DocsNote) { notes = append(notes, n) })
	store.Append(ctx, "s", agent.Entry{Kind: agent.KindUser, Content: "q", CreatedAt: 1})
	if err := prep.PrepareNotifications(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	if len(notes) != 1 || len(notes[0].Docs) != 1 || notes[0].Docs[0].Title != "a (caption)" || notes[0].Found != 2 {
		t.Fatalf("want a's best fragment, found=2 documents: %+v", notes)
	}
}

// The top document surfaces only when it beats the runner-up by the margin;
// an unclear pass surfaces nothing and marks nothing seen, so a later, clearer
// pass can still surface it.
func TestDocsPreparer_MarginOverRunnerUp(t *testing.T) {
	store, _ := openSessionStore("")
	ctx := context.Background()
	stats := &surfacingStats{}
	var notes []DocsNote
	var finder fixedFinder = fixedFinder{{DocID: "a", Score: 4}, {DocID: "b", Score: 3}} // 4 < 1.5×3
	f := &finder
	prep := docsPreparer(store, finderFunc(func() []agent.DocHit { return *f }), agent.FinderOpts{}, stats, nil, nil, nil, nil, nil,
		func(n DocsNote) { notes = append(notes, n) })
	store.Append(ctx, "s", agent.Entry{Kind: agent.KindUser, Content: "q1", CreatedAt: 1})
	prep.PrepareNotifications(ctx, "s")
	if len(notes) != 0 || stats.Passes != 1 || stats.Found != 2 || stats.Surfaced != 0 {
		t.Fatalf("an unclear pass surfaces nothing: notes=%v stats=%+v", notes, stats)
	}
	*f = fixedFinder{{DocID: "a", Score: 9}, {DocID: "b", Score: 3}}
	store.Append(ctx, "s", agent.Entry{Kind: agent.KindUser, Content: "q2", CreatedAt: 2})
	prep.PrepareNotifications(ctx, "s")
	if len(notes) != 1 || notes[0].Docs[0].DocID != "a" {
		t.Fatalf("a clear pass surfaces the top document (not seen before): %+v", notes)
	}
}

type finderFunc func() []agent.DocHit

func (f finderFunc) Find(context.Context, []string) ([]agent.DocHit, error) { return f(), nil }

// A short document goes in whole; a long one (fetch says no) stays a pointer.
func TestDocsPreparer_InlinesShortBodies(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fetch  docBodyFunc
		inline bool
		want   string
	}{
		{"short", func(context.Context, string, int) (string, bool) { return "the whole note", true }, true, "the whole note"},
		{"long", func(context.Context, string, int) (string, bool) { return "", false }, false, "Call the search tool"},
	} {
		store, _ := openSessionStore("")
		ctx := context.Background()
		var notes []DocsNote
		prep := docsPreparer(store, fixedFinder{{DocID: "n", Title: "n", Score: 5, Line: "q?"}}, agent.FinderOpts{}, nil, nil, tc.fetch, nil, nil, nil,
			func(n DocsNote) { notes = append(notes, n) })
		store.Append(ctx, "s", agent.Entry{Kind: agent.KindUser, Content: "q", CreatedAt: 1})
		prep.PrepareNotifications(ctx, "s")
		es := store.docsEntries()
		if len(notes) != 1 || notes[0].Docs[0].Inline != tc.inline || len(es) != 1 || !strings.Contains(es[0], tc.want) {
			t.Errorf("%s: notes=%+v entries=%q", tc.name, notes, es)
		}
	}
}

// Scores compare only within one index, so notes and documents each get their
// own top-1 + margin: a clear note and a clear document both surface, even
// when the document's raw score is higher (M2 live: 2.3e-5 vs 8.5e-6).
func TestDocsPreparer_MarginPerGroup(t *testing.T) {
	store, _ := openSessionStore("")
	ctx := context.Background()
	var notes []DocsNote
	finder := fixedFinder{
		{DocID: "/repo/suggest.go", Title: "suggest.go", Score: 2.3e-5},
		{DocID: "/mem/stash.md", Title: "stash", Score: 8.5e-6},
	}
	group := func(id string) string {
		if strings.HasPrefix(id, "/mem/") {
			return groupMemory
		}
		return ""
	}
	prep := docsPreparer(store, finder, agent.FinderOpts{}, nil, nil, nil, group, nil, nil,
		func(n DocsNote) { notes = append(notes, n) })
	store.Append(ctx, "s", agent.Entry{Kind: agent.KindUser, Content: "should I stash?", CreatedAt: 1})
	if err := prep.PrepareNotifications(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	if len(notes) != 1 || notes[0].Surfaced != 2 {
		t.Fatalf("want one pass surfacing the note AND the document, got %+v", notes)
	}
	es := store.docsEntries()
	if len(es) != 1 || !strings.Contains(es[0], "earlier session") || !strings.Contains(es[0], "stash") {
		t.Errorf("the note must surface, labelled as a saved note: %q", es)
	}
}

// A surfaced SKILL.md names the skill and how to load it, instead of
// quoting a fragment the model would act on without loading the skill; and
// skills rank in their own group, so a document does not crowd one out.
func TestDocsPreparer_SkillHitPointsAtTheSkill(t *testing.T) {
	store, _ := openSessionStore("")
	ctx := context.Background()
	var notes []DocsNote
	finder := fixedFinder{
		{DocID: "/repo/deploy.go", Title: "deploy.go", Score: 9, Line: "func deploy"},
		{DocID: "/s/release/SKILL.md", Title: "SKILL.md", Score: 1, Line: "1. tag the build"},
	}
	skill := func(id string) (skillHit, bool) {
		if id == "/s/release/SKILL.md" {
			return skillHit{Skill: Skill{Name: "release", Description: "cut a release"}}, true
		}
		return skillHit{}, false
	}
	fetched := false
	fetch := func(_ context.Context, id string, _ int) (string, bool) {
		fetched = fetched || id == "/s/release/SKILL.md"
		return "", false
	}
	prep := docsPreparer(store, finder, agent.FinderOpts{}, nil, nil, fetch, docGroup(nil, skill, nil), skill, nil,
		func(n DocsNote) { notes = append(notes, n) })
	store.Append(ctx, "s", agent.Entry{Kind: agent.KindUser, Content: "cut a release", CreatedAt: 1})
	if err := prep.PrepareNotifications(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	if len(notes) != 1 || notes[0].Surfaced != 2 {
		t.Fatalf("want the document AND the skill surfaced, got %+v", notes)
	}
	es := store.docsEntries()
	if len(es) != 1 || !strings.Contains(es[0], `call skill name="release"`) || strings.Contains(es[0], "tag the build") {
		t.Errorf("the skill must surface as a load cue, not a fragment: %q", es)
	}
	if fetched {
		t.Error("a skill's body must not be inlined")
	}
	if notes[0].Docs[1].Title != "skill release" {
		t.Errorf("the UI row should name the skill: %+v", notes[0].Docs)
	}
}

// A hit on a skill's reference file is the same load cue, and names the
// file to read.
func TestDocsPreparer_SkillReferenceHit(t *testing.T) {
	store, _ := openSessionStore("")
	ctx := context.Background()
	finder := fixedFinder{{DocID: "/s/release/reference/tags.md", Title: "tags.md", Score: 3, Line: "tag v1"}}
	skill := func(id string) (skillHit, bool) {
		if id == "/s/release/reference/tags.md" {
			return skillHit{Skill: Skill{Name: "release", Description: "cut a release"}, File: "reference/tags.md"}, true
		}
		return skillHit{}, false
	}
	prep := docsPreparer(store, finder, agent.FinderOpts{}, nil, nil, nil, docGroup(nil, skill, nil), skill, nil, nil)
	store.Append(ctx, "s", agent.Entry{Kind: agent.KindUser, Content: "how do I tag", CreatedAt: 1})
	if err := prep.PrepareNotifications(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	es := store.docsEntries()
	if len(es) != 1 || !strings.Contains(es[0], `call skill name="release"`) || !strings.Contains(es[0], `file="reference/tags.md"`) || strings.Contains(es[0], "tag v1") {
		t.Errorf("cue: %q", es)
	}
}

// Two files of one skill are one candidate: the margin is between skills.
func TestDocsPreparer_OneCandidatePerSkill(t *testing.T) {
	store, _ := openSessionStore("")
	ctx := context.Background()
	finder := fixedFinder{
		{DocID: "/s/a/ref.md", Title: "ref.md", Score: 3},
		{DocID: "/s/a/SKILL.md", Title: "SKILL.md", Score: 2.9},
		{DocID: "/s/b/SKILL.md", Title: "SKILL.md", Score: 1},
	}
	skill := func(id string) (skillHit, bool) {
		name := filepath.Base(filepath.Dir(id))
		return skillHit{Skill: Skill{Name: name, Description: "d"}}, true
	}
	prep := docsPreparer(store, finder, agent.FinderOpts{}, nil, nil, nil, docGroup(nil, skill, nil), skill, nil, nil)
	store.Append(ctx, "s", agent.Entry{Kind: agent.KindUser, Content: "x", CreatedAt: 1})
	if err := prep.PrepareNotifications(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	if es := store.docsEntries(); len(es) != 1 || !strings.Contains(es[0], `the runner-up (of 2)`) || !strings.Contains(es[0], `name="a"`) {
		t.Errorf("cue: %q", es)
	}
}

// An imported rules file in the rules index is its own group, framed as such.
func TestDocsPreparer_RulesHit(t *testing.T) {
	store, _ := openSessionStore("")
	ctx := context.Background()
	finder := fixedFinder{{DocID: "/ws/docs/style.md", Title: "style.md", Score: 3, Line: "tabs"}}
	rules := func(id string) bool { return id == "/ws/docs/style.md" }
	prep := docsPreparer(store, finder, agent.FinderOpts{}, nil, nil, nil, docGroup(nil, nil, rules), nil, nil, nil)
	store.Append(ctx, "s", agent.Entry{Kind: agent.KindUser, Content: "indent style", CreatedAt: 1})
	if err := prep.PrepareNotifications(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	if es := store.docsEntries(); len(es) != 1 || !strings.Contains(es[0], "the project's rules import") {
		t.Errorf("framing: %q", es)
	}
}

// Why says whether a margin was tested at all, and what the rule read: the
// similarity, the query words held and the lead over the runner-up. A hit
// from an older raglit reads at BM25's small scale (%.2f printed 0.00).
func TestDocHitInfo_Why(t *testing.T) {
	for _, tc := range []struct {
		d    DocHitInfo
		want string
	}{
		{DocHitInfo{Score: 1.234e-5, Candidates: 1}, "only candidate · score 1.2e-05"},
		{DocHitInfo{Score: 3e-5, RunnerUp: 1e-5, Candidates: 4}, "3.0× the runner-up (of 4) · score 3e-05"},
		{DocHitInfo{Score: 0.5}, "score 0.5"}, // an older engine: no candidates known
		{DocHitInfo{Score: 0.03, Candidates: 4, Similarity: 0.712, Matched: 3, QueryTerms: 4, RunnerUpSim: 0.64},
			"sim 0.71 · 3/4 terms · +0.07 over runner-up (of 4)"},
		{DocHitInfo{Score: 0.03, Candidates: 1, Similarity: 0.66, Matched: 2, QueryTerms: 3}, "only candidate · sim 0.66 · 2/3 terms"},
		{DocHitInfo{Score: 0.03, Candidates: 2, Similarity: 0.7, Matched: 0, QueryTerms: 3}, "sim 0.70 · 0/3 terms · runner-up has no sim (of 2)"},
		{DocHitInfo{Score: 2e-5, Candidates: 1, Matched: 2, QueryTerms: 3}, "only candidate · 2/3 terms"}, // no embeddings
		{DocHitInfo{Score: 2e-5, Candidates: 3, Matched: 2, QueryTerms: 3}, "2/3 terms · top of 3"},
	} {
		if got := tc.d.Why(); got != tc.want {
			t.Errorf("Why(%+v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

// Every surfaced item says where it came from — kind, file, index and, for
// a skill, its root — to the UI and in the model's text; a lone candidate
// is marked as one.
func TestDocsPreparer_SourceLabels(t *testing.T) {
	store, _ := openSessionStore("")
	ctx := context.Background()
	var notes []DocsNote
	home := "/home/u"
	skillDoc := home + "/.claude/skills/synced/b1/docs/SKILL.md"
	finder := fixedFinder{
		{DocID: skillDoc, Title: "SKILL.md", Score: 1.2e-5},
		{DocID: home + "/.dun/memory/stash.md", Title: "stash", Score: 4e-5},
		{DocID: home + "/.dun/memory/other.md", Title: "other", Score: 1e-5},
		{DocID: "/ws/docs/guide.md", Title: "guide", Score: 2, Line: "the guide"},
		{DocID: "/ws/docs/old.md", Title: "old", Score: 0.5},
	}
	skill := func(id string) (skillHit, bool) {
		if id == skillDoc {
			return skillHit{Skill: Skill{Name: "docs", Description: "editable docs",
				Root: skillRootLabel(filepath.Dir(filepath.Dir(skillDoc)), "user")}}, true
		}
		return skillHit{}, false
	}
	group := func(id string) string {
		switch {
		case strings.Contains(id, "/memory/"):
			return groupMemory
		case id == skillDoc:
			return groupSkill
		}
		return ""
	}
	indexes := map[string]string{skillDoc: "skills", home + "/.dun/memory/stash.md": "memory", "/ws/docs/guide.md": "proja:handbook"}
	src := &hitSources{
		index:    func(id string) string { return indexes[id] },
		attached: func(ix string) bool { return ix == "proja:handbook" },
		display:  func(p string) string { return displayPath(p, home, "/ws") },
	}
	prep := docsPreparer(store, finder, agent.FinderOpts{}, nil, nil, nil, group, skill, src,
		func(n DocsNote) { notes = append(notes, n) })
	store.Append(ctx, "s", agent.Entry{Kind: agent.KindUser, Content: "x", CreatedAt: 1})
	if err := prep.PrepareNotifications(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	if len(notes) != 1 || len(notes[0].Docs) != 3 {
		t.Fatalf("want three surfaced, got %+v", notes)
	}
	want := map[string]string{
		"skill docs": "skill · ~/.claude/skills/synced/b1/docs/SKILL.md · index skills · claude.ai sync (synced/b1)",
		"stash":      "memory · ~/.dun/memory/stash.md · index memory",
		"guide":      "attached doc · docs/guide.md · index proja:handbook",
	}
	whys := map[string]string{
		"skill docs": "only candidate · score 1.2e-05",
		"stash":      "4.0× the runner-up (of 2) · score 4e-05",
		"guide":      "4.0× the runner-up (of 2) · score 2",
	}
	es := store.docsEntries()
	for _, d := range notes[0].Docs {
		if d.Source() != want[d.Title] || d.Why() != whys[d.Title] {
			t.Errorf("%s: source %q why %q", d.Title, d.Source(), d.Why())
		}
		if len(es) != 1 || !strings.Contains(es[0], "Source: "+want[d.Title]) || !strings.Contains(es[0], whys[d.Title]) {
			t.Errorf("the model's text must carry %q's source and why: %q", d.Title, es)
		}
	}
}

func TestSkillRootLabel(t *testing.T) {
	home := t.TempDir()
	defer func(f func() (string, error)) { userHomeDir = f }(userHomeDir)
	userHomeDir = func() (string, error) { return home, nil }
	dunSkills := filepath.Join(filepath.Dir(SessionsDir()), "skills")
	for _, tc := range []struct{ dir, source, want string }{
		{filepath.Join(home, ".claude", "skills"), "user", "user ~/.claude/skills"},
		{filepath.Join(home, ".claude", "skills", "synced", "b1"), "user", "claude.ai sync (synced/b1)"},
		{dunSkills, "user", "dun user skills"},
		{"/p/.claude/skills", "project", "project .claude/skills"},
		{"/p/.dun/skills", "project", "project .dun/skills"},
		{"/x/skills", "extension raglit", "extension raglit"},
	} {
		if got := skillRootLabel(tc.dir, tc.source); got != tc.want {
			t.Errorf("skillRootLabel(%q, %q) = %q, want %q", tc.dir, tc.source, got, tc.want)
		}
	}
}

func TestDisplayPath(t *testing.T) {
	for _, tc := range []struct{ p, want string }{
		{"/ws/a/b.md", "a/b.md"},
		{"/home/u/.dun/memory/n.md", "~/.dun/memory/n.md"},
		{"/ws2/x.md", "/ws2/x.md"},
		{"/ws", "/ws"},
		{"skill://x/SKILL.md", "skill://x/SKILL.md"},
	} {
		if got := displayPath(tc.p, "/home/u", "", "/ws"); got != tc.want {
			t.Errorf("displayPath(%q) = %q, want %q", tc.p, got, tc.want)
		}
	}
}

// raglit tags each hit with its index; the first (best) hit of a document
// names it.
func TestHitIndexes_Note(t *testing.T) {
	var x hitIndexes
	x.note(`{"hits":[{"index":"skills","doc_id":"/a","score":2},{"index":"default","doc_id":"/a","score":1},{"index":"memory","doc_id":"/b"}]}`)
	x.note(`[{"index":"proja:handbook","id":"/c"}]`)
	for id, want := range map[string]string{"/a": "skills", "/b": "memory", "/c": "proja:handbook", "/d": ""} {
		if got := x.get(id); got != want {
			t.Errorf("index of %s = %q, want %q", id, got, want)
		}
	}
	s := &surfacer{sel: map[string]string{"raglit": "projb:*,proja:handbook"}}
	if !s.attached("proja:handbook") || s.attached("projb:*") || s.attached("handbook") {
		t.Error("attached: only the named indexes after the own wildcard")
	}
}

// raglit's measures ride the raw hits: a document keeps its fragments' best
// similarity and term count, and a hit with no query_terms (a raglit
// before e66a3df) has no measures.
func TestHitIndexes_NoteMeasures(t *testing.T) {
	var x hitIndexes
	got := x.note(`{"hits":[
		{"index":"main","doc_id":"/a","score":0.03,"similarity":0.61,"matched_terms":["x"],"query_terms":3},
		{"index":"main","doc_id":"/a","score":0.02,"similarity":0.72,"matched_terms":["x","y"],"query_terms":3},
		{"index":"skills","doc_id":"/b","score":0.01,"matched_terms":[],"query_terms":3},
		{"index":"old","doc_id":"/c","score":1e-5}]}`)
	if strings.Join(got, ",") != "main,skills,old" {
		t.Errorf("indexes named: %v", got)
	}
	if m, ok := x.metaOf("/a"); !ok || !m.HasSim || m.Sim != 0.72 || m.Matched != 2 || m.Terms != 3 {
		t.Errorf("/a: %+v %v", m, ok)
	}
	if m, ok := x.metaOf("/b"); !ok || m.HasSim || m.Matched != 0 || m.Terms != 3 {
		t.Errorf("/b (no similarity): %+v %v", m, ok)
	}
	if _, ok := x.metaOf("/c"); ok {
		t.Error("/c is from an older raglit: no measures")
	}
}

// metaSources serves raglit's measures to docsPreparer: a document's index
// is its first path element ("/main/a" → "main"); meaning says which
// indexes have embeddings (absent: the list does not name it).
func metaSources(meta map[string]hitMeta, meaning map[string]bool) *hitSources {
	index := func(id string) string { return strings.Split(strings.TrimPrefix(id, "/"), "/")[0] }
	return &hitSources{
		index: index,
		meta:  func(id string) (hitMeta, bool) { m, ok := meta[id]; return m, ok },
		meaning: func(id string) (bool, bool) {
			m, ok := meaning[index(id)]
			return m, ok
		},
	}
}

// surfaceWith runs one pass over hits and returns what surfaced.
func surfaceWith(t *testing.T, hits fixedFinder, src *hitSources) []DocHitInfo {
	t.Helper()
	store, _ := openSessionStore("")
	ctx := context.Background()
	var notes []DocsNote
	prep := docsPreparer(store, hits, agent.FinderOpts{}, nil, nil, nil, nil, nil, src,
		func(n DocsNote) { notes = append(notes, n) })
	store.Append(ctx, "s", agent.Entry{Kind: agent.KindUser, Content: "q", CreatedAt: 1})
	if err := prep.PrepareNotifications(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	if len(notes) == 0 {
		return nil
	}
	es := store.docsEntries()
	for _, d := range notes[0].Docs {
		if len(es) != 1 || !strings.Contains(es[0], d.Why()) {
			t.Errorf("the model's text must carry the why %q: %q", d.Why(), es)
		}
	}
	return notes[0].Docs
}

// An index with embeddings: a lone candidate needs the floor, nothing more.
func TestDocsPreparer_SimilarityFloor(t *testing.T) {
	means := map[string]bool{"main": true}
	hits := fixedFinder{{DocID: "/main/a", Title: "a", Score: 0.03}}
	below := metaSources(map[string]hitMeta{"/main/a": {Sim: surfaceFloor - 0.01, HasSim: true, Matched: 3, Terms: 3}}, means)
	if got := surfaceWith(t, hits, below); got != nil {
		t.Errorf("a lone candidate under the floor surfaced: %+v", got)
	}
	sim := surfaceFloor + 0.01
	above := metaSources(map[string]hitMeta{"/main/a": {Sim: sim, HasSim: true, Matched: 2, Terms: 3}}, means)
	got := surfaceWith(t, hits, above)
	if want := fmt.Sprintf("only candidate · sim %.2f · 2/3 terms", sim); len(got) != 1 || got[0].Why() != want {
		t.Errorf("a lone candidate over the floor: %+v, want why %q", got, want)
	}
}

// Two candidates: the top needs the floor AND surfaceDelta over the
// runner-up's similarity. The order is by similarity, not by score (a
// hybrid score is a rank fusion).
func TestDocsPreparer_SimilarityDelta(t *testing.T) {
	means := map[string]bool{"main": true}
	top := surfaceFloor + 0.05
	hits := fixedFinder{{DocID: "/main/b", Title: "b", Score: 0.033}, {DocID: "/main/a", Title: "a", Score: 0.03}}
	near := metaSources(map[string]hitMeta{
		"/main/a": {Sim: top, HasSim: true, Matched: 3, Terms: 4},
		"/main/b": {Sim: top - surfaceDelta/2, HasSim: true, Matched: 4, Terms: 4},
	}, means)
	if got := surfaceWith(t, hits, near); got != nil {
		t.Errorf("a lead under the delta surfaced: %+v", got)
	}
	runner := top - surfaceDelta - 0.02
	clear := metaSources(map[string]hitMeta{
		"/main/a": {Sim: top, HasSim: true, Matched: 3, Terms: 4},
		"/main/b": {Sim: runner, HasSim: true, Matched: 4, Terms: 4},
	}, means)
	got := surfaceWith(t, hits, clear)
	want := fmt.Sprintf("sim %.2f · 3/4 terms · +%.2f over runner-up (of 2)", top, top-runner)
	if len(got) != 1 || got[0].DocID != "/main/a" || got[0].Why() != want || got[0].RunnerUpSim != runner {
		t.Errorf("the higher similarity wins by the delta: %+v, want why %q", got, want)
	}
}

// A hit the vector side did not rank has no similarity: it never surfaces
// from an index with embeddings, however many words it holds; and as a
// runner-up it sets no bar.
func TestDocsPreparer_NoSimilarityNeverSurfaces(t *testing.T) {
	means := map[string]bool{"main": true}
	lone := fixedFinder{{DocID: "/main/a", Title: "a", Score: 0.03}}
	if got := surfaceWith(t, lone, metaSources(map[string]hitMeta{"/main/a": {Matched: 4, Terms: 4}}, means)); got != nil {
		t.Errorf("a hit with no similarity surfaced: %+v", got)
	}
	two := fixedFinder{{DocID: "/main/b", Title: "b", Score: 0.033}, {DocID: "/main/a", Title: "a", Score: 0.03}}
	got := surfaceWith(t, two, metaSources(map[string]hitMeta{
		"/main/a": {Sim: surfaceFloor, HasSim: true, Matched: 1, Terms: 4},
		"/main/b": {Matched: 4, Terms: 4},
	}, means))
	if len(got) != 1 || got[0].DocID != "/main/a" || !strings.Contains(got[0].Why(), "runner-up has no sim (of 2)") {
		t.Errorf("a runner-up with no similarity: %+v", got)
	}
}

// An index without embeddings has no similarity: the top hit (by score)
// needs coverageFloor of the query's words, and the score ratio over a
// runner-up. An index the list does not name is judged by whether any of
// its hits has a similarity.
func TestDocsPreparer_CoverageWithoutEmbeddings(t *testing.T) {
	means := map[string]bool{"plain": false}
	hits := fixedFinder{{DocID: "/plain/a", Title: "a", Score: 3e-5}, {DocID: "/plain/b", Title: "b", Score: 1.5e-5}}
	if got := surfaceWith(t, hits, metaSources(map[string]hitMeta{
		"/plain/a": {Matched: 1, Terms: 3}, "/plain/b": {Matched: 3, Terms: 3}}, means)); got != nil {
		t.Errorf("the top hit holds 1 of 3 words: %+v", got)
	}
	cover := metaSources(map[string]hitMeta{"/plain/a": {Matched: 2, Terms: 4}, "/plain/b": {Matched: 1, Terms: 4}}, means)
	got := surfaceWith(t, hits, cover)
	if len(got) != 1 || got[0].DocID != "/plain/a" || got[0].Why() != "2/4 terms · 2.0× the runner-up (of 2)" {
		t.Errorf("2 of 4 words is the floor: %+v", got)
	}
	near := fixedFinder{{DocID: "/plain/a", Title: "a", Score: 2e-5}, {DocID: "/plain/b", Title: "b", Score: 1.9e-5}}
	if got := surfaceWith(t, near, cover); got != nil {
		t.Errorf("enough words but no margin over the runner-up: %+v", got)
	}

	// Not in the list: no similarity on any of its hits means no embeddings.
	lone := fixedFinder{{DocID: "/other/a", Title: "a", Score: 2e-5}}
	got = surfaceWith(t, lone, metaSources(map[string]hitMeta{"/other/a": {Matched: 3, Terms: 4}}, nil))
	if len(got) != 1 || got[0].Why() != "only candidate · 3/4 terms" {
		t.Errorf("an unlisted index with no similarity: coverage: %+v", got)
	}
	// ... and a similarity on one says it has them.
	two := fixedFinder{{DocID: "/other/a", Title: "a", Score: 0.03}, {DocID: "/other/b", Title: "b", Score: 0.02}}
	if got := surfaceWith(t, two, metaSources(map[string]hitMeta{
		"/other/a": {Matched: 4, Terms: 4}, "/other/b": {Sim: surfaceFloor - 0.1, HasSim: true, Matched: 1, Terms: 4}}, nil)); got != nil {
		t.Errorf("an unlisted index with a similarity: the similarity rule: %+v", got)
	}
}

// The query is the newest user message, its first surfaceQueryWords words;
// a later pass in the turn (no new user message) searches the newest entry.
func TestDocsPreparer_QueryIsTheNewestMessageCapped(t *testing.T) {
	store, _ := openSessionStore("")
	ctx := context.Background()
	var queries []string
	finder := textsFinder(func(texts []string) { queries = append(queries, strings.Join(texts, "|")) })
	prep := docsPreparer(store, finder, agent.FinderOpts{}, nil, nil, nil, nil, nil, nil, nil)

	var long []string
	for i := 0; i < surfaceQueryWords+30; i++ {
		long = append(long, fmt.Sprintf("w%d", i))
	}
	store.Append(ctx, "s", agent.Entry{Kind: agent.KindAssistant, Content: "the reply to the last message", CreatedAt: 1})
	store.Append(ctx, "s", agent.Entry{Kind: agent.KindUser, Content: strings.Join(long, "  \n"), CreatedAt: 2})
	prep.PrepareNotifications(ctx, "s")
	store.Append(ctx, "s", agent.Entry{Kind: agent.KindAssistant, Content: "now reading notify.go", CreatedAt: 3})
	store.Append(ctx, "s", agent.Entry{Kind: agent.KindAssistant, Content: "", CreatedAt: 4})
	prep.PrepareNotifications(ctx, "s")
	want := []string{strings.Join(long[:surfaceQueryWords], " "), "now reading notify.go"}
	if len(queries) != 2 || queries[0] != want[0] || queries[1] != want[1] {
		t.Errorf("queries = %q, want %q", queries, want)
	}
}

type textsFinder func([]string)

func (f textsFinder) Find(_ context.Context, texts []string) ([]agent.DocHit, error) {
	f(texts)
	return nil, nil
}
