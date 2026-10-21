package dun

import (
	"context"
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
	prep := docsPreparer(store, finder, agent.FinderOpts{MaxHits: 2}, stats, estimate,
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

// Two fragments of one document fill one slot, not two.
func TestDocsPreparer_OneSlotPerDocument(t *testing.T) {
	store, _ := openSessionStore("")
	ctx := context.Background()
	var notes []DocsNote
	finder := fixedFinder{
		{DocID: "a", Title: "a (caption)", Score: 9},
		{DocID: "a", Title: "a (text)", Score: 8},
		{DocID: "b", Title: "b", Score: 3},
	}
	prep := docsPreparer(store, finder, agent.FinderOpts{MaxHits: 2}, nil, nil,
		func(n DocsNote) { notes = append(notes, n) })
	store.Append(ctx, "s", agent.Entry{Kind: agent.KindUser, Content: "q", CreatedAt: 1})
	if err := prep.PrepareNotifications(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	if len(notes) != 1 || len(notes[0].Docs) != 2 || notes[0].Docs[0].DocID != "a" || notes[0].Docs[1].DocID != "b" {
		t.Fatalf("want a then b, one slot each: %+v", notes)
	}
	if notes[0].Docs[0].Title != "a (caption)" {
		t.Errorf("the best fragment of a document is the one kept: %q", notes[0].Docs[0].Title)
	}
	if notes[0].Found != 2 {
		t.Errorf("found counts documents: %d", notes[0].Found)
	}
}
