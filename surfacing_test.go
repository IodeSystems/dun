package dun

import (
	"context"
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
	prep := docsPreparer(store, finder, agent.FinderOpts{}, stats, estimate, nil,
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
	prep := docsPreparer(store, finder, agent.FinderOpts{}, nil, nil, nil,
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
	prep := docsPreparer(store, finderFunc(func() []agent.DocHit { return *f }), agent.FinderOpts{}, stats, nil, nil,
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
		prep := docsPreparer(store, fixedFinder{{DocID: "n", Title: "n", Score: 5, Line: "q?"}}, agent.FinderOpts{}, nil, nil, tc.fetch,
			func(n DocsNote) { notes = append(notes, n) })
		store.Append(ctx, "s", agent.Entry{Kind: agent.KindUser, Content: "q", CreatedAt: 1})
		prep.PrepareNotifications(ctx, "s")
		es := store.docsEntries()
		if len(notes) != 1 || notes[0].Docs[0].Inline != tc.inline || len(es) != 1 || !strings.Contains(es[0], tc.want) {
			t.Errorf("%s: notes=%+v entries=%q", tc.name, notes, es)
		}
	}
}
