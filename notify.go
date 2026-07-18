package dun

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/iodesystems/agentkit/agent"
	"github.com/iodesystems/agentkit/mcpmgr"
	"github.com/iodesystems/agentkit/ragnotify"
)

// Proactive notifications — wiring raglit's search tool as an agent.DocFinder so
// dun can ping relevant docs before each turn (the ragnotify pattern, over an
// MCP server instead of a Go call). The finder reuses the SAME raglit server the
// model searches explicitly.
//
// Unlike agentkit's stock FinderPreparer (one notification PER hit), dun uses an
// AGGREGATING preparer: each pass emits ONE summary — "N found, M surfaced" —
// with the surfaced docs listed. "Surfaced" = the top-MaxHits actually injected
// into the prompt (what the model is given to read); "found" = all candidate
// hits after threshold/dedup. The UI collapses this to one line, expandable to
// the doc list (see the TUI's docsBlock).

// docsFinder returns a DocFinder over the discovered raglit `search` tool, or nil
// if raglit isn't in the tool set.
func docsFinder(mgr *mcpmgr.Manager, tools []mcpmgr.MCPTool) agent.DocFinder {
	for _, t := range tools {
		if t.Name == "search" {
			return ragnotify.MCPFinder(mgr, t.ServerID, "search", ragnotify.Opts{})
		}
	}
	return nil
}

// DocHitInfo is one surfaced document, for the UI.
type DocHitInfo struct {
	Title string
	DocID string
	Line  string
	Score float64
}

// DocsNote is the aggregated proactive-RAG summary for one pass.
type DocsNote struct {
	Found    int          // candidate hits after threshold/dedup
	Surfaced int          // top-MaxHits actually injected into the prompt
	Docs     []DocHitInfo // the surfaced docs
}

// docsPreparer is dun's aggregating NotificationPreparer. It mirrors agentkit's
// FinderPreparer watermark/dedup/threshold logic, but instead of one notice per
// hit it appends ONE readable summary Entry (Tag "docs", so the store skips the
// plain onNotify — the UI is driven by onDocs with structure instead) and calls
// onDocs with the found/surfaced counts + surfaced docs.
func docsPreparer(store *sessionStore, finder agent.DocFinder, opts agent.FinderOpts, onDocs func(DocsNote)) agent.NotificationPreparer {
	maxHits := opts.MaxHits
	if maxHits <= 0 {
		maxHits = 3
	}
	kinds := opts.Kinds
	if len(kinds) == 0 {
		kinds = []agent.EntryKind{agent.KindUser, agent.KindAssistant}
	}
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = 3 * time.Second
	}
	observed := make(map[agent.EntryKind]bool, len(kinds))
	for _, k := range kinds {
		observed[k] = true
	}

	var mu sync.Mutex
	watermark := map[string]int64{}
	seen := map[string]map[string]bool{}

	return agent.PreparerFunc(func(ctx context.Context, sessionID string) error {
		entries, err := store.Context(ctx, sessionID)
		if err != nil {
			return err
		}

		mu.Lock()
		mark := watermark[sessionID]
		seenIDs := seen[sessionID]
		if seenIDs == nil {
			seenIDs = map[string]bool{}
			seen[sessionID] = seenIDs
		}
		mu.Unlock()

		var texts []string
		maxObserved := mark
		for _, e := range entries {
			if e.CreatedAt <= mark || !observed[e.Kind] {
				continue
			}
			if e.Content != "" {
				texts = append(texts, e.Content)
			}
			if e.CreatedAt > maxObserved {
				maxObserved = e.CreatedAt
			}
		}
		if len(texts) == 0 {
			return nil
		}

		fctx := ctx
		if timeout > 0 {
			var cancel context.CancelFunc
			fctx, cancel = context.WithTimeout(ctx, timeout)
			defer cancel()
		}
		hits, err := finder.Find(fctx, texts)
		if err != nil {
			return nil // fail-open: keep the watermark so these entries retry
		}

		kept := hits[:0:0]
		for _, h := range hits {
			if h.Score >= opts.MinScore && h.DocID != "" && !seenIDs[h.DocID] {
				kept = append(kept, h)
			}
		}
		sort.SliceStable(kept, func(i, j int) bool { return kept[i].Score > kept[j].Score })

		if len(kept) > 0 {
			surfaced := kept
			if len(surfaced) > maxHits {
				surfaced = surfaced[:maxHits]
			}
			// Model-facing content: a readable pointer list of the surfaced docs.
			var b strings.Builder
			fmt.Fprintf(&b, "%d possibly-relevant document(s); surfacing %d:\n", len(kept), len(surfaced))
			docs := make([]DocHitInfo, 0, len(surfaced))
			for _, h := range surfaced {
				fmt.Fprintf(&b, "- %q (id=%s, score=%.2f) — %s\n", h.Title, h.DocID, h.Score, h.Line)
				seenIDs[h.DocID] = true
				docs = append(docs, DocHitInfo{Title: h.Title, DocID: h.DocID, Line: h.Line, Score: h.Score})
			}
			b.WriteString("Call the search tool with an id to read a document if it helps.")
			if err := store.Append(ctx, sessionID, agent.Entry{
				ID: uuid.New().String(), Kind: agent.KindNotification, Tag: tagDocs,
				Content: b.String(), CreatedAt: time.Now().UnixNano(),
			}); err != nil {
				return err
			}
			if onDocs != nil {
				onDocs(DocsNote{Found: len(kept), Surfaced: len(surfaced), Docs: docs})
			}
		}

		mu.Lock()
		if maxObserved > watermark[sessionID] {
			watermark[sessionID] = maxObserved
		}
		mu.Unlock()
		return nil
	})
}
