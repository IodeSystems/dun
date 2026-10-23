package dun

import (
	"context"
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
	prep := docsPreparer(store, finder, agent.FinderOpts{}, stats, estimate, nil, nil, nil,
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
	prep := docsPreparer(store, finder, agent.FinderOpts{}, nil, nil, nil, nil, nil,
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
	prep := docsPreparer(store, finderFunc(func() []agent.DocHit { return *f }), agent.FinderOpts{}, stats, nil, nil, nil, nil,
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
		prep := docsPreparer(store, fixedFinder{{DocID: "n", Title: "n", Score: 5, Line: "q?"}}, agent.FinderOpts{}, nil, nil, tc.fetch, nil, nil,
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
	prep := docsPreparer(store, finder, agent.FinderOpts{}, nil, nil, nil, group, nil,
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
	prep := docsPreparer(store, finder, agent.FinderOpts{}, nil, nil, fetch, docGroup(nil, skill, nil), skill,
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
	prep := docsPreparer(store, finder, agent.FinderOpts{}, nil, nil, nil, docGroup(nil, skill, nil), skill, nil)
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
	prep := docsPreparer(store, finder, agent.FinderOpts{}, nil, nil, nil, docGroup(nil, skill, nil), skill, nil)
	store.Append(ctx, "s", agent.Entry{Kind: agent.KindUser, Content: "x", CreatedAt: 1})
	if err := prep.PrepareNotifications(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	if es := store.docsEntries(); len(es) != 1 || !strings.Contains(es[0], `(2 candidate skill(s)`) || !strings.Contains(es[0], `name="a"`) {
		t.Errorf("cue: %q", es)
	}
}

// An imported rules file in the rules index is its own group, framed as such.
func TestDocsPreparer_RulesHit(t *testing.T) {
	store, _ := openSessionStore("")
	ctx := context.Background()
	finder := fixedFinder{{DocID: "/ws/docs/style.md", Title: "style.md", Score: 3, Line: "tabs"}}
	rules := func(id string) bool { return id == "/ws/docs/style.md" }
	prep := docsPreparer(store, finder, agent.FinderOpts{}, nil, nil, nil, docGroup(nil, nil, rules), nil, nil)
	store.Append(ctx, "s", agent.Entry{Kind: agent.KindUser, Content: "indent style", CreatedAt: 1})
	if err := prep.PrepareNotifications(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	if es := store.docsEntries(); len(es) != 1 || !strings.Contains(es[0], "the project's rules import") {
		t.Errorf("framing: %q", es)
	}
}
